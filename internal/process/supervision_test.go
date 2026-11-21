package process

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func startIdleProcess(t *testing.T, m *Manager, name string) *Process {
	t.Helper()
	p, err := m.StartInProc(context.Background(), Spec{Name: name}, func(p *Process) error {
		<-p.Context().Done()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Supervisors check shutdown while holding a process lock. Table operations
// acquire the manager lock before process locks, so this check must not acquire
// the manager lock and create an inverse dependency.
func TestShutdownCheckDoesNotAcquireManagerLock(t *testing.T) {
	m, _ := newTestManager(t)
	func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		checked := make(chan bool, 1)
		go func() { checked <- m.isShuttingDown() }()
		select {
		case shuttingDown := <-checked:
			if shuttingDown {
				t.Fatal("new manager is already shutting down")
			}
		case <-time.After(time.Second):
			t.Fatal("shutdown check blocked on the manager lock")
		}
	}()
	m.StopAll(time.Second)
	if !m.isShuttingDown() {
		t.Fatal("shutdown check did not observe StopAll")
	}
}

// TestStopEscalatesToKillAfterGraceDeadline: a child that cannot observe a
// graceful terminate is escalated to a tree kill once the stop deadline
// passes — a bounded grace phase, never an open-ended wait and never an
// instant kill.
func TestStopEscalatesToKillAfterGraceDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, _ := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "term-immune",
		Args: childHelperArgs(t),
		Env:  childHelperEnv("term_immune"),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	if err := m.Stop(p.ID(), 300*time.Millisecond); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("stop returned in %s: child killed instantly, no grace phase ran", elapsed)
	}
	if info := p.Info(); info.State != StateStopped {
		t.Errorf("state = %q, want stopped after kill escalation", info.State)
	}
}

// TestStopAlreadyStoppedProcess: Stop on a process that already exited on
// its own returns cleanly instead of erroring or hanging.
func TestStopAlreadyStoppedProcess(t *testing.T) {
	m, _ := newTestManager(t)
	p, err := m.StartInProc(context.Background(), Spec{Name: "shortlived"}, func(p *Process) error {
		return nil // exit immediately
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("self-exiting process never finished")
	}
	if err := m.Stop(p.ID(), time.Second); err != nil {
		t.Fatalf("Stop on an already-stopped process: %v", err)
	}
}

// TestConcurrentStopSameProcess: concurrent Stops on one pid must all
// return promptly and leave the process stopped exactly once (run under
// -race).
func TestConcurrentStopSameProcess(t *testing.T) {
	m, _ := newTestManager(t)
	p := startIdleProcess(t, m, "contended")

	const racers = 8
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.Stop(p.ID(), 5*time.Second)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("racer %d: Stop: %v", i, err)
		}
	}
	if info := p.Info(); info.State != StateStopped {
		t.Errorf("state after concurrent stops = %q, want stopped", info.State)
	}
}

// TestStopAllOverMixedStates: StopAll stops the live processes, tolerates
// the already-stopped and failed ones, and terminates without hanging.
func TestStopAllOverMixedStates(t *testing.T) {
	m, _ := newTestManager(t)
	live := startIdleProcess(t, m, "live")

	selfExited, err := m.StartInProc(context.Background(), Spec{Name: "self-exited"}, func(p *Process) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-selfExited.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("self-exiting process never finished")
	}

	failed, err := m.StartInProc(context.Background(), Spec{Name: "failed"}, func(p *Process) error {
		return errors.New("intentional failure")
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-failed.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("failing process never finished")
	}

	done := make(chan struct{})
	go func() { m.StopAll(5 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("StopAll hung over mixed process states")
	}

	if info := live.Info(); info.State != StateStopped {
		t.Errorf("live process state = %q, want stopped", info.State)
	}
	if m.Count() != 0 {
		t.Errorf("live process count = %d, want 0", m.Count())
	}
	// Exited processes remain listed (reaping is tracked in Milestone 3, #31).
	if got := len(m.List()); got != 3 {
		t.Errorf("listed processes = %d, want 3 (no reaping yet)", got)
	}
}
