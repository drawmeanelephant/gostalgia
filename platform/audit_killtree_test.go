//go:build unix

package platform

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestAuditKillProcessTreeRecycledGroup is the deterministic reproducer for
// the recycled-pgid hazard of issue #147: after the supervised child is
// reaped, its pid is burned back into circulation as the leader of an
// unrelated process group, and the exact post-Wait KillProcessTree call
// that internal/process makes must not SIGKILL that group. Forcing
// wraparound takes ~100k forks (minutes), so it is skipped unless
// AUDIT_PID_WRAPAROUND=1.
func TestAuditKillProcessTreeRecycledGroup(t *testing.T) {
	if os.Getenv("AUDIT_PID_WRAPAROUND") == "" {
		t.Skip("set AUDIT_PID_WRAPAROUND=1 to run the pid-recycling repro")
	}

	// The supervised child: own process group, exits immediately and is
	// reaped by Wait, freeing both its pid and its pgid.
	cmd := exec.Command("sh", "-c", "exit 0")
	SetupProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	deadPid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	t.Logf("supervised child pid=%d exited and was reaped; its pgid %d is now free", deadPid, deadPid)

	// Burn pids until one is reissued to a new process-group leader.
	var victim *exec.Cmd
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) && victim == nil {
		c := exec.Command("sleep", "120")
		SetupProcessTree(c)
		if err := c.Start(); err != nil {
			continue
		}
		if c.Process.Pid == deadPid {
			victim = c
			break
		}
		_ = c.Process.Kill()
		_ = c.Wait()
	}
	if victim == nil {
		t.Fatalf("pid %d was not reissued before the deadline", deadPid)
	}
	defer func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	}()
	t.Logf("victim pid=%d is now group leader of recycled pgid %d", deadPid, deadPid)

	// The exact call internal/process makes after cmd.Wait(). Its error is
	// ignored there too (the child is already reaped, so Process.Kill
	// reports ErrProcessDone).
	_ = KillProcessTree(cmd)

	done := make(chan error, 1)
	go func() { done <- victim.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("BUG: KillProcessTree killed unrelated recycled group pgid %d: victim exited with %v", deadPid, err)
	case <-time.After(300 * time.Millisecond):
		// Victim survived: the stale group signal was suppressed.
	}
}

// TestKillProcessTreeSweepsStragglerGroup verifies the ownership guard does
// not regress the descendant sweep: after the leader exits and is reaped,
// a grandchild still holding the process group is SIGKILLed.
func TestKillProcessTreeSweepsStragglerGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 & exit 0")
	SetupProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	defer func() {
		// Best-effort cleanup if the sweep under test fails.
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()

	// The backgrounded sleep inherited the child's process group; it
	// reparents but keeps pgid == pid, so the group stays non-empty.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(-pid, 0) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if syscall.Kill(-pid, 0) != nil {
		t.Skip("straggler group never materialized")
	}

	_ = KillProcessTree(cmd)

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) == syscall.ESRCH {
			return // group fully dead: the straggler was swept
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("straggler process group %d survived KillProcessTree", pid)
}

// TestKillProcessTreeReapedEmptyGroup verifies that killing a reaped child
// whose process group is already gone is a quiet no-op.
func TestKillProcessTreeReapedEmptyGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	SetupProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait child: %v", err)
	}
	if err := KillProcessTree(cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("KillProcessTree on reaped child: %v", err)
	}
}

// TestKillProcessTreeLiveGroup covers the primary contract: a live child's
// whole group is killed, leader and descendants alike.
func TestKillProcessTreeLiveGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 300 & sleep 300")
	SetupProcessTree(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()

	time.Sleep(150 * time.Millisecond)
	if err := KillProcessTree(cmd); err != nil {
		t.Fatalf("KillProcessTree: %v", err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d survived KillProcessTree", pid)
}
