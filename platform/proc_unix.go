//go:build unix

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// SetupProcessTree configures cmd so that it and any child processes it spawns
// run in their own process group, enabling clean process-tree termination.
func SetupProcessTree(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessTree kills cmd and all its descendant processes by sending
// SIGKILL to the process group.
//
// The group signal is only sent while the group provably still belongs to
// cmd's child — a pid (and therefore the pgid the child created with
// setpgid) can be recycled into an unrelated process group once the child
// is reaped, and a blind kill(-pid) would then SIGKILL strangers:
//
//   - While the child is alive or an unreaped zombie the group is
//     provably ours. cmd.Process.Signal(0) consults os.Process's internal
//     done state, so this stays race-free against a concurrent Wait even
//     if the pid was recycled after reaping — a done process never probes
//     the pid again.
//   - After reaping, the group may persist without its leader (stray
//     grandchildren hold the pgid). It is only signaled when no process
//     currently holds the leader's pid: a group whose leader is alive at
//     that pid could be a recycled foreign group, which must survive.
//
// Residual: a recycled group whose leader has itself already died is
// indistinguishable from our stragglers; closing that last window needs
// pidfds, which the stdlib does not expose.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if pid > 0 && processGroupOwned(cmd, pid) {
		// Send SIGKILL to the entire process group (-pid).
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	return cmd.Process.Kill()
}

// processGroupOwned reports whether the process group pid is still the one
// created by cmd's child, so that kill(-pid) cannot hit a recycled group.
func processGroupOwned(cmd *exec.Cmd, pid int) bool {
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		// Child alive or unreaped zombie: its pid — and pgid — cannot be
		// recycled, so the group is provably ours.
		return true
	}
	// Child reaped: only a group outliving a freed leader pid is ours.
	// ESRCH on the pid means nothing currently holds it (a recycled group
	// leader would), while a live group means stragglers remain.
	return syscall.Kill(pid, 0) == syscall.ESRCH &&
		syscall.Kill(-pid, 0) == nil
}

// SampleProcessResources samples resource usage for an active process by PID.
// Unsupported metrics or platforms return Supported: false.
func SampleProcessResources(pid int) ResourceUsage {
	if pid <= 0 {
		return ResourceUsage{Supported: false}
	}

	// Try reading Linux /proc/<pid>/statm and /proc/<pid>/stat
	usage, ok := readLinuxProc(pid)
	if ok {
		return usage
	}

	// Non-Linux Unix or unsupported procfs: report unavailable
	return ResourceUsage{Supported: false}
}

func readLinuxProc(pid int) (ResourceUsage, bool) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return ResourceUsage{}, false
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return ResourceUsage{}, false
	}
	pages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return ResourceUsage{}, false
	}
	pageSize := uint64(os.Getpagesize())
	rssBytes := pages * pageSize

	var userMs, sysMs int64
	if statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		statFields := strings.Fields(string(statData))
		if len(statFields) >= 15 {
			if u, err := strconv.ParseInt(statFields[13], 10, 64); err == nil {
				userMs = linuxTicksToMs(u, linuxClockTicks())
			}
			if s, err := strconv.ParseInt(statFields[14], 10, 64); err == nil {
				sysMs = linuxTicksToMs(s, linuxClockTicks())
			}
		}
	}

	return ResourceUsage{
		Supported:   true,
		MemoryBytes: rssBytes,
		CPUUserMs:   userMs,
		CPUSysMs:    sysMs,
	}, true
}

// processStateMemory reports the reaped child's peak RSS from its rusage.
// ru_maxrss is kilobytes on Linux and bytes on darwin.
func processStateMemory(ps *os.ProcessState) (uint64, bool) {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil || ru.Maxrss <= 0 {
		return 0, false
	}
	if runtime.GOOS == "darwin" {
		return uint64(ru.Maxrss), true
	}
	return uint64(ru.Maxrss) * 1024, true
}

var (
	linuxClkTckOnce sync.Once
	linuxClkTck     int64
)

// linuxClockTicks returns the user-visible clock-tick rate (USER_HZ) that
// /proc stat time fields are reported in, resolved once via getconf(1).
// The fallback is 100, the value on effectively every Linux host; the
// lookup exists for kernels or emulated environments where it differs.
func linuxClockTicks() int64 {
	linuxClkTckOnce.Do(func() {
		linuxClkTck = 100
		out, err := exec.Command("getconf", "CLK_TCK").Output()
		if err != nil {
			return
		}
		if v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil && v > 0 {
			linuxClkTck = v
		}
	})
	return linuxClkTck
}

// linuxTicksToMs converts kernel clock ticks to milliseconds.
func linuxTicksToMs(ticks, hz int64) int64 {
	if hz <= 0 {
		return 0
	}
	return ticks * 1000 / hz
}

// ProcessAlive checks if a process with the given PID is still alive.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
