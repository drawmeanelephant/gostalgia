package runtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/platform"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pingRouter returns a router with a sys/ping route, as a started sys
// service would have registered it.
func pingRouter(t *testing.T) *ipc.Router {
	t.Helper()
	r := ipc.NewRouter()
	if err := r.Handle("sys/ping", func(ctx context.Context, req ipc.Request) (any, error) {
		return map[string]bool{"pong": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

// serveIPC opens the endpoint ListenIPC derives from root and serves auth
// over the given router, the way a booting environment does mid-start.
func serveIPC(t *testing.T, root, token string, r *ipc.Router) string {
	t.Helper()
	ln, endpoint, err := platform.ListenIPC(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := ipc.NewServer(ln, r, token, discardLogger())
	go srv.Serve()
	t.Cleanup(func() { srv.Close() })
	return endpoint
}

func writeRuntimeFile(t *testing.T, root, endpoint, token string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"pid": os.Getpid(), "endpoint": endpoint, "token": token})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNotRunningAcceptsMissingFile(t *testing.T) {
	if err := checkNotRunning(t.TempDir()); err != nil {
		t.Fatalf("checkNotRunning with no runtime.json: %v", err)
	}
}

// TestCheckNotRunningIgnoresStaleFile: a runtime.json whose endpoint no
// longer answers is a file from a dead instance and must not block boot.
func TestCheckNotRunningIgnoresStaleFile(t *testing.T) {
	root := t.TempDir()
	ln, endpoint, err := platform.ListenIPC(root)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close() // down again: nothing answers on the endpoint

	writeRuntimeFile(t, root, endpoint, "tok")
	if err := checkNotRunning(root); err != nil {
		t.Fatalf("stale runtime.json must be ignored, got: %v", err)
	}
}

func TestCheckNotRunningRefusesLiveInstance(t *testing.T) {
	root := t.TempDir()
	endpoint := serveIPC(t, root, "tok", pingRouter(t))
	writeRuntimeFile(t, root, endpoint, "tok")
	if err := checkNotRunning(root); err == nil {
		t.Fatal("checkNotRunning accepted a live environment, want refusal")
	}
}

// TestCheckNotRunningRefusesBootingInstance is the duplicate-boot-guard
// regression: an instance that has bound its socket and written
// runtime.json but not yet registered its sys routes used to read as
// "stale" to a second boot, which then sailed straight past the guard.
func TestCheckNotRunningRefusesBootingInstance(t *testing.T) {
	root := t.TempDir()
	endpoint := serveIPC(t, root, "tok", ipc.NewRouter()) // no sys routes yet
	writeRuntimeFile(t, root, endpoint, "tok")
	if err := checkNotRunning(root); err == nil {
		t.Fatal("checkNotRunning mistook a booting instance for a dead one")
	}
}

// TestCheckNotRunningIgnoresForeignInstance: a listener that rejects the
// recorded token is not the environment this root describes.
func TestCheckNotRunningIgnoresForeignInstance(t *testing.T) {
	root := t.TempDir()
	endpoint := serveIPC(t, root, "other-token", pingRouter(t))
	writeRuntimeFile(t, root, endpoint, "tok")
	if err := checkNotRunning(root); err != nil {
		t.Fatalf("checkNotRunning must ignore an instance that rejects the token, got: %v", err)
	}
}

// Regression for #262: a listener whose accept loop died stays bound, so
// Dial still succeeds but the auth handshake never completes. The guard
// used to read that as "stale file" and boot over a live environment.
// Connected-but-unresponsive must be occupied, and it must fail fast —
// not after the client's 30 s default call timeout.
func TestCheckNotRunningOccupiedWhenUnresponsive(t *testing.T) {
	root := t.TempDir()
	ln, endpoint, err := platform.ListenIPC(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() }) // bound, never Accept()ed
	writeRuntimeFile(t, root, endpoint, "tok")

	start := time.Now()
	if err := checkNotRunning(root); err == nil {
		t.Fatal("guard reported the root free while its endpoint is bound but unresponsive")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("guard waited %v on a dead endpoint; it must fail fast", d)
	}
}
