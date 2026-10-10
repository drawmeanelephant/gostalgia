//go:build linux

package platform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	linuxRLimitCPU    = 0
	linuxRLimitNProc  = 6
	linuxRLimitNoFile = 7
	linuxRLimitAS     = 9
)

// sandboxInitEnv carries the serialized confinement program to the re-exec'd
// init helper running inside the new namespaces (see linuxSandboxInit).
const sandboxInitEnv = "GOSTALGIA_SANDBOX_INIT"

var (
	linuxCapsOnce sync.Once
	linuxCaps     HostSecurityCapabilities
)

// GetHostSecurityCapabilities probes and returns the sandbox enforcement mechanisms
// available on this Linux host.
func GetHostSecurityCapabilities() HostSecurityCapabilities {
	linuxCapsOnce.Do(func() {
		caps := HostSecurityCapabilities{
			Platform:          "linux",
			Supported:         true,
			ResourceLimits:    true,
			DescendantControl: true,
		}

		// Check if user and net namespaces are enabled in the kernel
		if _, err := os.Stat("/proc/self/ns/user"); err != nil {
			caps.Supported = false
			caps.Reason = "user namespace unsupported by kernel"
		} else if _, err := os.Stat("/proc/self/ns/net"); err != nil {
			caps.Supported = false
			caps.Reason = "network namespace unsupported by kernel"
		} else if data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && strings.TrimSpace(string(data)) == "0" {
			caps.Supported = false
			caps.Reason = "unprivileged user namespaces disabled by sysctl"
		} else if data, err := os.ReadFile("/proc/sys/kernel/apparmor_restrict_unprivileged_userns"); err == nil && strings.TrimSpace(string(data)) == "1" {
			caps.Supported = false
			caps.Reason = "unprivileged user namespaces restricted by AppArmor"
		}

		// Active probe: verify that unprivileged user namespace clone is permitted on this host.
		if caps.Supported {
			sh, err := exec.LookPath("sh")
			if err != nil {
				sh = "/bin/sh"
			}
			cmd := exec.Command(sh, "-c", "exit 0")
			cmd.SysProcAttr = &syscall.SysProcAttr{
				Cloneflags: syscall.CLONE_NEWUSER,
			}
			if err := cmd.Run(); err != nil {
				caps.Supported = false
				caps.Reason = fmt.Sprintf("unprivileged user namespace restricted by host: %v", err)
			}
		}

		if caps.Supported {
			caps.NetworkIsolation = true
			caps.FilesystemSandbox = true
		}

		linuxCaps = caps
	})
	return linuxCaps
}

