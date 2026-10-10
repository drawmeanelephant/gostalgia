package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// validFSName enforces the io/fs name contract on the raw backends:
// names arrive here already in fs form (no leading slash, no dot
// segments) and must stay that way. Backslashes are rejected on every
// platform: they are ordinary filename characters on Unix but path
// separators on Windows, and one portable contract is easier to reason
// about than two.
func validFSName(op, name string) error {
	if strings.ContainsRune(name, '\\') {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if !fs.ValidPath(name) {
		return &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	return nil
}

// HostFS is an FS backed by a directory on the host. All operations go
// through os.Root, so a name that escapes the backing directory — through
// ".." elements or symlinks — fails closed. Errors reference environment
// paths, never host paths.
type HostFS struct {
	root     *os.Root
	rootPath string

	saveStageHook   func(stageName string) error
	saveRenameHook  func(stageName, targetName string) error
	saveDirSyncHook func(dir string, syncErr error)
}

// NewHost opens dir as the backing directory for a HostFS.
func NewHost(dir string) (*HostFS, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("vfs: open host root %s: %w", dir, err)
	}
	return &HostFS{root: r, rootPath: dir}, nil
}

// RootPath returns the backing host directory.
func (h *HostFS) RootPath() string { return h.rootPath }

// Close releases the backing root handle.
func (h *HostFS) Close() error { return h.root.Close() }

// openChecked opens name and then verifies the opened handle — not the
// path — refers to a regular file or a directory. Checking after open
// is deliberate: a pre-open Lstat leaves a race in which the path is
// swapped for a special file, and openNonblock keeps the open itself
// from blocking on a FIFO waiting for a peer. FIFOs, sockets, and
// devices carry no document content and are refused.
func (h *HostFS) openChecked(op, name string, flag int, perm fs.FileMode) (*os.File, error) {
	f, err := h.root.OpenFile(name, flag|openNonblock, perm)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		_ = f.Close()
		return nil, &Error{
			Op:      op,
			Path:    "/" + name,
			Code:    ErrInvalid,
			Message: "special files cannot be opened",
		}
	}
	return f, nil
}

func (h *HostFS) Open(name string) (fs.File, error) {
	if err := validFSName("open", name); err != nil {
		return nil, err
	}
	return h.openChecked("open", name, os.O_RDONLY, 0)
}

func (h *HostFS) Stat(name string) (fs.FileInfo, error) {
	if err := validFSName("stat", name); err != nil {
		return nil, err
	}
	// Lstat first, and Open only what cannot block: os.Root.Open on a
	// FIFO (or other special file) waits until a writer opens the other
	// end, and Stat is a metadata call. The handle-based path exists for
	// Windows: there os.Root.Lstat disagrees with handle Stat for
	// directories (stale mtimes), and every consumer — fstest included
	// — compares entry metadata against the handle's Stat, so regular
	// files and directories still go through the handle. Special files
	// report Lstat, which is what ReadDir's entries report for them too.
	info, err := h.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return info, nil
	}
	f, err := h.root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

