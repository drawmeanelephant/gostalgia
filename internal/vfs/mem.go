package vfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemFS is an in-memory FS: the environment's tmpfs at /tmp, and the test
// double for filesystem-backed subsystems. It follows the io/fs name
// contract strictly (fs-form names, no dot segments).
type MemFS struct {
	mu             sync.RWMutex
	nodes          map[string]*memNode // fs-style path -> node; "." is the root
	saveStageHook  func(stageName string) error
	saveRenameHook func(stageName, targetName string) error
}

type memNode struct {
	name    string // base name
	data    []byte
	mode    fs.FileMode
	modTime time.Time
}

func NewMem() *MemFS {
	m := &MemFS{nodes: map[string]*memNode{}}
	m.nodes["."] = &memNode{name: ".", mode: fs.ModeDir | 0o755, modTime: time.Now()}
	return m
}

func (m *MemFS) fail(op, name string, err error) error {
	return &fs.PathError{Op: op, Path: name, Err: err}
}

func (m *MemFS) parentOf(name string) string {
	if dir := path.Dir(name); dir != "." {
		return dir
	}
	return "."
}

func (m *MemFS) ensureDir(dir string) {
	if dir == "." {
		return
	}
	if _, ok := m.nodes[dir]; ok {
		return
	}
	m.ensureDir(m.parentOf(dir))
	m.nodes[dir] = &memNode{
		name:    path.Base(dir),
		mode:    fs.ModeDir | 0o755,
		modTime: time.Now(),
	}
}

