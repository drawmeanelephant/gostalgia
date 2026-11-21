//go:build unix

package vfs

// Regression tests for the open-path FIFO hang (issue #79): Stat and
// ReadDir were hardened in #16, but Open, ReadFile, and WriteFile still
// called os.Root.Open on special files — a FIFO blocks until a writer
// arrives. These paths must reject non-regular files promptly.

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestHostFSOpenRejectsSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	h, err := NewHost(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	for _, tc := range []struct {
		name string
		op   func() error
	}{
		{"Open", func() error {
			f, err := h.Open("fifo")
			if err == nil {
				_ = f.Close()
			}
			return err
		}},
		{"ReadFile", func() error {
			_, err := h.ReadFile("fifo")
			return err
		}},
		{"WriteFile", func() error {
			return h.WriteFile("fifo", []byte("x"), 0o644)
		}},
	} {
		done := make(chan error, 1)
		go func() { done <- tc.op() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s(fifo) succeeded; special files must be rejected", tc.name)
			}
		case <-time.After(fifoDeadline):
			t.Errorf("%s(fifo) did not return within %s", tc.name, fifoDeadline)
		}
	}

	// Regular files and directories are unaffected.
	f, err := h.Open(".")
	if err != nil {
		t.Fatalf("Open(dir) failed: %v", err)
	}
	_ = f.Close()

	if err := h.WriteFile("reg.txt", []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile(reg.txt) failed: %v", err)
	}
	data, err := h.ReadFile("reg.txt")
	if err != nil || string(data) != "data" {
		t.Fatalf("ReadFile(reg.txt) = %q, %v", data, err)
	}
}
