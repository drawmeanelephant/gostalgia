//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// TryLockRoot attempts the exclusive, process-lifetime claim on an
// environment root that the duplicate-boot guard relies on. The returned
// file holds the LockFileEx lock: Close releases it, and so does process
// death — a crashed instance can never wedge its root. A second caller
// holding the lock gets ErrLocked.
func TryLockRoot(root string) (*os.File, error) {
	path := rootLockPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("platform: create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("platform: open root lock: %w", err)
	}
	// Byte range [0,1); FAIL_IMMEDIATELY makes this a try-lock.
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol)
	if err != nil {
		_ = f.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, root)
		}
		return nil, fmt.Errorf("platform: lock %s: %w", path, err)
	}
	return f, nil
}
