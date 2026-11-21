//go:build !unix && !windows

package platform

import (
	"os"
	"os/exec"
)

// processStateMemory is unavailable on platforms without rusage behind
// ProcessState.SysUsage; memory stays unreported.
func processStateMemory(ps *os.ProcessState) (uint64, bool) {
	return 0, false
}

// TerminateProcessTree has no graceful group signal on platforms without
// Unix process groups; Stop escalates to KillProcessTree on its deadline.
func TerminateProcessTree(cmd *exec.Cmd) error {
	return nil
}

// EnrollProcessTree performs post-start tree registration; platforms
// without job objects or process groups have nothing to enroll.
func EnrollProcessTree(cmd *exec.Cmd) error {
	return nil
}
