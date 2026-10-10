//go:build unix

package platform

import (
	"os"
	"syscall"
	"testing"
)

func TestShutdownSignals(t *testing.T) {
	got := ShutdownSignals()
	want := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
	if len(got) != len(want) {
		t.Fatalf("ShutdownSignals() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ShutdownSignals()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