func (h *HostFS) ReadFile(name string) ([]byte, error) {
	if err := validFSName("read", name); err != nil {
		return nil, err
	}
	f, err := h.openChecked("read", name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (h *HostFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := validFSName("readdir", name); err != nil {
		return nil, err
	}
	f, err := h.root.OpenFile(name, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return entries, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	// Back each entry's Info with the same metadata that HostFS.Stat
	// (and Open+File.Stat) reports. On Windows, directory metadata from
	// an enumeration differs from a handle Stat; one source keeps
	// entry.Info() == Stat() true everywhere. Only regular files and
	// directories may be opened here: os.Root.Open on a FIFO blocks
	// until a writer arrives and would stall the whole listing (and
	// shutdown behind it), so special entries — symlinks, FIFOs,
	// sockets, devices — fall back to Lstat, which cannot block. Info
	// reports whatever the fallback returned, keeping the per-entry
	// invariant for them too.
	for i, e := range entries {
		child := e.Name()
		if name != "." {
			child = name + "/" + e.Name()
		}
		if !e.Type().IsRegular() && !e.IsDir() {
			if info, err := h.root.Lstat(child); err == nil {
				entries[i] = hostEntry{DirEntry: e, info: info}
			}
			continue
		}
		if cf, err := h.root.OpenFile(child, os.O_RDONLY|openNonblock, 0); err == nil {
			info, err := cf.Stat()
			cf.Close()
			if err == nil {
				entries[i] = hostEntry{DirEntry: e, info: info}
			}
		}
	}
	return entries, nil
}

// hostEntry overrides DirEntry.Info with a handle-backed FileInfo.
type hostEntry struct {
	fs.DirEntry
	info fs.FileInfo
}

func (he hostEntry) Info() (fs.FileInfo, error) { return he.info, nil }

func (h *HostFS) MkdirAll(name string) error {
	if err := validFSName("mkdir", name); err != nil {
		return err
	}
	if name == "." {
		return nil
	}
	return h.root.MkdirAll(name, 0o755)
}

func (h *HostFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	if err := validFSName("write", name); err != nil {
		return err
	}
	if dir := path.Dir(name); dir != "." {
		if err := h.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if perm == 0 {
		perm = 0o644
	}
	f, err := h.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|openNonblock, perm)
	if err != nil {
		return err
	}
	// Same post-open check as reads: without it, a path swapped for a
	// FIFO (or one created between name lookup and open) would write to
	// or block on a special file.
	if info, err := f.Stat(); err != nil {
		_ = f.Close()
		return err
	} else if !info.Mode().IsRegular() {
		_ = f.Close()
		return &Error{
			Op:      "write",
			Path:    "/" + name,
			Code:    ErrInvalid,
			Message: "path is not a regular file",
		}
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (h *HostFS) Remove(name string) error {
	if err := validFSName("remove", name); err != nil {
		return err
	}
	if name == "." {
		return &fs.PathError{Op: "remove", Path: name, Err: errors.New("cannot remove the root")}
	}
	return h.root.Remove(name)
}

func (h *HostFS) Rename(oldName, newName string) error {
	if err := validFSName("rename", oldName); err != nil {
		return err
	}
	if err := validFSName("rename", newName); err != nil {
		return err
	}
	if oldName == "." || newName == "." {
		return &fs.PathError{Op: "rename", Path: oldName, Err: errors.New("cannot rename the root")}
	}
	return h.root.Rename(oldName, newName)
}

func (h *HostFS) RemoveAll(name string) error {
	if err := validFSName("removeall", name); err != nil {
		return err
	}
	if name == "." {
		return &fs.PathError{Op: "removeall", Path: name, Err: errors.New("cannot remove the root")}
	}
	return h.root.RemoveAll(name)
}

var hostSaveSeq atomic.Uint64

// syncDir fsyncs a directory so that a rename into it is durable across
// a crash. It is best-effort: filesystems that cannot sync a directory
// handle (EINVAL on some Unix filesystems, and platforms like Windows
// where directory sync is unsupported) report an error that callers
// ignore — the rename has already succeeded and the write is done.
func (h *HostFS) syncDir(dir string) {
	d, err := h.root.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	err = d.Sync()
	if h.saveDirSyncHook != nil {
		h.saveDirSyncHook(dir, err)
	}
}

func (h *HostFS) SaveAtomic(name string, data []byte, perm fs.FileMode) error {
	if err := validFSName("save", name); err != nil {
		return err
	}
	if name == "." {
		return &fs.PathError{Op: "save", Path: name, Err: errors.New("cannot save to root")}
	}
	if int64(len(data)) > MaxDocumentSize {
		return &Error{
			Op:      "save",
			Path:    "/" + name,
			Code:    ErrTooLarge,
			Message: fmt.Sprintf("document size %d exceeds limit of %d bytes", len(data), MaxDocumentSize),
		}
	}
	// If destination exists and is a directory, refuse to overwrite with a file.
	if info, err := h.root.Lstat(name); err == nil && info.IsDir() {
		return &Error{
			Op:      "save",
			Path:    "/" + name,
			Code:    ErrIsDir,
			Message: "destination is a directory",
		}
	}

	dir := path.Dir(name)
	if dir != "." {
		if err := h.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if perm == 0 {
		perm = 0o644
	}

	// Staging file in the same directory guarantees it shares the filesystem mount.
	seq := hostSaveSeq.Add(1)
	stageName := fmt.Sprintf("%s.tmp.%d.%d", name, time.Now().UnixNano(), seq)
	f, err := h.root.OpenFile(stageName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}

	if h.saveStageHook != nil {
		if hookErr := h.saveStageHook(stageName); hookErr != nil {
			_ = f.Close()
			_ = h.root.Remove(stageName)
			return hookErr
		}
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = h.root.Remove(stageName)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = h.root.Remove(stageName)
		return err
	}
	if err := f.Close(); err != nil {
		_ = h.root.Remove(stageName)
		return err
	}

	if h.saveRenameHook != nil {
		if hookErr := h.saveRenameHook(stageName, name); hookErr != nil {
			recoverName := fmt.Sprintf("%s.recover", name)
			if rErr := h.root.Rename(stageName, recoverName); rErr != nil {
				recoverName = stageName
			} else {
				h.syncDir(dir)
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

	if err := h.root.Rename(stageName, name); err != nil {
		// Preserve staged data as a recoverable artifact rather than silently losing it.
		recoverName := fmt.Sprintf("%s.recover", name)
		if rErr := h.root.Rename(stageName, recoverName); rErr != nil {
			recoverName = stageName
		} else {
			h.syncDir(dir)
		}
		return &Error{
			Op:          "save",
			Path:        "/" + name,
			RecoverPath: "/" + recoverName,
			Code:        ErrIO,
			Err:         err,
			Message:     fmt.Sprintf("atomic replacement failed: %v", err),
		}
	}
	h.syncDir(dir)
	return nil
}

// SetSaveHooks sets hooks for testing failure injection during atomic saves.
func (h *HostFS) SetSaveHooks(stageHook func(string) error, renameHook func(string, string) error) {
	h.saveStageHook = stageHook
	h.saveRenameHook = renameHook
}
