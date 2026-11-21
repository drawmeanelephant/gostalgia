package vfs

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// ScopedVFS provides an application-scoped view of the virtual filesystem.
// It enforces app-private storage partitioning, path-scoped grants,
// cross-app isolation, and confinement checks on every filesystem operation.
type ScopedVFS struct {
	vfs   *VFS
	appID string
}

var _ DocumentFS = (*ScopedVFS)(nil)

// AppID returns the application ID this scoped view is bound to.
func (s *ScopedVFS) AppID() string {
	return s.appID
}

// ensurePrivateStorage ensures that the app-private storage partition exists.
func (s *ScopedVFS) ensurePrivateStorage() error {
	return s.vfs.MkdirAll(AppDataDir(s.appID))
}

// check verifies that appID has permission for envPath in the requested mode,
// and ensures that symlinks on host-backed storage do not escape the authorized root.
func (s *ScopedVFS) check(op, envPath string, mode AccessMode) error {
	norm, err := Normalize(envPath)
	if err != nil {
		return &Error{Op: op, Path: envPath, Code: ErrInvalid, Message: fmt.Sprintf("invalid path: %v", err)}
	}
	cleanPath := "/" + norm

	// Check access policy via GrantStore
	if err := s.vfs.CheckAccess(s.appID, cleanPath, mode); err != nil {
		if vErr, ok := err.(*Error); ok {
			vErr.Op = op
			return vErr
		}
		return &Error{Op: op, Path: cleanPath, Code: ErrPermission, Err: err, Message: err.Error()}
	}

	// Determine the authorized root for confinement checking
	var authorizedRoot string
	if IsAppPrivatePath(s.appID, cleanPath) {
		authorizedRoot = AppDataDir(s.appID)
	} else if gs := s.vfs.Grants(); gs != nil {
		if g, ok := gs.FindMatchingGrant(s.appID, cleanPath); ok {
			authorizedRoot = g.Path
		}
	}

	if authorizedRoot != "" {
		if err := s.checkSymlinkConfinement(op, cleanPath, authorizedRoot); err != nil {
			return err
		}
	}
	return nil
}

// checkTree verifies bulk-operation authorization for envPath. Beyond the
// per-path check, a directory operand reads or mutates every descendant
// beneath it, so a non-recursive grant covering only the directory entry
// is not enough: the operation is denied unless app-private storage, a
// shared host mount, or a recursive grant covers the whole subtree.
func (s *ScopedVFS) checkTree(op, envPath string, mode AccessMode, dir bool) error {
	if err := s.check(op, envPath, mode); err != nil {
		return err
	}
	if dir && !s.coversSubtree(envPath, mode) {
		return &Error{
			Op:      op,
			Path:    envPath,
			Code:    ErrPermission,
			Message: fmt.Sprintf("permission denied: bulk operation on directory %q requires a recursive grant (a non-recursive grant covers only the directory itself)", envPath),
		}
	}
	return nil
}

// coversSubtree reports whether appID may perform mode on envPath and every
// descendant beneath it.
func (s *ScopedVFS) coversSubtree(envPath string, mode AccessMode) bool {
	norm, err := Normalize(envPath)
	if err != nil {
		return false
	}
	cleanPath := "/" + norm
	if IsAppPrivatePath(s.appID, cleanPath) {
		return true
	}
	if isShared, _, _ := s.vfs.SharedHostInfo(cleanPath); isShared {
		return true
	}
	if gs := s.vfs.Grants(); gs != nil {
		return gs.CoversSubtree(s.appID, cleanPath, mode)
	}
	// No grant store: the VFS is unscoped, matching CheckAccess.
	return true
}

// isDir reports whether envPath exists and is a directory.
func (s *ScopedVFS) isDir(envPath string) bool {
	info, err := s.vfs.Stat(envPath)
	return err == nil && info.IsDir()
}

