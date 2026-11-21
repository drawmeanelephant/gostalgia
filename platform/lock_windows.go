//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// errSharingViolation is ERROR_SHARING_VIOLATION, returned by CreateFile
// when another process holds the file open with sharing disabled.
const errSharingViolation = syscall.Errno(32)

// TryLockRoot attempts the exclusive, process-lifetime claim on an
// environment root that the duplicate-boot guard relies on. The returned
// file is opened with dwShareMode = 0: the open itself is the exclusive
// lock, held for the file's lifetime. Close releases it, and so does
// process death — a crashed instance can never wedge its root. A second
// caller gets ErrLocked.
func TryLockRoot(root string) (*os.File, error) {
	path := rootLockPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("platform: create lock dir: %w", err)
	}
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, fmt.Errorf("platform: lock path: %w", err)
	}
	h, err := syscall.CreateFile(name,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, root)
		}
		return nil, fmt.Errorf("platform: lock %s: %w", path, err)
	}
	return os.NewFile(uintptr(h), path), nil
}
