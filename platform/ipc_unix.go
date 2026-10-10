//go:build unix

package platform

import (
	"crypto/rand"
	"crypto/sha1"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ipcDirOnce sync.Once
	ipcDirPath string
	ipcDirErr  error
)

// ipcSocketDir returns the private directory that holds every unix socket
// the runtime binds. It is created once per process under os.TempDir() with
// owner-only permissions, so nothing outside the runtime can predict a
// socket path, pre-plant an occupant, or unlink a live socket from under
// the listener — the shared-temp-dir boot DoS and IPC-outage vectors.
// POSIX permissions cannot exclude same-uid processes, so sandbox launch
// policies also mask the directory (see IPCSocketDir callers).
func ipcSocketDir() (string, error) {
	ipcDirOnce.Do(func() {
		ipcDirPath, ipcDirErr = os.MkdirTemp("", "gostalgia-ipc-")
		if ipcDirErr != nil {
			return
		}
		if err := os.Chmod(ipcDirPath, 0o700); err != nil {
			_ = os.Remove(ipcDirPath)
			ipcDirPath = ""
			ipcDirErr = err
		}
	})
	return ipcDirPath, ipcDirErr
}

// IPCSocketDir returns the private per-process directory that holds the
// runtime's unix socket files, creating it on first call. It is exported so
// sandbox policies can mask it for confined children. Empty on error.
func IPCSocketDir() string {
	dir, err := ipcSocketDir()
	if err != nil {
		return ""
	}
	return dir
}

// ListenIPC opens the environment's local IPC listener. Unix-like hosts
// use a domain socket inside a private per-boot directory, named by a hash
// of the environment root. The name is kept short deliberately: domain
// socket paths are limited to about 104 bytes on macOS, and environment
// roots can live deep in temp or home directories.
func ListenIPC(root string) (net.Listener, string, error) {
	dir, err := ipcSocketDir()
	if err != nil {
		return nil, "", fmt.Errorf("platform: ipc socket dir: %w", err)
	}
	sum := sha1.Sum([]byte(root))
	path := filepath.Join(dir, fmt.Sprintf("gostalgia-%x.sock", sum[:5]))
	// Only this process can write the directory, so a path occupant can
	// only be a stale socket of ours: safe to replace, and a remove
	// failure is a real error, not an attacker-planted boot blocker.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, "", fmt.Errorf("platform: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, "", err
	}
	return ln, "unix://" + path, nil
}

// ListenChildIPC creates a dedicated IPC socket for a child application process.
// The socket is created inside the private IPC directory with a random name
// and 0600 permissions. A name collision yields a fresh name rather than an
// unlink of whatever holds the path.
func ListenChildIPC(appID string) (net.Listener, string, error) {
	dir, err := ipcSocketDir()
	if err != nil {
		return nil, "", fmt.Errorf("platform: ipc socket dir: %w", err)
	}
	const attempts = 8
	for i := 0; i < attempts; i++ {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return nil, "", fmt.Errorf("platform: rand: %w", err)
		}
		path := filepath.Join(dir, fmt.Sprintf("gs-app-%x.sock", b))
		ln, err := net.Listen("unix", path)
		if err != nil {
			if errors.Is(err, syscall.EADDRINUSE) && i < attempts-1 {
				continue
			}
			return nil, "", fmt.Errorf("platform: listen child unix %s: %w", path, err)
		}
		if err := os.Chmod(path, 0o600); err != nil {
			ln.Close()
			_ = os.Remove(path)
			return nil, "", err
		}
		return ln, "unix://" + path, nil
	}
	return nil, "", fmt.Errorf("platform: listen child unix: exhausted name retries")
}

// ChildIPC returns a pre-connected anonymous IPC channel for a sandboxed
// child process. parent is the runtime's end of the channel; child is an
// open file that must be passed to the child via exec.Cmd.ExtraFiles (it
// becomes fd 3 there). Unlike a socket-path listener, the channel exists
// before spawn and needs no filesystem endpoint, so a confined child can
// reach the runtime without any network-outbound capability — a Seatbelt or
// namespace policy that denies socket connect() still allows I/O on the
// inherited descriptor.
//
// The caller should close child once exec.Cmd.Start has succeeded: the child
// then owns the only peer copy, and its exit yields EOF on parent.
func ChildIPC() (parent net.Conn, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("platform: socketpair: %w", err)
	}
	parentFile := os.NewFile(uintptr(fds[0]), "gostalgia-child-ipc")
	child = os.NewFile(uintptr(fds[1]), "gostalgia-child-ipc-peer")
	if parentFile == nil || child == nil {
		_ = parentFile.Close()
		if child != nil {
			_ = child.Close()
		} else {
			_ = syscall.Close(fds[1])
		}
		return nil, nil, fmt.Errorf("platform: socketpair: invalid fd")
	}
	// net.FileConn dups the descriptor; close the raw parent fd afterwards.
	parent, err = net.FileConn(parentFile)
	_ = parentFile.Close()
	if err != nil {
		_ = child.Close()
		return nil, nil, fmt.Errorf("platform: socketpair fileconn: %w", err)
	}
	return parent, child, nil
}
