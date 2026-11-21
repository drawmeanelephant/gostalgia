//go:build windows

package platform

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
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

// kernel32 calls not exposed by the stdlib syscall package. Core packages
// stay inside the stdlib boundary (test/e2e TestCoreDependencyBoundary),
// so job objects are driven through LazyDLL rather than x/sys/windows.
var (
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procCreateJobObject          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
)

const (
	jobObjectInfoClassExtendedLimit = 9
	jobObjectLimitKillOnJobClose    = 0x00002000
	processSetQuota                 = 0x0100
	processTerminate                = 0x0001
)

// JOBOBJECT_BASIC_LIMIT_INFORMATION, IO_COUNTERS and
// JOBOBJECT_EXTENDED_LIMIT_INFORMATION laid out to match the Win32
// structs: Go's natural field alignment on 64-bit builds produces the
// same layout (64 + 48 + 32 = 144 bytes). Only LimitFlags is ever set.
type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInformation struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// SetupProcessTree configures cmd attributes on Windows.
func SetupProcessTree(cmd *exec.Cmd) {
	// Job-object enrollment happens in EnrollProcessTree after Start;
	// there is no pre-start attribute equivalent to Unix setpgid here.
}

func createJobObject() (syscall.Handle, error) {
	h, _, err := procCreateJobObject.Call(0, 0)
	if h == 0 {
		return 0, err
	}
	return syscall.Handle(h), nil
}

func setJobLimits(job syscall.Handle, limits *jobObjectExtendedLimitInformation) error {
	r, _, err := procSetInformationJobObject.Call(
		uintptr(job),
		jobObjectInfoClassExtendedLimit,
		uintptr(unsafe.Pointer(limits)),
		unsafe.Sizeof(*limits),
	)
	if r == 0 {
		return err
	}
	return nil
}

// EnrollProcessTree places the started child into a dedicated Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so teardown covers the whole
// tree including grandchildren. Called immediately after cmd.Start.
func EnrollProcessTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	job, err := createJobObject()
	if err != nil {
		return err
	}
	var limits jobObjectExtendedLimitInformation
	limits.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	if err := setJobLimits(job, &limits); err != nil {
		_ = syscall.CloseHandle(job)
		return err
	}
	h, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		_ = syscall.CloseHandle(job)
		return err
	}
	defer syscall.CloseHandle(h)
	r, _, aerr := procAssignProcessToJobObject.Call(uintptr(job), uintptr(h))
	if r == 0 {
		_ = syscall.CloseHandle(job)
		return aerr
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
		job := v.(syscall.Handle)
		_, _, _ = procTerminateJobObject.Call(uintptr(job), 1)
		_ = syscall.CloseHandle(job)
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
