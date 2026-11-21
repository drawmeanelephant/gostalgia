package vfs

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTrashDir is the standard trash path in the environment drive.
const DefaultTrashDir = "/users/guest/.trash"

var trashSeq atomic.Uint64

// VFS is the environment filesystem: a root FS plus a mount table whose
// entries shadow subtrees of the root. Applications receive a *VFS and
// only ever speak environment paths.
type VFS struct {
	root     FS
	mu       sync.RWMutex
	mounts   map[string]FS // fs-style mount point ("tmp") -> fs
	trashDir string
	grants   *GrantStore
}

func New(root FS) *VFS {
	return &VFS{
		root:     root,
		mounts:   map[string]FS{},
		trashDir: DefaultTrashDir,
		grants:   NewGrantStore(),
	}
}

// Grants returns the grant store associated with this VFS.
func (v *VFS) Grants() *GrantStore {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.grants
}

// SetGrants configures the grant store used by this VFS.
func (v *VFS) SetGrants(gs *GrantStore) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.grants = gs
}

// CheckAccess checks whether appID is authorized to perform mode on envPath.
func (v *VFS) CheckAccess(appID, envPath string, mode AccessMode) error {
	if isShared, _, readOnly := v.SharedHostInfo(envPath); isShared {
		if mode == AccessReadWrite && readOnly {
			return &Error{Op: "access", Path: envPath, Code: ErrReadOnly, Message: "shared host mount is read-only"}
		}
		return nil
	}
	v.mu.RLock()
	gs := v.grants
	v.mu.RUnlock()
	if gs == nil {
		return nil
	}
	return gs.CheckAccess(appID, envPath, mode)
}

// ForApp returns an application-scoped view of this VFS.
func (v *VFS) ForApp(appID string) *ScopedVFS {
	return &ScopedVFS{
		vfs:   v,
		appID: appID,
	}
}

// Mount mounts f at path (environment form, e.g. "/tmp"), shadowing
// whatever the root has there.
func (v *VFS) Mount(path string, f FS) error {
	name, err := Normalize(path)
	if err != nil {
		return err
	}
	if name == "." {
		return errors.New("vfs: cannot mount over the root")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, exists := v.mounts[name]; exists {
		return fmt.Errorf("vfs: %s is already mounted", path)
	}
	v.mounts[name] = f
	return nil
}

// Unmount removes the mount at path. If the mounted filesystem implements
// a Close method, it is closed to release backing OS resources.
func (v *VFS) Unmount(path string) error {
	name, err := Normalize(path)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	fsys, ok := v.mounts[name]
	if !ok {
		return fmt.Errorf("vfs: nothing is mounted at %s", path)
	}
	delete(v.mounts, name)
	if closer, ok := fsys.(interface{ Close() error }); ok {
		_ = closer.Close()
	}
	return nil
}

// MountPoints lists active mount points in environment form.
func (v *VFS) MountPoints() []string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make([]string, 0, len(v.mounts))
	for mp := range v.mounts {
		out = append(out, "/"+mp)
	}
	sort.Strings(out)
	return out
}

// SharedHostMountInfo contains metadata for an active shared host folder mount.
type SharedHostMountInfo struct {
	MountPath string `json:"mount_path"`
	HostPath  string `json:"host_path"`
	ReadOnly  bool   `json:"read_only"`
}

// SharedHostMounts returns all active shared host folder mounts.
func (v *VFS) SharedHostMounts() []SharedHostMountInfo {
	v.mu.RLock()
	defer v.mu.RUnlock()
	var list []SharedHostMountInfo
	for mp, fsys := range v.mounts {
		if sh, ok := fsys.(*SharedHostFS); ok {
			list = append(list, SharedHostMountInfo{
				MountPath: "/" + mp,
				HostPath:  sh.RootPath(),
				ReadOnly:  sh.readOnly,
			})
		}
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].MountPath < list[j].MountPath
	})
	return list
}

