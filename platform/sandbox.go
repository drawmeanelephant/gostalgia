package platform

import (
	"errors"
)

// Standard isolation level identifiers.
const (
	IsolationInProc  = "inproc"
	IsolationTrusted = "trusted"
	IsolationSandbox = "sandbox"
	IsolationStrict  = "strict"
)

// ErrSandboxUnsupported is returned when host-level sandbox enforcement
// is requested on a platform or configuration that cannot enforce it.
var ErrSandboxUnsupported = errors.New("platform: host sandbox enforcement is unsupported")

// sandboxInitEnv carries the serialized confinement program to a re-exec'd
// init helper. Platforms that cannot express a confinement step through
// os/exec (Linux namespace mounts, Darwin pre-exec rlimits) re-execute the
// current binary with this payload; the platform init hook decodes it,
// applies the confinement, and execs the real target.
const sandboxInitEnv = "GOSTALGIA_SANDBOX_INIT"

// ExecutionPolicy defines the host confinement guarantees applied to a process.
type ExecutionPolicy struct {
	Isolation       string   `json:"isolation"`                  // "trusted", "sandbox", "strict"
	DenyNetwork     bool     `json:"deny_network"`               // block all host network egress, including remote unix sockets
	DenyDescendants bool     `json:"deny_descendants"`           // block or immediately kill descendant processes
	ReadOnlyFS      bool     `json:"read_only_fs"`               // restrict host filesystem writes
	AllowedPaths    []string `json:"allowed_paths,omitempty"`    // permitted filesystem paths (read; write unless ReadOnlyFS)
	MaskedPaths     []string `json:"masked_paths,omitempty"`     // host paths hidden from sandboxed children (e.g. env root holding runtime.json)
	MaxMemoryBytes  uint64   `json:"max_memory_bytes,omitempty"` // maximum address space/memory limit in bytes
	MaxCPUSeconds   uint64   `json:"max_cpu_seconds,omitempty"`  // maximum CPU time limit in seconds
	MaxOpenFiles    uint64   `json:"max_open_files,omitempty"`   // maximum open file descriptors
	MaxProcesses    uint64   `json:"max_processes,omitempty"`    // maximum child processes/threads
}

// HostSecurityCapabilities describes which sandbox enforcement mechanisms
// are supported by the current OS and host configuration.
type HostSecurityCapabilities struct {
	Platform          string `json:"platform"`
	Supported         bool   `json:"supported"`
	NetworkIsolation  bool   `json:"network_isolation"`
	ResourceLimits    bool   `json:"resource_limits"`
	DescendantControl bool   `json:"descendant_control"`
	FilesystemSandbox bool   `json:"filesystem_sandbox"`
	Reason            string `json:"reason,omitempty"`
}

// PostStartHook is an optional callback executed immediately after process
// creation (pid allocated) to apply kernel-level restrictions (e.g. prlimit64).
type PostStartHook func(pid int) error

// DefaultSandboxPolicy returns the standard sandbox policy for external apps.
func DefaultSandboxPolicy() ExecutionPolicy {
	return ExecutionPolicy{
		Isolation:       IsolationSandbox,
		DenyNetwork:     true,
		DenyDescendants: false,
		ReadOnlyFS:      false,
		MaxMemoryBytes:  2 * 1024 * 1024 * 1024, // 2 GB virtual memory address space for 64-bit runtime
		MaxOpenFiles:    1024,
	}
}

// DefaultStrictPolicy returns the strict sandbox policy for external apps.
func DefaultStrictPolicy() ExecutionPolicy {
	return ExecutionPolicy{
		Isolation:       IsolationStrict,
		DenyNetwork:     true,
		DenyDescendants: true,
		ReadOnlyFS:      true,
		MaxMemoryBytes:  2 * 1024 * 1024 * 1024, // 2 GB virtual memory address space for 64-bit runtime
		MaxOpenFiles:    512,
	}
}

// Validate checks that the execution policy has a valid isolation level and parameters.
func (p ExecutionPolicy) Validate() error {
	switch p.Isolation {
	case "", IsolationInProc, IsolationTrusted, IsolationSandbox, IsolationStrict:
		return nil
	default:
		return errors.New("platform: unknown isolation level: " + p.Isolation)
	}
}

// PolicyForIsolation resolves an isolation level into a concrete ExecutionPolicy.
func PolicyForIsolation(isolation string) (ExecutionPolicy, error) {
	switch isolation {
	case "", IsolationTrusted:
		return ExecutionPolicy{Isolation: IsolationTrusted}, nil
	case IsolationSandbox:
		return DefaultSandboxPolicy(), nil
	case IsolationStrict:
		return DefaultStrictPolicy(), nil
	default:
		return ExecutionPolicy{}, errors.New("platform: unknown isolation level: " + isolation)
	}
}
