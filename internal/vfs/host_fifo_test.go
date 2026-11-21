//go:build unix

package vfs

// Regression tests for the FIFO hang (issue #16): os.Root.Open on a FIFO
// blocks until a writer opens the other end, so Stat and ReadDir must
// never Open a special file. Everything here uses unix-only file types
// (FIFO, unix socket), hence the build tag.

import (
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const fifoDeadline = 4 * time.Second

// statResult carries an operation's outcome out of a goroutine so all
// assertions run on the test goroutine.
type statResult struct {
	info fs.FileInfo
	err  error
}

// runWithDeadline runs op in a goroutine. If it has not returned within
// fifoDeadline the test fails; with the regression present the goroutine
// stays blocked forever, which is exactly the defect being caught.
func runWithDeadline(t *testing.T, what string, op func() (fs.FileInfo, error)) (fs.FileInfo, error) {
	t.Helper()
	ch := make(chan statResult, 1)
	go func() {
		info, err := op()
		ch <- statResult{info, err}
	}()
	select {
	case r := <-ch:
		return r.info, r.err
	case <-time.After(fifoDeadline):
		t.Fatalf("%s did not return within %s", what, fifoDeadline)
		return nil, nil
	}
}

func TestHostFSStatDoesNotBlockOnSpecialFiles(t *testing.T) {
	// darwin caps a unix socket address at 104 bytes and t.TempDir()
	// paths routinely exceed it, so the root lives in the shortest
	// conventional temp location.
	dir, err := os.MkdirTemp("/tmp", "gostalgia-fifo-")
	must(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
	sock := filepath.Join(dir, "sock")
	ln, err := net.Listen("unix", sock)
	must(t, err)
	// Keep the listener bound for the test: closing it unlinks the
	// socket file on darwin.
	t.Cleanup(func() { _ = ln.Close() })

	h, err := NewHost(dir)
	must(t, err)
	t.Cleanup(func() { h.Close() })

	info, err := runWithDeadline(t, "Stat(fifo)", func() (fs.FileInfo, error) {
		return h.Stat("fifo")
	})
	must(t, err)
	if info.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("Stat(fifo).Mode() = %v, want named pipe", info.Mode())
	}

	info, err = runWithDeadline(t, "Stat(sock)", func() (fs.FileInfo, error) {
		return h.Stat("sock")
	})
	must(t, err)
	if info.Mode()&fs.ModeSocket == 0 {
		t.Errorf("Stat(sock).Mode() = %v, want socket", info.Mode())
	}
}

func TestHostFSReadDirListsSpecialFilesWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644))
	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))

	h, err := NewHost(dir)
	must(t, err)
	t.Cleanup(func() { h.Close() })

	// One FIFO entry used to block the whole listing, and behind it
	// every Server.Close waiting on the fs/list handler.
	ch := make(chan struct {
		entries []fs.DirEntry
		err     error
	}, 1)
	go func() {
		entries, err := h.ReadDir(".")
		ch <- struct {
			entries []fs.DirEntry
			err     error
		}{entries, err}
	}()
	var entries []fs.DirEntry
	select {
	case r := <-ch:
		must(t, r.err)
		entries = r.entries
	case <-time.After(fifoDeadline):
		t.Fatalf("HostFS.ReadDir blocked >%s on a FIFO entry", fifoDeadline)
	}

	if len(entries) != 2 || entries[0].Name() != "a.txt" || entries[1].Name() != "fifo" {
		t.Fatalf("root entries = %v, want [a.txt fifo]", names(entries))
	}

	// The FIFO entry survives the listing and its metadata is the
	// special-file kind.
	fifo := entries[1]
	if fifo.Type()&fs.ModeNamedPipe == 0 {
		t.Errorf("fifo entry Type() = %v, want named pipe", fifo.Type())
	}
	fifoInfo, err := fifo.Info()
	must(t, err)
	if fifoInfo.Mode()&fs.ModeNamedPipe == 0 {
		t.Errorf("fifo entry Info().Mode() = %v, want named pipe", fifoInfo.Mode())
	}

	// The per-entry invariant holds for both kinds: what ReadDir's
	// entries report matches what Stat answers for the same path.
	for _, e := range entries {
		entryInfo, err := e.Info()
		must(t, err)
		statInfo, err := h.Stat(e.Name())
		must(t, err)
		if entryInfo.Mode() != statInfo.Mode() || entryInfo.Size() != statInfo.Size() {
			t.Errorf("%s: entry.Info() = mode %v size %d, Stat() = mode %v size %d",
				e.Name(), entryInfo.Mode(), entryInfo.Size(), statInfo.Mode(), statInfo.Size())
		}
	}
}

func names(entries []fs.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}