// SharedHostInfo reports whether envPath falls within an active shared host folder mount.
func (v *VFS) SharedHostInfo(envPath string) (isSharedHost bool, hostPath string, readOnly bool) {
	name, err := Normalize(envPath)
	if err != nil {
		return false, "", false
	}
	fsys, _, _ := v.resolveMount(name)
	if sh, ok := fsys.(*SharedHostFS); ok {
		return true, sh.RootPath(), sh.readOnly
	}
	return false, "", false
}

// Root returns the root filesystem (used for status reporting only).
func (v *VFS) Root() FS { return v.root }

// TrashDir returns the configured environment trash directory.
func (v *VFS) TrashDir() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.trashDir
}

// SetTrashDir overrides the environment trash directory.
func (v *VFS) SetTrashDir(dir string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.trashDir = dir
}

func (v *VFS) isMountPoint(name string) bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	_, ok := v.mounts[name]
	return ok
}

func (v *VFS) resolve(name string) (FS, string) {
	fsys, sub, _ := v.resolveMount(name)
	return fsys, sub
}

func (v *VFS) resolveMount(name string) (FS, string, string) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if name != "." {
		var best string
		for mp := range v.mounts {
			if name == mp || strings.HasPrefix(name, mp+"/") {
				if len(mp) > len(best) {
					best = mp
				}
			}
		}
		if best != "" {
			if name == best {
				return v.mounts[best], ".", best
			}
			return v.mounts[best], strings.TrimPrefix(name, best+"/"), best
		}
	}
	return v.root, name, ""
}

func (v *VFS) Open(name string) (fs.File, error) {
	name, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	fsys, sub := v.resolve(name)
	return fsys.Open(sub)
}

func (v *VFS) Stat(name string) (fs.FileInfo, error) {
	name, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	fsys, sub := v.resolve(name)
	return fsys.Stat(sub)
}

func (v *VFS) ReadFile(name string) ([]byte, error) {
	name, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	fsys, sub := v.resolve(name)
	return fsys.ReadFile(sub)
}

func (v *VFS) ReadDir(name string) ([]fs.DirEntry, error) {
	name, err := Normalize(name)
	if err != nil {
		return nil, err
	}
	fsys, sub := v.resolve(name)
	return fsys.ReadDir(sub)
}

func (v *VFS) MkdirAll(name string) error {
	name, err := Normalize(name)
	if err != nil {
		return err
	}
	fsys, sub := v.resolve(name)
	return fsys.MkdirAll(sub)
}

func (v *VFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	name, err := Normalize(name)
	if err != nil {
		return err
	}
	if v.isMountPoint(name) {
		return &Error{Op: "write", Path: "/" + name, Code: ErrInvalid, Message: "cannot write over an active mount point"}
	}
	fsys, sub := v.resolve(name)
	return fsys.WriteFile(sub, data, perm)
}

func (v *VFS) Remove(name string) error {
	name, err := Normalize(name)
	if err != nil {
		return err
	}
	if name == "." {
		return &Error{Op: "remove", Path: "/", Code: ErrInvalid, Message: "cannot remove the root"}
	}
	if v.isMountPoint(name) {
		return &Error{Op: "remove", Path: "/" + name, Code: ErrInvalid, Message: "cannot remove an active mount point"}
	}
	fsys, sub := v.resolve(name)
	return fsys.Remove(sub)
}

func (v *VFS) RemoveAll(name string) error {
	norm, err := Normalize(name)
	if err != nil {
		return err
	}
	if norm == "." {
		return &Error{Op: "removeall", Path: "/", Code: ErrInvalid, Message: "cannot remove the root"}
	}
	if v.isMountPoint(norm) {
		return &Error{Op: "removeall", Path: "/" + norm, Code: ErrInvalid, Message: "cannot remove an active mount point"}
	}
	fsys, sub := v.resolve(norm)
	return fsys.RemoveAll(sub)
}