// checkSymlinkConfinement verifies that the resolved host path does not escape the authorized root.
func (s *ScopedVFS) checkSymlinkConfinement(op, envPath, authorizedRoot string) error {
	normTarget, err := Normalize(envPath)
	if err != nil {
		return err
	}
	fsys, sub := s.vfs.resolve(normTarget)
	hostFS, ok := fsys.(*HostFS)
	if !ok {
		return nil // in-memory or other backend without host symlink escapes
	}

	normAuth, err := Normalize(authorizedRoot)
	if err != nil {
		return err
	}
	authFS, authSub := s.vfs.resolve(normAuth)
	authHost, ok := authFS.(*HostFS)
	if !ok {
		return nil
	}

	hostTarget := filepath.Join(hostFS.RootPath(), sub)
	hostAuthRoot := filepath.Join(authHost.RootPath(), authSub)

	evalRoot, err := filepath.EvalSymlinks(hostAuthRoot)
	if err != nil {
		// If the authorized root doesn't exist on host yet, no escape is possible
		return nil
	}

	targetToEval := hostTarget
	for {
		evalTarget, err := filepath.EvalSymlinks(targetToEval)
		if err == nil {
			sep := string(filepath.Separator)
			if evalTarget != evalRoot && !strings.HasPrefix(evalTarget, evalRoot+sep) {
				return &Error{
					Op:      op,
					Path:    envPath,
					Code:    ErrEscape,
					Message: fmt.Sprintf("symlink escape detected: %q resolves to %q outside authorized root %q", envPath, evalTarget, authorizedRoot),
				}
			}
			break
		}
		parent := filepath.Dir(targetToEval)
		if parent == targetToEval {
			break
		}
		targetToEval = parent
	}
	return nil
}

func (s *ScopedVFS) Open(name string) (fs.File, error) {
	if err := s.check("open", name, AccessRead); err != nil {
		return nil, err
	}
	return s.vfs.Open(name)
}

func (s *ScopedVFS) Stat(name string) (fs.FileInfo, error) {
	norm, err := Normalize(name)
	if err != nil {
		return nil, &Error{Op: "stat", Path: name, Code: ErrInvalid, Message: err.Error()}
	}

	// App-private root stat
	if norm == "apps/data" {
		return nil, &Error{Op: "stat", Path: name, Code: ErrPermission, Message: "permission denied: cannot access app private storage root"}
	}
	if norm == "apps/data/"+s.appID {
		_ = s.ensurePrivateStorage()
		return s.vfs.Stat(name)
	}

	if err := s.check("stat", name, AccessRead); err != nil {
		return nil, err
	}
	return s.vfs.Stat(name)
}

func (s *ScopedVFS) ReadFile(name string) ([]byte, error) {
	if err := s.check("read", name, AccessRead); err != nil {
		return nil, err
	}
	return s.vfs.ReadFile(name)
}

func (s *ScopedVFS) ReadDir(name string) ([]fs.DirEntry, error) {
	norm, err := Normalize(name)
	if err != nil {
		return nil, &Error{Op: "readdir", Path: name, Code: ErrInvalid, Message: err.Error()}
	}

	// 1. Listing /apps/data: only show this app's own directory (principal-aware view)
	if norm == "apps/data" {
		_ = s.ensurePrivateStorage()
		entries, err := s.vfs.ReadDir("/apps/data")
		if err != nil {
			return nil, err
		}
		var filtered []fs.DirEntry
		for _, e := range entries {
			if e.Name() == s.appID {
				filtered = append(filtered, e)
			}
		}
		return filtered, nil
	}

	// 2. Listing own app private storage directory
	if norm == "apps/data/"+s.appID {
		_ = s.ensurePrivateStorage()
		return s.vfs.ReadDir(name)
	}

	// 3. Regular path: check access
	if err := s.check("readdir", name, AccessRead); err != nil {
		// If direct access denied, check if this is an ancestor directory
		// containing explicitly granted subpaths (principal-aware view)
		if gs := s.vfs.Grants(); gs != nil {
			grants := gs.List(s.appID)
			var matchingSubEntries map[string]bool
			for _, g := range grants {
				if g.Revoked {
					continue
				}
				gNorm, _ := Normalize(g.Path)
				prefix := norm + "/"
				if strings.HasPrefix(gNorm, prefix) {
					sub := strings.TrimPrefix(gNorm, prefix)
					top := strings.Split(sub, "/")[0]
					if top != "" {
						if matchingSubEntries == nil {
							matchingSubEntries = make(map[string]bool)
						}
						matchingSubEntries[top] = true
					}
				}
			}
			if len(matchingSubEntries) > 0 {
				entries, err := s.vfs.ReadDir(name)
				if err != nil {
					return nil, err
				}
				var filtered []fs.DirEntry
				for _, e := range entries {
					if matchingSubEntries[e.Name()] {
						filtered = append(filtered, e)
					}
				}
				return filtered, nil
			}
		}
		return nil, err
	}

	return s.vfs.ReadDir(name)
}

