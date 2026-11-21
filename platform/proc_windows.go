//go:build windows

package platform

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows process-tree teardown uses a Job Object per child: the child and
// every descendant it spawns are confined to a job created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so terminating the job takes the
// whole tree. Closing the job handle is also the last-resort teardown —
// an exited runtime's handles are closed by the OS, so a dead supervisor
// can never orphan grandchildren.
//
// The assignment race is bounded, not eliminated: a child that spawns a
// descendant in the window between cmd.Start and EnrollProcessTree can
// leak that one descendant. Closing over it needs a suspended create,
// which os/exec does not expose; the window is a few instructions wide
// and every stop/shutdown path still terminates the job itself.
var childJobs sync.Map // pid -> job handle

// SetupProcessTree configures cmd attributes on Windows.
func SetupProcessTree(cmd *exec.Cmd) {
	// Job-object enrollment happens in EnrollProcessTree after Start;
	// there is no pre-start attribute equivalent to Unix setpgid here.
}

// EnrollProcessTree places the started child into a dedicated Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so teardown covers the whole
// tree including grandchildren. Called immediately after cmd.Start.
func EnrollProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		_ = windows.CloseHandle(job)
		return err
	}
	childJobs.Store(pid, job)
	return nil
}

// TerminateProcessTree has no graceful group signal on Windows: console
// control events are not available to detached children. Stop's graceful
// phase for external apps is the IPC conn close; escalation on deadline
// kills the job, which takes the whole tree.
func TerminateProcessTree(cmd *exec.Cmd) error {
	return nil
}

// KillProcessTree kills cmd's entire process tree on Windows by
// terminating its Job Object. Children spawned before EnrollProcessTree
// ran (a few instructions of startup) are outside the job and survive;
// every later descendant is confined.
func KillProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if v, ok := childJobs.LoadAndDelete(pid); ok {
		job := v.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		_ = windows.CloseHandle(job)
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