func (v *VFS) SaveAtomic(path string, data []byte, perm fs.FileMode) error {
	return v.SaveAtomicOpt(path, data, perm, true)
}

func (v *VFS) SaveAtomicOpt(path string, data []byte, perm fs.FileMode, overwrite bool) error {
	norm, err := Normalize(path)
	if err != nil {
		return err
	}
	if norm == "." {
		return &Error{Op: "save", Path: path, Code: ErrInvalid, Message: "cannot save to root"}
	}
	if v.isMountPoint(norm) {
		return &Error{Op: "save", Path: path, Code: ErrInvalid, Message: "cannot save over an active mount point"}
	}
	if !overwrite {
		if _, err := v.Stat(path); err == nil {
			return &Error{Op: "save", Path: path, Code: ErrExist, Message: "destination already exists"}
		}
	}
	fsys, sub := v.resolve(norm)
	return fsys.SaveAtomic(sub, data, perm)
}

// SaveAtomicBounded routes a bounded large save to the mounted backend.
// Overwrite semantics match SaveAtomicOpt with overwrite=true.
func (v *VFS) SaveAtomicBounded(path string, data []byte, perm fs.FileMode, limit int64) error {
	norm, err := Normalize(path)
	if err != nil {
		return err
	}
	if norm == "." {
		return &Error{Op: "save", Path: path, Code: ErrInvalid, Message: "cannot save to root"}
	}
	if v.isMountPoint(norm) {
		return &Error{Op: "save", Path: path, Code: ErrInvalid, Message: "cannot save over an active mount point"}
	}
	fsys, sub := v.resolve(norm)
	return SaveAtomicBounded(fsys, sub, data, perm, limit)
}

func (v *VFS) Rename(oldPath, newPath string) error {
	return v.RenameOpt(oldPath, newPath, false)
}

func (v *VFS) RenameOpt(oldPath, newPath string, overwrite bool) error {
	oldNorm, err := Normalize(oldPath)
	if err != nil {
		return err
	}
	newNorm, err := Normalize(newPath)
	if err != nil {
		return err
	}
	if oldNorm == "." || newNorm == "." {
		return &Error{Op: "rename", Path: oldPath, Dest: newPath, Code: ErrInvalid, Message: "cannot rename the root"}
	}
	if v.isMountPoint(oldNorm) || v.isMountPoint(newNorm) {
		return &Error{Op: "rename", Path: oldPath, Dest: newPath, Code: ErrInvalid, Message: "cannot rename an active mount point"}
	}
	if oldNorm == newNorm {
		return nil
	}

	fs1, sub1, mp1 := v.resolveMount(oldNorm)
	fs2, sub2, mp2 := v.resolveMount(newNorm)

	if fs1 != fs2 || mp1 != mp2 {
		return &Error{
			Op:      "rename",
			Path:    oldPath,
			Dest:    newPath,
			Code:    ErrCrossMount,
			Message: "cross-mount rename not supported; unsupported atomicity fails explicitly rather than weakening confinement",
		}
	}

	if !overwrite {
		if _, err := fs2.Stat(sub2); err == nil {
			return &Error{
				Op:      "rename",
				Path:    oldPath,
				Dest:    newPath,
				Code:    ErrExist,
				Message: "destination already exists",
			}
		}
	}

	return fs1.Rename(sub1, sub2)
}

