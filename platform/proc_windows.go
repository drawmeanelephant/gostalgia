//go:build windows

package platform

import (
	"os"
	"os/exec"
	"syscall"
)

// SetupProcessTree configures cmd attributes on Windows.
func SetupProcessTree(cmd *exec.Cmd) {
	// Standard Windows process hierarchy.
}

// KillProcessTree kills cmd on Windows.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// SampleProcessResources samples resource usage for an active process by PID on Windows.
func SampleProcessResources(pid int) ResourceUsage {
	return ResourceUsage{Supported: false}
}

// processStateMemory is unavailable on Windows: the rusage behind
// ProcessState.SysUsage carries no RSS, so memory stays unreported.
func processStateMemory(ps *os.ProcessState) (uint64, bool) {
	return 0, false
}

const (
	winProcessQueryLimitedInformation = 0x1000
	winStillActive                    = 259
)

// ProcessAlive checks if a process with the given PID is still alive.
// OpenProcess fails for dead or nonexistent pids; GetExitCodeProcess
// distinguishes a live process from an exited one whose handle remains
// open (a terminated process still yields an exit code, STILL_ACTIVE
// marks the running case).
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(winProcessQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == winStillActive
}