func (s *ScopedVFS) MkdirAll(name string) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "mkdirall", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" {
		return &Error{Op: "mkdirall", Path: name, Code: ErrPermission, Message: "permission denied: cannot modify app private storage root"}
	}
	if norm == "apps/data/"+s.appID {
		return s.ensurePrivateStorage()
	}
	if err := s.check("mkdirall", name, AccessReadWrite); err != nil {
		return err
	}
	return s.vfs.MkdirAll(name)
}

func (s *ScopedVFS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "write", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" || norm == "apps/data/"+s.appID {
		return &Error{Op: "write", Path: name, Code: ErrIsDir, Message: "path is a directory"}
	}
	if err := s.check("write", name, AccessReadWrite); err != nil {
		return err
	}
	if IsAppPrivatePath(s.appID, name) {
		_ = s.ensurePrivateStorage()
	}
	return s.vfs.WriteFile(name, data, perm)
}

func (s *ScopedVFS) SaveAtomic(name string, data []byte, perm fs.FileMode) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "save", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" || norm == "apps/data/"+s.appID {
		return &Error{Op: "save", Path: name, Code: ErrIsDir, Message: "cannot save to directory"}
	}
	if err := s.check("save", name, AccessReadWrite); err != nil {
		return err
	}
	if IsAppPrivatePath(s.appID, name) {
		_ = s.ensurePrivateStorage()
	}
	return s.vfs.SaveAtomic(name, data, perm)
}

func (s *ScopedVFS) SaveAtomicOpt(name string, data []byte, perm fs.FileMode, overwrite bool) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "save", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" || norm == "apps/data/"+s.appID {
		return &Error{Op: "save", Path: name, Code: ErrIsDir, Message: "cannot save to directory"}
	}
	if err := s.check("save", name, AccessReadWrite); err != nil {
		return err
	}
	if IsAppPrivatePath(s.appID, name) {
		_ = s.ensurePrivateStorage()
	}
	return s.vfs.SaveAtomicOpt(name, data, perm, overwrite)
}

func (s *ScopedVFS) Remove(name string) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "remove", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" || norm == "apps/data/"+s.appID {
		return &Error{Op: "remove", Path: name, Code: ErrInvalid, Message: "cannot remove app private storage root"}
	}
	if err := s.checkTree("remove", name, AccessReadWrite, s.isDir(name)); err != nil {
		return err
	}
	return s.vfs.Remove(name)
}

func (s *ScopedVFS) RemoveAll(name string) error {
	norm, err := Normalize(name)
	if err != nil {
		return &Error{Op: "removeall", Path: name, Code: ErrInvalid, Message: err.Error()}
	}
	if norm == "apps/data" || norm == "apps/data/"+s.appID {
		return &Error{Op: "removeall", Path: name, Code: ErrInvalid, Message: "cannot remove app private storage root"}
	}
	if err := s.checkTree("removeall", name, AccessReadWrite, s.isDir(name)); err != nil {
		return err
	}
	return s.vfs.RemoveAll(name)
}

func (s *ScopedVFS) Rename(oldPath, newPath string) error {
	return s.RenameOpt(oldPath, newPath, false)
}

func (s *ScopedVFS) RenameOpt(oldPath, newPath string, overwrite bool) error {
	oldNorm, err := Normalize(oldPath)
	if err != nil {
		return &Error{Op: "rename", Path: oldPath, Dest: newPath, Code: ErrInvalid, Message: err.Error()}
	}
	newNorm, err := Normalize(newPath)
	if err != nil {
		return &Error{Op: "rename", Path: oldPath, Dest: newPath, Code: ErrInvalid, Message: err.Error()}
	}
	if oldNorm == "apps/data" || oldNorm == "apps/data/"+s.appID || newNorm == "apps/data" || newNorm == "apps/data/"+s.appID {
		return &Error{Op: "rename", Path: oldPath, Dest: newPath, Code: ErrInvalid, Message: "cannot rename app private storage root"}
	}
	// Renaming a directory relocates its whole subtree: both ends need
	// coverage for every descendant, not just the directory entry.
	dir := s.isDir(oldPath)
	if err := s.checkTree("rename", oldPath, AccessReadWrite, dir); err != nil {
		return err
	}
	if err := s.checkTree("rename", newPath, AccessReadWrite, dir); err != nil {
		return err
	}
	return s.vfs.RenameOpt(oldPath, newPath, overwrite)
}