func setPrlimit(pid int, resource int, limit uint64) error {
	if limit == 0 {
		return nil
	}
	var rlim syscall.Rlimit
	rlim.Cur = limit
	rlim.Max = limit
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PRLIMIT64,
		uintptr(pid),
		uintptr(resource),
		uintptr(unsafe.Pointer(&rlim)),
		0, 0, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// ConfigureSandbox sets up host-level confinement for cmd on Linux.
func ConfigureSandbox(cmd *exec.Cmd, policy ExecutionPolicy) (PostStartHook, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if policy.Isolation == "" || policy.Isolation == IsolationTrusted {
		return nil, nil
	}

	caps := GetHostSecurityCapabilities()
	if !caps.Supported && (policy.Isolation == IsolationSandbox || policy.Isolation == IsolationStrict || policy.DenyNetwork) {
		return nil, fmt.Errorf("%w: %s", ErrSandboxUnsupported, caps.Reason)
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL

	sandboxed := policy.Isolation == IsolationSandbox || policy.Isolation == IsolationStrict
	if sandboxed || policy.DenyNetwork {
		cmd.SysProcAttr.Unshareflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	}
	if policy.DenyNetwork {
		cmd.SysProcAttr.Unshareflags |= syscall.CLONE_NEWNET
	}

	if sandboxed {
		// Filesystem confinement needs mount(2) calls that os/exec cannot
		// express, so the child re-executes this binary: platform's init()
		// hook (linuxSandboxInit) runs inside the fresh mount namespace,
		// applies the policy mounts, and then execs the real argv.
		cmd.SysProcAttr.Unshareflags |= syscall.CLONE_NEWNS
		cfg := sandboxInitConfig{
			Path:         cmd.Path,
			Argv:         append([]string(nil), cmd.Args...),
			ReadOnlyFS:   policy.ReadOnlyFS,
			AllowedPaths: append([]string(nil), policy.AllowedPaths...),
			MaskedPaths:  append([]string(nil), policy.MaskedPaths...),
			DenyNetwork:  policy.DenyNetwork,
		}
		payload, err := json.Marshal(cfg)
		if err != nil {
			return nil, fmt.Errorf("platform: encode sandbox init: %w", err)
		}
		cmd.Env = append(cmd.Env, sandboxInitEnv+"="+base64.StdEncoding.EncodeToString(payload))
		cmd.Path = "/proc/self/exe"
		cmd.Args = []string{"gostalgia-sandbox-init"}
	}

	postHook := func(pid int) error {
		if pid <= 0 {
			return nil
		}
		if policy.MaxMemoryBytes > 0 {
			if err := setPrlimit(pid, linuxRLimitAS, policy.MaxMemoryBytes); err != nil {
				return fmt.Errorf("platform: set RLIMIT_AS: %w", err)
			}
		}
		if policy.MaxOpenFiles > 0 {
			if err := setPrlimit(pid, linuxRLimitNoFile, policy.MaxOpenFiles); err != nil {
				return fmt.Errorf("platform: set RLIMIT_NOFILE: %w", err)
			}
		}
		if policy.MaxCPUSeconds > 0 {
			if err := setPrlimit(pid, linuxRLimitCPU, policy.MaxCPUSeconds); err != nil {
				return fmt.Errorf("platform: set RLIMIT_CPU: %w", err)
			}
		}
		if policy.MaxProcesses > 0 {
			if err := setPrlimit(pid, linuxRLimitNProc, policy.MaxProcesses); err != nil {
				return fmt.Errorf("platform: set RLIMIT_NPROC: %w", err)
			}
		}
		return nil
	}

	return postHook, nil
}

// sandboxInitConfig is serialized into sandboxInitEnv and interpreted by the
// init() hook of the re-executed process image.
type sandboxInitConfig struct {
	Path         string   `json:"path"`
	Argv         []string `json:"argv"`
	ReadOnlyFS   bool     `json:"read_only_fs,omitempty"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
	MaskedPaths  []string `json:"masked_paths,omitempty"`
	DenyNetwork  bool     `json:"deny_network,omitempty"`
}

func init() { linuxSandboxInit() }

// linuxSandboxInit is the re-exec hook. It runs in every binary that links
// this package (runtime, test binaries, helpers), but is a no-op unless the
// parent staged a sandbox init payload in the environment. When staged, it
// applies the mount program inside the already-created user/mount/network
// namespaces and execs the real target — it never returns to main.
func linuxSandboxInit() {
	enc := os.Getenv(sandboxInitEnv)
	if enc == "" {
		return
	}
	os.Unsetenv(sandboxInitEnv) // never leak the payload into the target
	var cfg sandboxInitConfig
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || json.Unmarshal(raw, &cfg) != nil || cfg.Path == "" {
		fmt.Fprintln(os.Stderr, "platform: invalid sandbox init payload")
		os.Exit(126)
	}
	if len(cfg.Argv) == 0 {
		cfg.Argv = []string{cfg.Path}
	}
	if err := linuxApplySandboxMounts(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "platform: sandbox init: %v\n", err)
		os.Exit(126)
	}
	if err := syscall.Exec(cfg.Path, cfg.Argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "platform: sandbox exec %s: %v\n", cfg.Path, err)
		os.Exit(127)
	}
}

// Directories that commonly host unix sockets (agents, docker, runtime IPC)
// or host scratch state. They are covered with fresh tmpfs mounts when the
// child is denied network access or confined to a read-only filesystem, so
// no host socket node remains reachable by path.
var linuxHideDirs = []string{"/tmp", "/var/tmp", "/run", "/var/run"}

// Pseudo filesystems that must never be remounted read-only: they carry no
// host file data, and several (proc, sysfs, devtmpfs) break or misbehave if
// flipped.
var linuxPseudoFSTypes = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "cgroup": true, "cgroup2": true, "pstore": true,
	"bpf": true, "tracefs": true, "debugfs": true, "configfs": true,
	"fusectl": true, "securityfs": true, "selinuxfs": true, "mqueue": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true, "nsfs": true,
	"ramfs": true, "efivarfs": true, "fuse.portal": true, "fuse.gvfsd-fuse": true,
}

func linuxApplySandboxMounts(cfg sandboxInitConfig) error {
	// Stop mount propagation so our changes never escape this namespace.
	if err := syscall.Mount("none", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}

	if cfg.ReadOnlyFS {
		if err := linuxRemountReadOnly(); err != nil {
			return err
		}
	}

	// Fresh scratch/tmpfs mounts hide host unix sockets and any host temp
	// content, and keep a writable /tmp under strict read-only policy.
	if cfg.DenyNetwork || cfg.ReadOnlyFS {
		for _, dir := range linuxHideDirs {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				if err := syscall.Mount("tmpfs", dir, "tmpfs", 0, "mode=1777"); err != nil {
					return fmt.Errorf("mask %s: %w", dir, err)
				}
			}
		}
	}

	// Mask sensitive host paths (environment root, runtime.json, ...).
	for _, m := range cfg.MaskedPaths {
		if m == "" {
			continue
		}
		p := m
		if resolved, err := filepath.EvalSymlinks(m); err == nil {
			p = resolved
		}
		st, err := os.Stat(p)
		if err != nil {
			continue // nothing to hide
		}
		if st.IsDir() {
			if err := syscall.Mount("tmpfs", p, "tmpfs", 0, "mode=0700"); err != nil {
				return fmt.Errorf("mask dir %s: %w", m, err)
			}
		} else {
			if err := syscall.Mount("/dev/null", p, "", syscall.MS_BIND, ""); err != nil {
				return fmt.Errorf("mask file %s: %w", m, err)
			}
		}
	}

	// Operator-granted paths stay reachable and keep their normal writability
	// even under a read-only policy.
	for _, p := range cfg.AllowedPaths {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := syscall.Mount(p, p, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("allow path %s: %w", p, err)
		}
		if cfg.ReadOnlyFS {
			if err := syscall.Mount("", p, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
				return fmt.Errorf("rw allow path %s: %w", p, err)
			}
		}
	}

	return nil
}

// linuxRemountReadOnly bind-remounts every real mount read-only. Pseudo
// filesystems are skipped: they hold no host file contents, and remounting
// proc/sysfs/devtmpfs would break the confined process.
func linuxRemountReadOnly() error {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return fmt.Errorf("read mounts: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mountPoint := strings.ReplaceAll(fields[1], "\\040", " ")
		fstype := fields[2]
		if linuxPseudoFSTypes[fstype] {
			continue
		}
		if err := syscall.Mount("", mountPoint, "", syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("remount %s read-only: %w", mountPoint, err)
		}
	}
	return nil
}

// KillDescendants kills unauthorized descendants of pid. It sweeps /proc once
// and kills two sets:
//
//  1. every member of the child's process group (except the group leader
//     itself) — descendants inherit the group, so this catches reparented
//     grandchildren and fork+exit races that a parent/children walk misses;
//  2. the transitive parent→child descendants of pid — this catches
//     descendants that escaped the process group via setpgid/setsid but
//     remain inside the ancestry tree.
//
// Residual: a descendant that both leaves the process group and orphans
// itself within one supervisor poll can evade tracking until teardown —
// KillProcessTree SIGKILLs the whole group on stop, but a fully escaped,
// daemonized straggler is out of reach without seccomp/LSM confinement.
// PR_SET_CHILD_SUBREAPER is deliberately not used: reparenting managed
// children to the runtime would race os/exec's wait and orphan zombies.
func KillDescendants(pid int) {
	if pid <= 0 {
		return
	}
	leaderStat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return
	}
	_, pgid, leaderStart, ok := linuxParseProcStat(string(leaderStat))
	if !ok {
		return
	}

	type procEnt struct {
		ppid  int
		pgid  int
		start uint64
	}
	procs := make(map[int]procEnt)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		p, err := strconv.Atoi(e.Name())
		if err != nil || p <= 0 {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p))
		if err != nil {
			continue
		}
		ppid, pg, start, ok := linuxParseProcStat(string(data))
		if ok {
			procs[p] = procEnt{ppid: ppid, pgid: pg, start: start}
		}
	}

	// Transitive descendants of pid by ppid (covers group escapees still in
	// the ancestry tree).
	inTree := map[int]bool{}
	queue := []int{pid}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for p, st := range procs {
			if st.ppid == cur && !inTree[p] {
				inTree[p] = true
				queue = append(queue, p)
			}
		}
	}

	// The /proc scan above is already stale: the leader can exit and its
	// pid — hence its pgid — can be recycled into an unrelated process
	// group before the sweep below runs. The pgid match is trusted only
	// while the group provably still belongs to this child: the child is
	// its own group leader (pgid == pid via Setpgid), and either the
	// process now at that pid is still our leader (same start time, so not
	// a recycled pid) or the leader slot is empty — a dead leader means the
	// pgid could not have been reissued while members survive, so remaining
	// members are stragglers of the group the child created.
	groupSweep := pgid == pid
	if groupSweep {
		if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			_, _, start, ok := linuxParseProcStat(string(stat))
			groupSweep = ok && start == leaderStart
		}
	}

	for p, st := range procs {
		if p == pid || !(inTree[p] || (groupSweep && st.pgid == pgid)) {
			continue
		}
		// The candidate's own pid may also have been recycled since the
		// scan; require its start time to still match before signaling.
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", p))
		if err != nil {
			continue
		}
		_, _, start, ok := linuxParseProcStat(string(stat))
		if !ok || start != st.start {
			continue
		}
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}
