package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TryLockRoot is the atomic claim behind the duplicate-boot guard (#259):
// held for the lock file's lifetime, it excludes any second boot —
// including one in the same process — until released by Close or death.
func TestTryLockRootMutualExclusion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "env")

	f, err := TryLockRoot(root)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	// The lock file lives beside the root; claiming it must not create
	// or mutate the root itself.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("lock attempt created the root (stat: %v)", err)
	}
	if _, err := TryLockRoot(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}
	g, err := TryLockRoot(root)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = g.Close()
}