func (m *MemFS) Open(name string) (fs.File, error) {
	if err := validFSName("open", name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[name]
	if !ok {
		return nil, m.fail("open", name, fs.ErrNotExist)
	}
	if n.dir() {
		return &memDir{fsys: m, node: n, path: name}, nil
	}
	return &memFile{node: n, reader: bytes.NewReader(n.data)}, nil
}

func (m *MemFS) Stat(name string) (fs.FileInfo, error) {
	if err := validFSName("stat", name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[name]
	if !ok {
		return nil, m.fail("stat", name, fs.ErrNotExist)
	}
	return memInfo{n}, nil
}

func (m *MemFS) ReadFile(name string) ([]byte, error) {
	if err := validFSName("read", name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[name]
	if !ok {
		return nil, m.fail("read", name, fs.ErrNotExist)
	}
	if n.dir() {
		return nil, m.fail("read", name, fs.ErrInvalid)
	}
	return append([]byte(nil), n.data...), nil
}

func (m *MemFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := validFSName("readdir", name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.nodes[name]
	if !ok {
		return nil, m.fail("readdir", name, fs.ErrNotExist)
	}
	if !n.dir() {
		return nil, m.fail("readdir", name, fs.ErrInvalid)
	}
	var out []fs.DirEntry
	for p, child := range m.nodes {
		if p == name || m.parentOf(p) != name {
			continue
		}
		out = append(out, memEntry{node: child, path: p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (m *MemFS) MkdirAll(name string) error {
	if err := validFSName("mkdir", name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[name]; ok && !n.dir() {
		return m.fail("mkdir", name, fs.ErrExist)
	}
	m.ensureDir(name)
	return nil
}

func (m *MemFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if err := validFSName("write", name); err != nil {
		return err
	}
	if perm == 0 {
		perm = 0o644
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if n, ok := m.nodes[name]; ok && n.dir() {
		return m.fail("write", name, fs.ErrInvalid)
	}
	m.ensureDir(m.parentOf(name))
	cp := append([]byte(nil), data...)
	m.nodes[name] = &memNode{name: path.Base(name), data: cp, mode: perm, modTime: time.Now()}
	return nil
}

func (m *MemFS) Remove(name string) error {
	if err := validFSName("remove", name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if name == "." {
		return m.fail("remove", name, fs.ErrInvalid)
	}
	n, ok := m.nodes[name]
	if !ok {
		return m.fail("remove", name, fs.ErrNotExist)
	}
	if n.dir() {
		for p := range m.nodes {
			if p != name && m.parentOf(p) == name {
				return m.fail("remove", name, fs.ErrInvalid)
			}
		}
	}
	delete(m.nodes, name)
	return nil
}

func (m *MemFS) Rename(oldName, newName string) error {
	if err := validFSName("rename", oldName); err != nil {
		return err
	}
	if err := validFSName("rename", newName); err != nil {
		return err
	}
	if oldName == "." || newName == "." {
		return m.fail("rename", oldName, fs.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	oldNode, ok := m.nodes[oldName]
	if !ok {
		return m.fail("rename", oldName, fs.ErrNotExist)
	}

	parent := m.parentOf(newName)
	parentNode, ok := m.nodes[parent]
	if !ok || !parentNode.dir() {
		return m.fail("rename", newName, fs.ErrNotExist)
	}

	if oldName == newName {
		return nil
	}

	if oldNode.dir() {
		if strings.HasPrefix(newName, oldName+"/") {
			return m.fail("rename", newName, fs.ErrInvalid)
		}
		if newNode, ok := m.nodes[newName]; ok {
			if !newNode.dir() {
				return m.fail("rename", newName, fs.ErrInvalid)
			}
			// Check if new directory is empty
			for p := range m.nodes {
				if p != newName && m.parentOf(p) == newName {
					return m.fail("rename", newName, fs.ErrExist)
				}
			}
		}
		// Rename all descendants
		prefix := oldName + "/"
		var toMove []string
		for p := range m.nodes {
			if strings.HasPrefix(p, prefix) {
				toMove = append(toMove, p)
			}
		}
		for _, p := range toMove {
			suffix := strings.TrimPrefix(p, prefix)
			newChild := newName + "/" + suffix
			node := m.nodes[p]
			delete(m.nodes, p)
			m.nodes[newChild] = node
		}
		oldNode.name = path.Base(newName)
		m.nodes[newName] = oldNode
		delete(m.nodes, oldName)
		return nil
	}

	// Regular file
	if newNode, ok := m.nodes[newName]; ok && newNode.dir() {
		return m.fail("rename", newName, fs.ErrInvalid)
	}
	oldNode.name = path.Base(newName)
	m.nodes[newName] = oldNode
	delete(m.nodes, oldName)
	return nil
}

func (m *MemFS) RemoveAll(name string) error {
	if err := validFSName("removeall", name); err != nil {
		return err
	}
	if name == "." {
		return m.fail("removeall", name, fs.ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.nodes[name]; !ok {
		return nil
	}

	prefix := name + "/"
	for p := range m.nodes {
		if strings.HasPrefix(p, prefix) {
			delete(m.nodes, p)
		}
	}
	delete(m.nodes, name)
	return nil
}

func (m *MemFS) SaveAtomic(name string, data []byte, perm fs.FileMode) error {
	if err := validFSName("save", name); err != nil {
		return err
	}
	if name == "." {
		return m.fail("save", name, fs.ErrInvalid)
	}
	if int64(len(data)) > MaxDocumentSize {
		return &Error{
			Op:      "save",
			Path:    "/" + name,
			Code:    ErrTooLarge,
			Message: fmt.Sprintf("document size %d exceeds limit of %d bytes", len(data), MaxDocumentSize),
		}
	}
	if perm == 0 {
		perm = 0o644
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if n, ok := m.nodes[name]; ok && n.dir() {
		recoverName := name + ".recover"
		// An existing artifact may be the only copy of an earlier failed
		// save; never clobber it (HostFS refuses before touching it).
		if _, exists := m.nodes[recoverName]; !exists {
			cp := append([]byte(nil), data...)
			m.nodes[recoverName] = &memNode{
				name:    path.Base(recoverName),
				data:    cp,
				mode:    perm,
				modTime: time.Now(),
			}
		}
		return &Error{
			Op:          "save",
			Path:        "/" + name,
			RecoverPath: "/" + recoverName,
			Code:        ErrIsDir,
			Message:     "destination is a directory",
		}
	}

	if m.saveStageHook != nil {
		if hookErr := m.saveStageHook(name); hookErr != nil {
			return hookErr
		}
	}

	if m.saveRenameHook != nil {
		if hookErr := m.saveRenameHook(name, name); hookErr != nil {
			recoverName := name + ".recover"
			cp := append([]byte(nil), data...)
			m.nodes[recoverName] = &memNode{
				name:    path.Base(recoverName),
				data:    cp,
				mode:    perm,
				modTime: time.Now(),
			}
			return &Error{
				Op:          "save",
				Path:        "/" + name,
				RecoverPath: "/" + recoverName,
				Code:        ErrIO,
				Err:         hookErr,
				Message:     fmt.Sprintf("atomic replacement failed: %v", hookErr),
			}
		}
	}

	m.ensureDir(m.parentOf(name))
	cp := append([]byte(nil), data...)
	m.nodes[name] = &memNode{
		name:    path.Base(name),
		data:    cp,
		mode:    perm,
		modTime: time.Now(),
	}
	return nil
}

func (m *MemFS) SetSaveHooks(stageHook func(string) error, renameHook func(string, string) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveStageHook = stageHook
	m.saveRenameHook = renameHook
}

func (n *memNode) dir() bool { return n.mode&fs.ModeDir != 0 }

// memInfo implements fs.FileInfo over a node.
type memInfo struct{ n *memNode }

func (i memInfo) Name() string       { return i.n.name }
func (i memInfo) Size() int64        { return int64(len(i.n.data)) }
func (i memInfo) Mode() fs.FileMode  { return i.n.mode }
func (i memInfo) ModTime() time.Time { return i.n.modTime }
func (i memInfo) IsDir() bool        { return i.n.dir() }
func (i memInfo) Sys() any           { return nil }

// memEntry implements fs.DirEntry.
type memEntry struct {
	node *memNode
	path string
}

func (e memEntry) Name() string { return e.node.name }
func (e memEntry) IsDir() bool  { return e.node.dir() }
func (e memEntry) Type() fs.FileMode {
	if e.node.dir() {
		return fs.ModeDir
	}
	return 0
}
func (e memEntry) Info() (fs.FileInfo, error) { return memInfo{e.node}, nil }

// memFile implements fs.File over a node's bytes.
type memFile struct {
	node   *memNode
	reader *bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) { return memInfo{f.node}, nil }
func (f *memFile) Read(b []byte) (int, error) { return f.reader.Read(b) }
func (f *memFile) Close() error               { return nil }

func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	return f.reader.Seek(offset, whence)
}

// memDir implements fs.ReadDirFile for directories, with standard cursor
// semantics: count <= 0 returns the remaining entries once, count > 0
// pages, and EOF is reported after the directory is drained.
type memDir struct {
	fsys *MemFS
	node *memNode
	path string

	all   []fs.DirEntry
	pos   int
	fetch bool
}

func (d *memDir) Stat() (fs.FileInfo, error) { return memInfo{d.node}, nil }
func (d *memDir) Close() error               { return nil }

func (d *memDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.path, Err: fs.ErrInvalid}
}

func (d *memDir) ReadDir(count int) ([]fs.DirEntry, error) {
	if !d.fetch {
		entries, err := d.fsys.ReadDir(d.path)
		if err != nil {
			return nil, err
		}
		d.all = entries
		d.fetch = true
	}
	if count <= 0 {
		if d.pos >= len(d.all) {
			return nil, nil
		}
		out := d.all[d.pos:]
		d.pos = len(d.all)
		return out, nil
	}
	if d.pos >= len(d.all) {
		return nil, io.EOF
	}
	end := min(d.pos+count, len(d.all))
	out := d.all[d.pos:end]
	d.pos = end
	return out, nil
}
