package vfs

// Regression test for issue #149: MemFS.SaveAtomic staged the payload
// into name+".recover" before reporting ErrIsDir for a directory
// destination, so a second failed save silently clobbered the first
// artifact's data. An existing .recover artifact may be the only copy
// of an earlier failed save and must survive.

import (
	"errors"
	"testing"
)

func TestAuditMemFSSaveAtomicClobbersRecoverArtifact(t *testing.T) {
	m := NewMem()
	if err := m.MkdirAll("dir"); err != nil {
		t.Fatal(err)
	}

	// First failed save leaves payload A in dir.recover.
	err := m.SaveAtomic("dir", []byte("first"), 0o644)
	var vfsErr *Error
	if !errors.As(err, &vfsErr) || vfsErr.Code != ErrIsDir {
		t.Fatalf("SaveAtomic(dir) = %v, want ErrIsDir", err)
	}
	got, err := m.ReadFile("dir.recover")
	if err != nil || string(got) != "first" {
		t.Fatalf("dir.recover = %q, %v after first save", got, err)
	}

	// Second failed save must not destroy the first artifact.
	err = m.SaveAtomic("dir", []byte("second"), 0o644)
	if !errors.As(err, &vfsErr) || vfsErr.Code != ErrIsDir {
		t.Fatalf("second SaveAtomic(dir) = %v, want ErrIsDir", err)
	}
	got, err = m.ReadFile("dir.recover")
	if err != nil || string(got) != "first" {
		t.Fatalf("second SaveAtomic clobbered the first .recover artifact (now %q); recoverable data was lost", got)
	}
}
