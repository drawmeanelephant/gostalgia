package vfs

// Regression test for issue #153: HostFS.SaveAtomic must fsync the
// parent directory after the atomic rename — without it, a crash can
// lose the directory entry even though the file data was fsynced.

import (
	"runtime"
	"testing"
)

func TestAuditHostFSSaveAtomicSyncsParentDir(t *testing.T) {
	h := newTestHostFS(t)
	defer h.Close()

	var synced []string
	h.saveDirSyncHook = func(dir string, err error) {
		synced = append(synced, dir)
		// Directory fsync is best-effort: filesystems that cannot sync a
		// directory handle (and Windows) may report an error. Everywhere
		// it is supported it must succeed.
		if err != nil && runtime.GOOS != "windows" {
			t.Errorf("directory fsync(%q) failed: %v", dir, err)
		}
	}

	if err := h.SaveAtomic("sub/file.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	if err := h.SaveAtomic("root.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}

	want := []string{"sub", "."}
	if len(synced) != len(want) {
		t.Fatalf("dir fsyncs = %v, want %v", synced, want)
	}
	for i := range want {
		if synced[i] != want[i] {
			t.Fatalf("dir fsyncs = %v, want %v", synced, want)
		}
	}
}
