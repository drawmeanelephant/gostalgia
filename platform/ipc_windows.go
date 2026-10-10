//go:build windows

package platform

import (
	"fmt"
	"net"
	"os"
)

// ListenIPC opens the environment's local IPC listener. Windows lacks a
// uniformly available AF_UNIX story, so the listener binds loopback TCP on
// an ephemeral port. The endpoint and token recorded in runtime.json keep
// this local-only in practice; named pipes are the migration path once a
// dependency for them is justified.
func ListenIPC(root string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("platform: listen loopback: %w", err)
	}
	return ln, "tcp://" + ln.Addr().String(), nil
}

// IPCSocketDir returns "" on Windows: IPC endpoints are loopback TCP, so
// there is no filesystem socket directory for a sandbox policy to mask.
func IPCSocketDir() string { return "" }

// ListenChildIPC creates a dedicated loopback listener for a child application process.
func ListenChildIPC(appID string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("platform: listen child loopback: %w", err)
	}
	return ln, "tcp://" + ln.Addr().String(), nil
}

// ChildIPC is unsupported on Windows: there is no way to pass a pre-connected
// socket descriptor to a child process. Sandbox isolation also fails closed on
// Windows, so this is unreachable in practice.
func ChildIPC() (net.Conn, *os.File, error) {
	return nil, nil, fmt.Errorf("%w: inherited-descriptor child IPC is unsupported on Windows", ErrSandboxUnsupported)
}