func (s *ScopedVFS) Copy(src, dst string, overwrite bool) error {
	// Copying a directory reads every descendant of src and writes a mirror
	// of them beneath dst: both need coverage for the whole subtree.
	dir := s.isDir(src)
	if err := s.checkTree("copy", src, AccessRead, dir); err != nil {
		return err
	}
	if err := s.checkTree("copy", dst, AccessReadWrite, dir); err != nil {
		return err
	}
	if IsAppPrivatePath(s.appID, dst) {
		_ = s.ensurePrivateStorage()
	}
	return s.vfs.Copy(src, dst, overwrite)
}

func (s *ScopedVFS) Move(src, dst string, overwrite bool) error {
	// Moving a directory removes the whole subtree at src and recreates it
	// beneath dst: both need coverage for every descendant.
	dir := s.isDir(src)
	if err := s.checkTree("move", src, AccessReadWrite, dir); err != nil {
		return err
	}
	if err := s.checkTree("move", dst, AccessReadWrite, dir); err != nil {
		return err
	}
	if IsAppPrivatePath(s.appID, dst) {
		_ = s.ensurePrivateStorage()
	}
	return s.vfs.Move(src, dst, overwrite)
}

func (s *ScopedVFS) Trash(targetPath string) (*TrashEntry, error) {
	if err := s.checkTree("trash", targetPath, AccessReadWrite, s.isDir(targetPath)); err != nil {
		return nil, err
	}
	return s.vfs.Trash(targetPath)
}

func (s *ScopedVFS) Restore(trashID, dst string, overwrite bool) (string, error) {
	if trashID == "" {
		return "", &Error{Op: "restore", Code: ErrInvalid, Message: "trash ID is required"}
	}
	// Verify that the trash entry was from a path accessible to this app
	entries, err := s.vfs.ListTrash()
	if err != nil {
		return "", err
	}
	var targetEntry *TrashEntry
	for _, e := range entries {
		if e.ID == trashID {
			targetEntry = &e
			break
		}
	}
	if targetEntry == nil {
		return "", &Error{Op: "restore", Code: ErrNotFound, Message: fmt.Sprintf("trash item %q not found", trashID)}
	}

	// Must have write access to original path; a directory entry restores a
	// whole subtree, so it needs recursive coverage too.
	if err := s.checkTree("restore", targetEntry.OriginalPath, AccessReadWrite, targetEntry.IsDir); err != nil {
		return "", err
	}

	// If destination is specified, must have write access to destination
	if dst != "" {
		if err := s.checkTree("restore", dst, AccessReadWrite, targetEntry.IsDir); err != nil {
			return "", err
		}
	}

	return s.vfs.Restore(trashID, dst, overwrite)
}

func (s *ScopedVFS) ListTrash() ([]TrashEntry, error) {
	all, err := s.vfs.ListTrash()
	if err != nil {
		return nil, err
	}
	var filtered []TrashEntry
	for _, e := range all {
		// Only show trash items originally from paths accessible to this app
		if err := s.check("list_trash", e.OriginalPath, AccessRead); err == nil {
			filtered = append(filtered, e)
		}
	}
	return filtered, nil
}

func (s *ScopedVFS) EmptyTrash() (int, error) {
	// Only empty trash entries that belong to this app's accessible scope
	myEntries, err := s.ListTrash()
	if err != nil {
		return 0, err
	}
	count := 0
	for _, e := range myEntries {
		if err := s.vfs.PurgeTrash(e.ID); err == nil {
			count++
		}
	}
	return count, nil
}

func (s *ScopedVFS) PurgeTrash(trashID string) error {
	entries, err := s.vfs.ListTrash()
	if err != nil {
		return err
	}
	var targetEntry *TrashEntry
	for _, e := range entries {
		if e.ID == trashID {
			targetEntry = &e
			break
		}
	}
	if targetEntry == nil {
		return &Error{Op: "purgetrash", Code: ErrNotFound, Message: fmt.Sprintf("trash item %q not found", trashID)}
	}
	if err := s.checkTree("purgetrash", targetEntry.OriginalPath, AccessReadWrite, targetEntry.IsDir); err != nil {
		return err
	}
	return s.vfs.PurgeTrash(trashID)
}
