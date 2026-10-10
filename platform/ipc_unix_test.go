//go:build unix

package platform

import (
	"crypto/sha1"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestListenIPCUnixSocketPath covers the unix domain socket path: the
// endpoint is a unix:// URL, the socket file exists with 0600 permissions,
// and a client can dial it and exchange a byte.
func TestListenIPCUnixSocketPath(t *testing.T) {
	ln, endpoint, err := ListenIPC(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	path := strings.TrimPrefix(endpoint, "unix://")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket file %s: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %s)", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket perms = %o, want 600 (local trust only)", perm)
	}

	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Write([]byte{0x42})
			conn.Close()
		}
	}()
	conn, err := DialIPC(endpoint)
	if err != nil {
		t.Fatalf("DialIPC(%s): %v", endpoint, err)
	}
	defer conn.Close()
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("round trip through unix socket: %v", err)
	}
}

// TestListenIPCUnixReplacesStaleSocket: a leftover socket file from a dead
// instance must not block a new listener.
func TestListenIPCUnixReplacesStaleSocket(t *testing.T) {
	root := t.TempDir()
	first, endpoint, err := ListenIPC(root)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, endpoint2, err := ListenIPC(root)
	if err != nil {
		t.Fatalf("re-listen on a stale socket path failed: %v", err)
	}
	defer second.Close()
	if endpoint2 != endpoint {
		t.Errorf("endpoint = %q, want the same derived path %q", endpoint2, endpoint)
	}
}

// TestListenIPCIgnoresPlantedOccupant: before sockets moved into a private
// directory, the path was predictable — $TMPDIR/gostalgia-<sha1(root)[:5]>.sock —
// and a non-empty directory planted there made the pre-bind os.Remove fail,
// blocking boot. The planted occupant must now be irrelevant.
func TestListenIPCIgnoresPlantedOccupant(t *testing.T) {
	root := t.TempDir()
	sum := sha1.Sum([]byte(root))
	oldPath := filepath.Join(os.TempDir(), fmt.Sprintf("gostalgia-%x.sock", sum[:5]))
	if err := os.MkdirAll(oldPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldPath, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(oldPath)

	ln, endpoint, err := ListenIPC(root)
	if err != nil {
		t.Fatalf("ListenIPC blocked by planted directory at %s: %v", oldPath, err)
	}
	defer ln.Close()
	if path := strings.TrimPrefix(endpoint, "unix://"); path == oldPath {
		t.Fatalf("endpoint still uses the shared predictable path %s", oldPath)
	}
}

// TestListenIPCPrivateSocketDir: the socket file must live in a per-boot
// directory under $TMPDIR that only the runtime can write (0700) — no other
// principal can plant an occupant or unlink a live socket through it.
func TestListenIPCPrivateSocketDir(t *testing.T) {
	ln, endpoint, err := ListenIPC(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	path := strings.TrimPrefix(endpoint, "unix://")
	dir := filepath.Dir(path)
	if filepath.Dir(dir) != filepath.Clean(os.TempDir()) {
		t.Fatalf("socket dir %s is not directly under $TMPDIR %s", dir, os.TempDir())
	}
	if !strings.HasPrefix(filepath.Base(dir), "gostalgia-ipc-") {
		t.Fatalf("socket dir %s does not match the private gostalgia-ipc-* pattern", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket dir perms = %o, want 700 (owner-only)", perm)
	}
}

// TestListenChildIPCPrivateDirAndCleanup: child sockets share the private
// directory, get random names that cannot collide with a predictable path,
// and are still removed by RemoveChildSocket.
func TestListenChildIPCPrivateDirAndCleanup(t *testing.T) {
	ln, endpoint, err := ListenChildIPC("com.test.app")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	path := strings.TrimPrefix(endpoint, "unix://")
	dir := filepath.Dir(path)
	if !strings.HasPrefix(filepath.Base(dir), "gostalgia-ipc-") {
		t.Fatalf("child socket %s is not inside the private socket dir", path)
	}
	if !strings.HasPrefix(filepath.Base(path), "gs-app-") {
		t.Fatalf("child socket name %s lost the gs-app- prefix", path)
	}

	RemoveChildSocket(endpoint)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("child socket %s still present after RemoveChildSocket (err=%v)", path, err)
	}
}

// TestChildIPCHelper is the re-executed child for TestChildIPCRoundTrip. It
// reads a byte from the inherited IPC descriptor (fd 3) and replies on it —
// no connect() is ever performed, which is what makes this transport usable
// under a network-denying sandbox profile.
func TestChildIPCHelper(t *testing.T) {
	if os.Getenv("GOSTALGIA_TEST_IPC_CHILD") != "1" {
		return
	}
	f := os.NewFile(3, "ipc")
	buf := make([]byte, 4)
	if _, err := f.Read(buf); err != nil {
		os.Exit(2)
	}
	if _, err := f.Write([]byte("PONG")); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

// TestChildIPCRoundTrip verifies that a child spawned with the ChildIPC
// descriptor in ExtraFiles can exchange bytes with the parent — the core of
// the #83 fix (IPC without any unix-socket connect capability).
func TestChildIPCRoundTrip(t *testing.T) {
	conn, child, err := ChildIPC()
	if err != nil {
		t.Fatalf("ChildIPC: %v", err)
	}
	defer conn.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestChildIPCHelper$")
	cmd.Env = append(os.Environ(), "GOSTALGIA_TEST_IPC_CHILD=1")
	cmd.ExtraFiles = []*os.File{child}
	if err := cmd.Start(); err != nil {
		child.Close()
		t.Fatalf("start helper: %v", err)
	}
	// Parent drops its copy so the child fully owns the peer end.
	child.Close()

	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("PING")); err != nil {
		t.Fatalf("write to child: %v", err)
	}
	buf := make([]byte, 4)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read from child: %v", err)
	}
	if string(buf) != "PONG" {
		t.Fatalf("reply = %q, want PONG", buf)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
}
