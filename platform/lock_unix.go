//go:build unix

package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// TryLockRoot attempts the exclusive, process-lifetime claim on an
// environment root that the duplicate-boot guard relies on. The returned
// file holds the kernel lock: Close releases it, and so does process
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, root)
		}
		return nil, fmt.Errorf("platform: lock %s: %w", path, err)
	}
	return f, nil
}
