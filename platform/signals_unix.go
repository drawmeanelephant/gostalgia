//go:build unix

package platform

import (
	"os"
	"syscall"
)

// ShutdownSignals returns the host signals that request a graceful
// environment shutdown on unix-like hosts: Ctrl-C, the conventional
// kill TERM, and terminal hangup (SIGHUP — its default disposition would
// otherwise kill the daemon outright, orphaning supervised children).
// Callers pass the result to os/signal.
func ShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
}