func (v *VFS) Copy(src, dst string, overwrite bool) error {
	srcNorm, err := Normalize(src)
	if err != nil {
		return err
	}
	dstNorm, err := Normalize(dst)
	if err != nil {
		return err
	}
	if srcNorm == "." || dstNorm == "." {
		return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot copy to or from root"}
	}
	if v.isMountPoint(srcNorm) || v.isMountPoint(dstNorm) {
		return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot copy an active mount point"}
	}
	if srcNorm == dstNorm {
		return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrInvalid, Message: "source and destination are identical"}
	}

	srcInfo, err := v.Stat(src)
	if err != nil {
		return &Error{Op: "copy", Path: src, Code: ErrNotFound, Err: err, Message: "source does not exist"}
	}

	if !srcInfo.IsDir() {
		// File copy
		if dstInfo, err := v.Stat(dst); err == nil {
			if dstInfo.IsDir() {
				return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrIsDir, Message: "cannot overwrite directory with file"}
			}
			if !overwrite {
				return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrExist, Message: "destination already exists"}
			}
		}
		data, err := v.ReadFile(src)
		if err != nil {
			return err
		}
		return v.SaveAtomic(dst, data, srcInfo.Mode().Perm())
	}

	// Directory copy
	if strings.HasPrefix(dstNorm, srcNorm+"/") {
		return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot copy directory into itself"}
	}
	if dstInfo, err := v.Stat(dst); err == nil {
		if !dstInfo.IsDir() {
			return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrNotDir, Message: "destination exists and is not a directory"}
		}
		if !overwrite {
			entries, _ := v.ReadDir(dst)
			if len(entries) > 0 {
				return &Error{Op: "copy", Path: src, Dest: dst, Code: ErrExist, Message: "destination directory is not empty"}
			}
		}
	} else {
		if err := v.MkdirAll(dst); err != nil {
			return err
		}
	}

	entries, err := v.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		childSrc := src + "/" + e.Name()
		childDst := dst + "/" + e.Name()
		if err := v.Copy(childSrc, childDst, overwrite); err != nil {
			return err
		}
	}
	return nil
}

