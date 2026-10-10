//go:build darwin

package platform

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAuditDarwinResourceLimitsNotEnforced is the regression test for the
// audit finding that darwin advertised ResourceLimits=true while
// ConfigureSandbox silently ignored every ExecutionPolicy limit. The fix
// has two halves, both verified here:
//
//  1. Honest reporting: macOS cannot enforce MaxMemoryBytes — XNU rejects
//     setrlimit for RLIMIT_AS/RLIMIT_DATA, Seatbelt has no rlimit
//     mechanism, and there is no prlimit64 — so the capability matrix
//     reports ResourceLimits=false instead of lying.
//  2. Enforcement of the enforceable subset: sandboxed children launch
//     through a self-limiting trampoline inside the Seatbelt sandbox that
//     applies RLIMIT_NOFILE, RLIMIT_CPU, and RLIMIT_NPROC before exec'ing
//     the target.
func TestAuditDarwinResourceLimitsNotEnforced(t *testing.T) {
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}

	caps := GetHostSecurityCapabilities()
	if !caps.Supported {
		t.Skipf("host sandbox unsupported: %s", caps.Reason)
	}
	if caps.ResourceLimits {
		t.Fatal("caps.ResourceLimits=true on darwin, but MaxMemoryBytes " +
			"(RLIMIT_AS/RLIMIT_DATA) is not enforceable by XNU; the " +
			"capability matrix must not claim full resource limiting")
	}

	sandboxed := func(t *testing.T, policy ExecutionPolicy, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = os.Environ()
		hook, err := ConfigureSandbox(cmd, policy)
		if err != nil {
			t.Fatalf("ConfigureSandbox: %v", err)
		}
		if hook != nil {
			t.Fatal("darwin ConfigureSandbox must confine via the launch " +
				"trampoline, not a PostStartHook")
		}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// The launch trampoline must run inside the sandbox: the command is
	// rewritten to sandbox-exec exec'ing this binary, which applies the
	// rlimits and execs the real target.
	t.Run("launches through trampoline", func(t *testing.T) {
		cmd := exec.Command("/bin/echo", "hi")
		if _, err := ConfigureSandbox(cmd, ExecutionPolicy{
			Isolation:    IsolationSandbox,
			DenyNetwork:  true,
			MaxOpenFiles: 16,
		}); err != nil {
			t.Fatalf("ConfigureSandbox: %v", err)
		}
		if cmd.Path != "/usr/bin/sandbox-exec" {
			t.Fatalf("cmd.Path = %q, want /usr/bin/sandbox-exec", cmd.Path)
		}
		self, _ := os.Executable()
		if got := cmd.Args[len(cmd.Args)-1]; got != self {
			t.Fatalf("trampoline arg = %q, want self %q", got, self)
		}
		var staged bool
		for _, e := range cmd.Env {
			if strings.HasPrefix(e, sandboxInitEnv+"=") {
				staged = true
			}
		}
		if !staged {
			t.Fatal("no sandbox init payload staged in child env")
		}
	})

	// RLIMIT_NOFILE=16 must actually stop the confined child past 16 fds.
	t.Run("open files limit enforced", func(t *testing.T) {
		out, err := sandboxed(t, ExecutionPolicy{
			Isolation:    IsolationSandbox,
			DenyNetwork:  true,
			MaxOpenFiles: 16,
		}, "/bin/sh", "-c",
			`i=3; while [ $i -lt 40 ]; do eval "exec $i< /etc/hosts" 2>/dev/null || exit 3; i=$((i+1)); done; echo LEAK`)
		if strings.Contains(out, "LEAK") {
			t.Fatalf("child opened 40 fds under MaxOpenFiles=16: %s", out)
		}
		if err == nil {
			t.Fatalf("child exited 0 after exceeding MaxOpenFiles=16: %s", out)
		}
	})

	// RLIMIT_CPU=1 must kill a spinning confined child.
	t.Run("cpu limit enforced", func(t *testing.T) {
		out, err := sandboxed(t, ExecutionPolicy{
			Isolation:     IsolationSandbox,
			DenyNetwork:   true,
			MaxCPUSeconds: 1,
		}, "/bin/sh", "-c", `while :; do :; done; echo LEAK`)
		if strings.Contains(out, "LEAK") {
			t.Fatalf("child outlived MaxCPUSeconds=1: %s", out)
		}
		if err == nil {
			t.Fatalf("spinning child exited 0 under MaxCPUSeconds=1: %s", out)
		}
	})

	// RLIMIT_NPROC counts per-uid on XNU, so any policy below the user's
	// live process count must leave the confined child unable to fork.
	t.Run("process limit enforced", func(t *testing.T) {
		out, err := sandboxed(t, ExecutionPolicy{
			Isolation:    IsolationSandbox,
			DenyNetwork:  true,
			MaxProcesses: 8,
		}, "/bin/sh", "-c", `sh -c 'echo nested' 2>/dev/null || echo fork-denied`)
		if strings.Contains(out, "nested") {
			t.Fatalf("child forked under MaxProcesses=8: %s", out)
		}
		// XNU reports the denied fork either as a shell diagnostic
		// ("Resource temporarily unavailable") or, if the outer shell
		// survives it, the fallback marker. Unenforced launches print
		// "nested" instead.
		if err == nil && !strings.Contains(out, "fork-denied") &&
			!strings.Contains(out, "unavailable") {
			t.Fatalf("expected fork-denial evidence, exit 0: %s", out)
		}
	})

	// MaxMemoryBytes is documented as unenforceable on darwin: the launch
	// must still succeed (fail-open field, honestly reported via caps)
	// rather than die or fail closed.
	t.Run("memory limit honestly not enforced", func(t *testing.T) {
		out, err := sandboxed(t, ExecutionPolicy{
			Isolation:      IsolationSandbox,
			DenyNetwork:    true,
			MaxMemoryBytes: 1 << 20,
		}, "/usr/bin/awk", `BEGIN{a=sprintf("%*s",67108864,"x"); print "allocated", length(a)}`)
		if err != nil || !strings.Contains(out, "allocated 67108864") {
			t.Fatalf("sandboxed launch failed or memory capped (err=%v): %s", err, out)
		}
	})
}
