package config

// Regression tests for issues #153 and #157: Store.saveLocked must
// fsync the parent directory after the rename (durability), and must
// stage into a unique O_EXCL temp file so two Store instances over one
// path cannot clobber each other's staging.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestAuditStoreSaveSyncsParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.json")
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	var synced []string
	s.dirSyncHook = func(d string, err error) {
		synced = append(synced, d)
		// Directory fsync is best-effort: where it is unsupported (some
		// filesystems, Windows) an error is tolerated.
		if err != nil && runtime.GOOS != "windows" {
			t.Errorf("directory fsync(%q) failed: %v", d, err)
		}
	}

	if err := s.Set("a.b", 1); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("dir fsyncs = %v, want [%s]", synced, dir)
	}
}

func TestAuditStoreConcurrentWritersSamePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "system.json")
	a, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	const writes = 60
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	stop := make(chan struct{})

	// A reader hammers Load; the document must always parse as complete
	// JSON — a torn read means a rename installed a partial file.
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				continue // file may not exist yet
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				errs <- err
				return
			}
		}
	}()

	for i, s := range []*Store{a, b} {
		wg.Add(1)
		go func(s *Store, n int) {
			defer wg.Done()
			for j := 0; j < writes; j++ {
				if err := s.Set("writer", n); err != nil {
					errs <- err
					return
				}
			}
		}(s, i)
	}
	wg.Wait()
	close(stop)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent access produced an error: %v", err)
	}

	// Final content is valid and written by one of the two writers.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("torn write: %v; content %q", err, data)
	}
	if w, ok := doc["writer"].(float64); !ok || (w != 0 && w != 1) {
		t.Fatalf("writer = %v, want 0 or 1", doc["writer"])
	}

	// No orphaned staging files may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "system.json" {
			t.Errorf("orphaned staging file %q", e.Name())
		}
	}
}
