package services

// Audit fix #161: pin the intended capability contract for proc/stop,
// app/stop and sys/shutdown (manifest-grantable, not admin-only), and verify
// proc/stop's timeout_seconds is bounded.

import (
	"context"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/process"
	"gostalgia/internal/security"
)

func TestAuditProcStopTimeoutSecondsClamped(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()

	old := maxProcStopTimeout
	maxProcStopTimeout = 50 * time.Millisecond
	defer func() { maxProcStopTimeout = old }()

	// An in-proc process that ignores its cancellation long enough to
	// outlive the clamped stop timeout.
	stubborn, err := env.ctx.Procs.StartInProc(context.Background(), process.Spec{
		Name: "stubborn",
	}, func(p *process.Process) error {
		time.Sleep(500 * time.Millisecond)
		return nil
	})
	must(t, err)

	start := time.Now()
	resp := env.call(context.Background(), admin, "proc/stop", map[string]any{
		"id":              int(stubborn.ID()),
		"timeout_seconds": 3600,
	})
	elapsed := time.Since(start)
	if resp.OK {
		t.Fatal("proc/stop unexpectedly succeeded on a process that ignores cancellation")
	}
	if !strings.Contains(resp.Error, "did not stop within 50ms") {
		t.Fatalf("expected clamped-timeout error, got: %s", resp.Error)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("proc/stop parked the handler for %s — timeout_seconds was not clamped", elapsed)
	}
}

// TestAuditProcStopShutdownCapabilityContract pins the intended trust model:
// proc.stop and shutdown are app-declarable capabilities, not admin-only —
// the gate is the manifest grant itself (see docs/security.md §2).
func TestAuditProcStopShutdownCapabilityContract(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	proc, err := env.ctx.Apps.Launch(ctx, "com.gostalgia.echo")
	must(t, err)
	t.Cleanup(func() { _ = env.ctx.Apps.Stop("com.gostalgia.echo", time.Second) })

	// Without proc.stop the route is denied.
	resp := env.call(ctx, security.NewCapabilities(security.CapProcList), "proc/stop", map[string]any{
		"id": int(proc.ID()),
	})
	if resp.OK || !strings.Contains(resp.Error, "permission denied") {
		t.Fatalf("proc/stop without proc.stop: OK=%v err=%q", resp.OK, resp.Error)
	}

	// With proc.stop, an app principal may stop any runtime-managed
	// process — there is deliberately no ownership scoping.
	resp = env.call(ctx, security.NewCapabilities(security.CapProcStop), "proc/stop", map[string]any{
		"id": int(proc.ID()),
	})
	if !resp.OK {
		t.Fatalf("proc/stop with proc.stop denied: %s", resp.Error)
	}

	// sys/shutdown is likewise gated only by the shutdown capability: an
	// app holding it can stop the environment.
	resp = env.call(ctx, security.NewCapabilities(security.CapShutdown), "sys/shutdown", map[string]any{"reason": "contract-test"})
	if !resp.OK {
		t.Fatalf("sys/shutdown with shutdown denied: %s", resp.Error)
	}
	resp = env.call(ctx, security.NewCapabilities(security.CapProcStop), "sys/shutdown", map[string]any{"reason": "contract-test"})
	if resp.OK || !strings.Contains(resp.Error, "permission denied") {
		t.Fatalf("sys/shutdown without shutdown: OK=%v err=%q", resp.OK, resp.Error)
	}
}