func (v *VFS) Move(src, dst string, overwrite bool) error {
	srcNorm, err := Normalize(src)
	if err != nil {
		return err
	}
	dstNorm, err := Normalize(dst)
	if err != nil {
		return err
	}
	if srcNorm == "." || dstNorm == "." {
		return &Error{Op: "move", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot move root"}
	}
	if v.isMountPoint(srcNorm) || v.isMountPoint(dstNorm) {
		return &Error{Op: "move", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot move an active mount point"}
	}
	if srcNorm == dstNorm {
		return nil
	}

	srcInfo, err := v.Stat(src)
	if err != nil {
		return &Error{Op: "move", Path: src, Code: ErrNotFound, Err: err, Message: "source does not exist"}
	}

	if srcInfo.IsDir() && strings.HasPrefix(dstNorm, srcNorm+"/") {
		return &Error{Op: "move", Path: src, Dest: dst, Code: ErrInvalid, Message: "cannot move directory into itself"}
	}

	if dstInfo, err := v.Stat(dst); err == nil {
		if !overwrite {
			return &Error{Op: "move", Path: src, Dest: dst, Code: ErrExist, Message: "destination already exists"}
		}
		if srcInfo.IsDir() && !dstInfo.IsDir() {
			return &Error{Op: "move", Path: src, Dest: dst, Code: ErrNotDir, Message: "cannot overwrite non-directory with directory"}
		}
		if !srcInfo.IsDir() && dstInfo.IsDir() {
			return &Error{Op: "move", Path: src, Dest: dst, Code: ErrIsDir, Message: "cannot overwrite directory with file"}
		}
	}

	fs1, sub1, mp1 := v.resolveMount(srcNorm)
	fs2, sub2, mp2 := v.resolveMount(dstNorm)

	if fs1 == fs2 && mp1 == mp2 {
		// Same mount: use atomic backend rename
		return fs1.Rename(sub1, sub2)
	}

	// Cross-mount move: copy and then remove source
	if err := v.Copy(src, dst, overwrite); err != nil {
		return err
	}
	return v.RemoveAll(src)
}

func (v *VFS) Trash(targetPath string) (*TrashEntry, error) {
	norm, err := Normalize(targetPath)
	if err != nil {
		return nil, err
	}
	if norm == "." {
		return nil, &Error{Op: "trash", Path: targetPath, Code: ErrInvalid, Message: "cannot trash the root"}
	}
	if v.isMountPoint(norm) {
		return nil, &Error{Op: "trash", Path: targetPath, Code: ErrInvalid, Message: "cannot trash an active mount point"}
	}

	trashDir := v.TrashDir()
	trashNorm, err := Normalize(trashDir)
	if err != nil {
		return nil, err
	}
	if norm == trashNorm || strings.HasPrefix(norm, trashNorm+"/") {
		return nil, &Error{Op: "trash", Path: targetPath, Code: ErrInvalid, Message: "cannot trash the trash directory"}
	}

	info, err := v.Stat(targetPath)
	if err != nil {
		return nil, &Error{Op: "trash", Path: targetPath, Code: ErrNotFound, Err: err, Message: "target does not exist"}
	}

	filesDir := trashDir + "/files"
	infoDir := trashDir + "/info"
	if err := v.MkdirAll(filesDir); err != nil {
		return nil, err
	}
	if err := v.MkdirAll(infoDir); err != nil {
		return nil, err
	}

	seq := trashSeq.Add(1)
	id := fmt.Sprintf("%d-%d", time.Now().UnixNano(), seq)
	baseName := path.Base(norm)
	itemDir := fmt.Sprintf("%s/%s", filesDir, id)
	trashFilePath := fmt.Sprintf("%s/%s", itemDir, baseName)

	if err := v.MkdirAll(itemDir); err != nil {
		return nil, err
	}

	if err := v.Move(targetPath, trashFilePath, true); err != nil {
		_ = v.RemoveAll(itemDir)
		return nil, err
	}

	entry := TrashEntry{
		ID:           id,
		OriginalPath: "/" + norm,
		TrashPath:    trashFilePath,
		Name:         baseName,
		IsDir:        info.IsDir(),
		Size:         info.Size(),
		TrashedAt:    time.Now().UTC(),
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		_ = v.Move(trashFilePath, targetPath, true)
		_ = v.RemoveAll(itemDir)
		return nil, err
	}

	metadataPath := fmt.Sprintf("%s/%s.json", infoDir, id)
	if err := v.SaveAtomic(metadataPath, append(data, '\n'), 0o644); err != nil {
		_ = v.Move(trashFilePath, targetPath, true)
		_ = v.RemoveAll(itemDir)
		return nil, err
	}

	return &entry, nil
}

func (v *VFS) Restore(trashID, dst string, overwrite bool) (string, error) {
	if strings.ContainsRune(trashID, '/') || strings.ContainsRune(trashID, '\\') || trashID == "" || trashID == "." || trashID == ".." {
		return "", &Error{Op: "restore", Path: trashID, Code: ErrInvalid, Message: "invalid trash ID"}
	}

	trashDir := v.TrashDir()
	metadataPath := fmt.Sprintf("%s/info/%s.json", trashDir, trashID)
	data, err := v.ReadFile(metadataPath)
	if err != nil {
		return "", &Error{Op: "restore", Path: trashID, Code: ErrNotFound, Err: err, Message: "trash metadata not found"}
	}

	var entry TrashEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return "", &Error{Op: "restore", Path: trashID, Code: ErrIO, Err: err, Message: "corrupt trash metadata"}
	}

	// The payload location is derived from the trash ID and the files/<id>/
	// tree. The metadata blob — including any recorded trash_path — is
	// advisory: it is an ordinary document writable through the filesystem,
	// so it must never select the move source.
	payloadPath, err := v.trashPayload(trashDir, trashID, entry.Name)
	if err != nil {
		return "", err
	}

	target := entry.OriginalPath
	if dst != "" {
		target = dst
	}
	targetNorm, err := Normalize(target)
	if err != nil {
		return "", err
	}

	if !overwrite {
		if _, err := v.Stat(target); err == nil {
			return "", &Error{Op: "restore", Path: payloadPath, Dest: target, Code: ErrExist, Message: "destination already exists"}
		}
	}

	parentDir := path.Dir(targetNorm)
	if parentDir != "." {
		if err := v.MkdirAll("/" + parentDir); err != nil {
			return "", err
		}
	}

	if err := v.Move(payloadPath, target, overwrite); err != nil {
		return "", err
	}

	_ = v.Remove(metadataPath)
	_ = v.RemoveAll(fmt.Sprintf("%s/files/%s", trashDir, trashID))

	return target, nil
}

// trashPayload resolves the stored payload for trashID from the
// files/<id>/ tree itself, never from recorded metadata. A single stored
// item is used directly; when the item directory holds several entries the
// advisory name disambiguates. The resolved path is verified to stay inside
// the files/ tree.
func (v *VFS) trashPayload(trashDir, trashID, name string) (string, error) {
	filesDir := trashDir + "/files"
	itemDir := filesDir + "/" + trashID
	payloads, err := v.ReadDir(itemDir)
	if err != nil {
		return "", &Error{Op: "restore", Path: trashID, Code: ErrNotFound, Err: err, Message: "trash payload not found"}
	}
	var payloadPath string
	switch len(payloads) {
	case 0:
		return "", &Error{Op: "restore", Path: trashID, Code: ErrNotFound, Message: "trash payload not found"}
	case 1:
		payloadPath = itemDir + "/" + payloads[0].Name()
	default:
		for _, p := range payloads {
			if p.Name() == name {
				payloadPath = itemDir + "/" + p.Name()
				break
			}
		}
		if payloadPath == "" {
			return "", &Error{Op: "restore", Path: trashID, Code: ErrInvalid, Message: "trash payload is ambiguous"}
		}
	}
	filesNorm, err := Normalize(filesDir)
	if err != nil {
		return "", err
	}
	payloadNorm, err := Normalize(payloadPath)
	if err != nil || !strings.HasPrefix(payloadNorm, filesNorm+"/") {
		return "", &Error{Op: "restore", Path: trashID, Code: ErrInvalid, Message: "trash payload resolves outside the trash tree"}
	}
	return "/" + payloadNorm, nil
}

func (v *VFS) ListTrash() ([]TrashEntry, error) {
	trashDir := v.TrashDir()
	infoDir := trashDir + "/info"
	entries, err := v.ReadDir(infoDir)
	if err != nil {
		if IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []TrashEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := v.ReadFile(infoDir + "/" + e.Name())
		if err != nil {
			continue
		}
		var entry TrashEntry
		if err := json.Unmarshal(data, &entry); err == nil {
			out = append(out, entry)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].TrashedAt.After(out[j].TrashedAt)
	})
	return out, nil
}

func (v *VFS) EmptyTrash() (int, error) {
	items, err := v.ListTrash()
	if err != nil {
		return 0, err
	}
	trashDir := v.TrashDir()
	count := 0
	for _, item := range items {
		_ = v.Remove(fmt.Sprintf("%s/info/%s.json", trashDir, item.ID))
		_ = v.RemoveAll(fmt.Sprintf("%s/files/%s", trashDir, item.ID))
		count++
	}
	return count, nil
}

func (v *VFS) PurgeTrash(trashID string) error {
	if strings.ContainsRune(trashID, '/') || strings.ContainsRune(trashID, '\\') || trashID == "" || trashID == "." || trashID == ".." {
		return &Error{Op: "purge", Path: trashID, Code: ErrInvalid, Message: "invalid trash ID"}
	}
	trashDir := v.TrashDir()
	metadataPath := fmt.Sprintf("%s/info/%s.json", trashDir, trashID)
	if _, err := v.Stat(metadataPath); err != nil {
		return &Error{Op: "purge", Path: trashID, Code: ErrNotFound, Err: err, Message: "trash item not found"}
	}
	_ = v.Remove(metadataPath)
	_ = v.RemoveAll(fmt.Sprintf("%s/files/%s", trashDir, trashID))
	return nil
}
