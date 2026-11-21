package vfs

import (
	"fmt"
	"io/fs"
)

// SharedHostFS is an FS backed by an explicitly mounted host directory.
// Confinement is strictly enforced by os.Root via HostFS, failing closed
// on dot segments ("..") and escaping symlinks.
type SharedHostFS struct {
	*HostFS
	readOnly bool
}

// NewSharedHost opens hostDir as an explicitly mounted shared host folder.
func NewSharedHost(hostDir string, readOnly bool) (*SharedHostFS, error) {
	h, err := NewHost(hostDir)
	if err != nil {
		return nil, fmt.Errorf("vfs: open shared host folder %s: %w", hostDir, err)
	}
	return &SharedHostFS{
		HostFS:   h,
		readOnly: readOnly,
	}, nil
}

// IsReadOnly reports whether the mount is read-only.
func (s *SharedHostFS) IsReadOnly() bool {
	return s.readOnly
}

func (s *SharedHostFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	if s.readOnly {
		return &Error{Op: "write", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.WriteFile(path, data, perm)
}

func (s *SharedHostFS) MkdirAll(path string) error {
	if s.readOnly {
		return &Error{Op: "mkdir", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.MkdirAll(path)
}

func (s *SharedHostFS) Remove(path string) error {
	if s.readOnly {
		return &Error{Op: "remove", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.Remove(path)
}

func (s *SharedHostFS) Rename(oldName, newName string) error {
	if s.readOnly {
		return &Error{Op: "rename", Path: oldName, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.Rename(oldName, newName)
}

func (s *SharedHostFS) RemoveAll(path string) error {
	if s.readOnly {
		return &Error{Op: "remove_all", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.RemoveAll(path)
}

func (s *SharedHostFS) SaveAtomic(path string, data []byte, perm fs.FileMode) error {
	if s.readOnly {
		return &Error{Op: "save", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.SaveAtomic(path, data, perm)
}

// SaveAtomicBounded enforces the read-only contract, then delegates the
// bounded large save to the host backend.
func (s *SharedHostFS) SaveAtomicBounded(path string, data []byte, perm fs.FileMode, limit int64) error {
	if s.readOnly {
		return &Error{Op: "save", Path: path, Code: ErrReadOnly, Message: "shared host mount is read-only"}
	}
	return s.HostFS.SaveAtomicBounded(path, data, perm, limit)
}
