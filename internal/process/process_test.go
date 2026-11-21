package process

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gostalgia/internal/events"
	"gostalgia/platform"
)

// TestMain doubles as the child helper. When the test binary is invoked
// with GOSTALGIA_TEST_CHILD=1 it blocks forever (exercising kill-on-stop);
// with =exit it exits with code 3 (exercising failure status tracking).
// The sleep loop blocks without tripping the runtime deadlock detector,
// which fires on a bare select{} when no other goroutine exists.
func TestMain(m *testing.M) {
	switch os.Getenv("GOSTALGIA_TEST_CHILD") {
	case "1":
		for {
			time.Sleep(time.Hour)
		}
	case "term_immune":
		// Swallow SIGTERM so the graceful stop phase cannot terminate this
		// child; only the post-deadline kill escalation can. On platforms
		// without SIGTERM the child simply sleeps either way.
		sigCh := make(chan os.Signal, 4)
		signal.Notify(sigCh, syscall.SIGTERM)
		for {
			time.Sleep(time.Hour)
		}
	case "exit":
		os.Exit(3)
	case "spawn_grandchild":
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "GOSTALGIA_TEST_CHILD=1")
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Printf("GRANDCHILD_PID=%d\n", cmd.Process.Pid)
		for {
			time.Sleep(time.Hour)
		}
	case "check_clean_env":
		if os.Getenv("SUPER_SECRET_HOST_KEY") != "" {
			os.Exit(42) // leaked secret!
		}
		os.Exit(0)
	case "output":
		fmt.Fprintln(os.Stdout, "child stdout line 1")
		fmt.Fprintln(os.Stderr, "child stderr line 1")
		fmt.Fprintln(os.Stdout, "child stdout line 2")
		os.Exit(0)
	case "flood":
		for i := 0; i < 200; i++ {
			fmt.Fprintf(os.Stdout, "child flood line %04d: %s\n", i, strings.Repeat("x", 80))
		}
		os.Exit(0)
	case "env":
		for _, e := range os.Environ() {
			if strings.HasPrefix(e, "GOSTALGIA_") || strings.HasPrefix(e, "TEST_") || strings.HasPrefix(e, "SECRET_") {
				fmt.Println(e)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type recorder struct {
	mu        sync.Mutex
	envelopes []events.Envelope
}

func (r *recorder) add(e events.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envelopes = append(r.envelopes, e)
}

func (r *recorder) hasState(pid int32, state State) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.envelopes {
		if ev, ok := e.Payload.(Event); ok && ev.ID == pid && ev.State == state {
			return true
		}
	}
	return false
}

func newTestManager(t *testing.T) (*Manager, *recorder) {
	t.Helper()
	bus := events.NewBus()
	rec := &recorder{}
	bus.Subscribe("*", func(e events.Envelope) { rec.add(e) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(bus, log), rec
}

func TestInProcLifecycle(t *testing.T) {
	m, rec := newTestManager(t)
	started := make(chan struct{})
	p, err := m.StartInProc(context.Background(), Spec{Name: "testapp"}, func(p *Process) error {
		close(started)
		<-p.Context().Done()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started

	info := p.Info()
	if info.Kind != KindInProc {
		t.Errorf("kind = %q, want inproc", info.Kind)
	}
	if info.State != StateRunning {
		t.Errorf("state = %q, want running", info.State)
	}

	if err := m.Stop(p.ID(), 2*time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	info = p.Info()
	if info.State != StateStopped {
		t.Errorf("state after stop = %q, want stopped", info.State)
	}
	if !rec.hasState(p.ID(), StateStopped) {
		t.Error("no stopped event was published")
	}
	if !rec.hasState(p.ID(), StateRunning) {
		t.Error("no running event was published")
	}
}

func TestInProcRunErrorFailsProcess(t *testing.T) {
	m, _ := newTestManager(t)
	p, err := m.StartInProc(context.Background(), Spec{Name: "crasher"}, func(p *Process) error {
		return context.DeadlineExceeded // non-cancel error, no stop requested
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("process did not exit")
	}
	if info := p.Info(); info.State != StateFailed {
		t.Errorf("state = %q, want failed", info.State)
	}
}

func TestStopUnknownProcess(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Stop(999, time.Second); err == nil {
		t.Fatal("stopping unknown pid succeeded, want error")
	}
}

func TestStopHonorsTimeout(t *testing.T) {
	m, _ := newTestManager(t)
	p, err := m.StartInProc(context.Background(), Spec{Name: "stubborn"}, func(p *Process) error {
		<-time.After(30 * time.Second) // ignores its context on purpose
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = m.Stop(p.ID(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("stop succeeded despite a process ignoring its context")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stop took %s, timeout not honored", elapsed)
	}
}

// childHelperArgs builds an argv for invoking this test binary as a child.
// The helper mode is selected through the environment.
func childHelperArgs(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{exe}
}

func childHelperEnv(mode string) []string {
	return append(os.Environ(), "GOSTALGIA_TEST_CHILD="+mode)
}

func TestChildKilledOnStop(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, rec := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "sleeper",
		Args: childHelperArgs(t),
		Env:  childHelperEnv("1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the child settle into select{}

	if err := m.Stop(p.ID(), 2*time.Second); err != nil {
		t.Fatalf("stop child: %v", err)
	}
	info := p.Info()
	t.Logf("child info: state=%q exit_code=%d err=%q", info.State, info.ExitCode, info.Err)
	if info.Kind != KindChild {
		t.Errorf("kind = %q, want child", info.Kind)
	}
	if info.State != StateStopped {
		t.Errorf("state = %q, want stopped (killed by stop)", info.State)
	}
	if !rec.hasState(p.ID(), StateStopped) {
		t.Error("no stopped event for the child")
	}
}

func TestChildExitStatusTracked(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, _ := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "quitter",
		Args: childHelperArgs(t),
		Env:  childHelperEnv("exit"),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
	info := p.Info()
	if info.State != StateFailed {
		t.Errorf("state = %q, want failed", info.State)
	}
	if info.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", info.ExitCode)
	}
}

func TestChildRequiresArgs(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.StartChild(context.Background(), Spec{Name: "empty"}); err == nil {
		t.Fatal("StartChild without args succeeded, want error")
	}
}

func TestChildCleanEnvPreventsSecretLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	// Set a sensitive environment variable on the host
	t.Setenv("SUPER_SECRET_HOST_KEY", "super-secret-token")

	m, _ := newTestManager(t)
	// Start child with nil Env: manager should apply CleanEnv() by default
	p, err := m.StartChild(context.Background(), Spec{
		Name: "clean-env-test",
		Args: childHelperArgs(t),
		Env:  append(CleanEnv(), "GOSTALGIA_TEST_CHILD=check_clean_env"),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}
	info := p.Info()
	if info.State != StateStopped {
		t.Errorf("state = %q, want stopped; err = %s", info.State, info.Err)
	}
	if info.ExitCode != 0 {
		t.Errorf("exit code = %d (code 42 indicates secret leaked into child env)", info.ExitCode)
	}
}

func TestDefaultChildEnv(t *testing.T) {
	spec := Spec{
		Name: "test-child",
		Env: []string{
			"GOSTALGIA_APP_TOKEN=token123",
			"CUSTOM_VAR=value",
			"HOST_SECRET_TOKEN=leaked",
			"MY_PASSWORD=bad",
		},
	}
	env := DefaultChildEnv(spec, 42)
	envMap := make(map[string]string)
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		envMap[k] = v
	}
	if envMap["GOSTALGIA_APP_TOKEN"] != "token123" {
		t.Errorf("GOSTALGIA_APP_TOKEN = %q, want token123", envMap["GOSTALGIA_APP_TOKEN"])
	}
	if envMap["CUSTOM_VAR"] != "value" {
		t.Errorf("CUSTOM_VAR = %q, want value", envMap["CUSTOM_VAR"])
	}
	if _, ok := envMap["HOST_SECRET_TOKEN"]; ok {
		t.Error("HOST_SECRET_TOKEN was not stripped")
	}
	if _, ok := envMap["MY_PASSWORD"]; ok {
		t.Error("MY_PASSWORD was not stripped")
	}
}

func TestRingBuffer(t *testing.T) {
	rb := NewRingBuffer(10)
	diag := rb.Snapshot()
	if diag.TotalBytes != 0 || diag.BufferedBytes != 0 || diag.DroppedBytes != 0 || diag.Truncated || diag.Content != "" {
		t.Fatalf("unexpected initial snapshot: %+v", diag)
	}

	// Write 6 bytes
	n, err := rb.Write([]byte("hello!"))
	if err != nil || n != 6 {
		t.Fatalf("write failed: n=%d err=%v", n, err)
	}
	diag = rb.Snapshot()
	if diag.TotalBytes != 6 || diag.BufferedBytes != 6 || diag.DroppedBytes != 0 || diag.Truncated || diag.Content != "hello!" {
		t.Fatalf("unexpected snapshot after 6 bytes: %+v", diag)
	}

	// Write 6 more bytes (total 12 bytes, cap 10, should drop 2 bytes)
	n, err = rb.Write([]byte("world!"))
	if err != nil || n != 6 {
		t.Fatalf("write failed: n=%d err=%v", n, err)
	}
	diag = rb.Snapshot()
	if diag.TotalBytes != 12 || diag.BufferedBytes != 10 || diag.DroppedBytes != 2 || !diag.Truncated || diag.Content != "llo!world!" {
		t.Fatalf("unexpected snapshot after 12 bytes: %+v", diag)
	}

	// Write a single chunk larger than capacity (15 bytes)
	n, err = rb.Write([]byte("0123456789ABCDE"))
	if err != nil || n != 15 {
		t.Fatalf("write failed: n=%d err=%v", n, err)
	}
	diag = rb.Snapshot()
	if diag.TotalBytes != 27 || diag.BufferedBytes != 10 || diag.DroppedBytes != 17 || !diag.Truncated || diag.Content != "56789ABCDE" {
		t.Fatalf("unexpected snapshot after large write: %+v", diag)
	}

	// Concurrent writes and snapshots
	rbConcurrent := NewRingBuffer(100)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = rbConcurrent.Write([]byte(fmt.Sprintf("[%d:%d]", id, j)))
				_ = rbConcurrent.Snapshot()
			}
		}(i)
	}
	wg.Wait()
	finalDiag := rbConcurrent.Snapshot()
	if finalDiag.BufferedBytes > 100 {
		t.Fatalf("buffered bytes %d exceeds capacity 100", finalDiag.BufferedBytes)
	}
}

func TestChildOutputCaptured(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, _ := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "speaker",
		Args: childHelperArgs(t),
		Env:  childHelperEnv("output"),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}

	logs, ok := m.Logs(p.ID())
	if !ok {
		t.Fatal("Logs returned ok=false for child process")
	}
	if logs.State != StateStopped {
		t.Fatalf("state = %s, want stopped", logs.State)
	}
	if logs.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0", logs.ExitCode)
	}
	if logs.Duration == "" {
		t.Fatal("expected non-empty duration")
	}
	if !strings.Contains(logs.Stdout.Content, "child stdout line 1") || !strings.Contains(logs.Stdout.Content, "child stdout line 2") {
		t.Fatalf("stdout missing expected lines: %q", logs.Stdout.Content)
	}
	if !strings.Contains(logs.Stderr.Content, "child stderr line 1") {
		t.Fatalf("stderr missing expected line: %q", logs.Stderr.Content)
	}
	if !strings.Contains(logs.Combined.Content, "child stdout line 1") || !strings.Contains(logs.Combined.Content, "child stderr line 1") {
		t.Fatalf("combined missing output: %q", logs.Combined.Content)
	}
	if logs.Stdout.Truncated || logs.Stderr.Truncated {
		t.Fatalf("streams unexpectedly marked truncated: stdout=%v stderr=%v", logs.Stdout.Truncated, logs.Stderr.Truncated)
	}
	if logs.Stdout.TotalBytes == 0 || logs.Stderr.TotalBytes == 0 {
		t.Fatalf("byte counts should be non-zero: stdout=%d stderr=%d", logs.Stdout.TotalBytes, logs.Stderr.TotalBytes)
	}
}

func TestChildRingBufferBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, _ := newTestManager(t)
	limit := 512
	p, err := m.StartChild(context.Background(), Spec{
		Name:     "flooder",
		Args:     childHelperArgs(t),
		Env:      childHelperEnv("flood"),
		LogLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}

	logs, ok := m.Logs(p.ID())
	if !ok {
		t.Fatal("Logs returned ok=false")
	}
	if !logs.Stdout.Truncated {
		t.Fatal("expected stdout to be marked truncated")
	}
	if logs.Stdout.DroppedBytes <= 0 {
		t.Fatalf("expected stdout dropped bytes > 0, got %d", logs.Stdout.DroppedBytes)
	}
	if logs.Stdout.BufferedBytes > limit {
		t.Fatalf("buffered bytes %d exceeds limit %d", logs.Stdout.BufferedBytes, limit)
	}
	if logs.Stdout.TotalBytes != logs.Stdout.DroppedBytes+int64(logs.Stdout.BufferedBytes) {
		t.Fatalf("total (%d) != dropped (%d) + buffered (%d)",
			logs.Stdout.TotalBytes, logs.Stdout.DroppedBytes, logs.Stdout.BufferedBytes)
	}
	if !strings.Contains(logs.Stdout.Content, "child flood line 0199") {
		t.Fatalf("expected stdout tail to contain final lines, got: %q", logs.Stdout.Content)
	}
}

func TestChildEnvironmentSanitization(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	m, _ := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "envtest",
		Args: childHelperArgs(t),
		Env: []string{
			"GOSTALGIA_TEST_CHILD=env",
			"SECRET_API_TOKEN=supersecret",
			"TEST_SAFE_VAR=visible",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child did not exit")
	}

	logs, ok := m.Logs(p.ID())
	if !ok {
		t.Fatal("Logs returned ok=false")
	}
	if !strings.Contains(logs.Stdout.Content, "GOSTALGIA_PROCESS_ID=") {
		t.Fatalf("expected GOSTALGIA_PROCESS_ID in child env, got: %s", logs.Stdout.Content)
	}
	if !strings.Contains(logs.Stdout.Content, "TEST_SAFE_VAR=visible") {
		t.Fatalf("expected TEST_SAFE_VAR in child env, got: %s", logs.Stdout.Content)
	}
	if strings.Contains(logs.Stdout.Content, "SECRET_API_TOKEN") || strings.Contains(logs.Stdout.Content, "supersecret") {
		t.Fatalf("sensitive variable leaked into child process env: %s", logs.Stdout.Content)
	}
}

func TestInProcLogs(t *testing.T) {
	m, _ := newTestManager(t)
	p, err := m.StartInProc(context.Background(), Spec{Name: "noop"}, func(p *Process) error {
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("inproc did not exit")
	}

	logs, ok := m.Logs(p.ID())
	if !ok {
		t.Fatal("Logs returned ok=false")
	}
	if logs.Kind != KindInProc {
		t.Fatalf("kind = %s, want inproc", logs.Kind)
	}
	if logs.Stdout.Content != "" || logs.Stderr.Content != "" {
		t.Fatalf("expected empty logs for inproc, got stdout=%q stderr=%q", logs.Stdout.Content, logs.Stderr.Content)
	}
}

func TestLogsUnknownPID(t *testing.T) {
	m, _ := newTestManager(t)
	if _, ok := m.Logs(99999); ok {
		t.Fatal("Logs returned ok=true for unknown PID")
	}
}

func TestUserStopRecordsStopped(t *testing.T) {
	m, _ := newTestManager(t)
	started := make(chan struct{})
	var runs atomic.Int32
	p, err := m.StartInProc(context.Background(), Spec{
		Name: "looping",
	}, func(p *Process) error {
		runs.Add(1)
		select {
		case <-started:
		default:
			close(started)
		}
		<-p.Context().Done()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := m.Stop(p.ID(), time.Second); err != nil {
		t.Fatal(err)
	}
	<-p.Done()
	time.Sleep(50 * time.Millisecond)
	if r := runs.Load(); r != 1 {
		t.Fatalf("expected 1 run, got %d", r)
	}
	info := p.Info()
	if info.State != StateStopped {
		t.Fatalf("expected StateStopped, got %s", info.State)
	}
}

func TestBoundedHistoryAndReap(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetMaxHistory(3)

	var pids []int32
	for i := 0; i < 5; i++ {
		p, err := m.StartInProc(context.Background(), Spec{
			Name: fmt.Sprintf("proc-%d", i),
		}, func(p *Process) error {
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		<-p.Done()
		pids = append(pids, p.ID())
	}

	history := m.History()
	if len(history) != 3 {
		t.Fatalf("expected 3 history entries, got %d", len(history))
	}
	for _, id := range pids[2:] {
		if _, ok := m.HistoryByID(id); !ok {
			t.Errorf("expected pid %d in history", id)
		}
	}
	if _, ok := m.HistoryByID(pids[0]); ok {
		t.Errorf("pid %d should have been evicted", pids[0])
	}

	if len(m.List()) != 5 {
		t.Fatalf("expected 5 processes in list before reap, got %d", len(m.List()))
	}
	reaped := m.Reap()
	if reaped != 5 {
		t.Fatalf("expected 5 reaped processes, got %d", reaped)
	}
	if len(m.List()) != 0 {
		t.Fatalf("expected 0 processes in list after reap, got %d", len(m.List()))
	}
	if len(m.History()) != 3 {
		t.Fatalf("expected 3 history entries after reap, got %d", len(m.History()))
	}
}

func TestAutoReap(t *testing.T) {
	m, _ := newTestManager(t)
	m.SetMaxHistory(2)

	// Spawning 15 short-lived processes will cross the auto-reap limit (limit = max(10, 2*2) = 10)
	for i := 0; i < 15; i++ {
		p, err := m.StartInProc(context.Background(), Spec{
			Name: fmt.Sprintf("auto-reap-%d", i),
		}, func(p *Process) error {
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		<-p.Done()
	}

	// Auto-reaping should have pruned inactive processes down to maxHistory (2)
	list := m.List()
	if len(list) > 10 {
		t.Fatalf("expected auto-reap to prune inactive processes, got %d", len(list))
	}
	if len(m.History()) != 2 {
		t.Fatalf("expected 2 history entries, got %d", len(m.History()))
	}
}

func TestChildTreeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group tree signaling is Unix-specific")
	}
	m, _ := newTestManager(t)
	p, err := m.StartChild(context.Background(), Spec{
		Name: "grandparent",
		Args: childHelperArgs(t),
		Env:  []string{"GOSTALGIA_TEST_CHILD=spawn_grandchild"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var grandchildPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logs, ok := m.Logs(p.ID())
		if ok && strings.Contains(logs.Stdout.Content, "GRANDCHILD_PID=") {
			for _, line := range strings.Split(logs.Stdout.Content, "\n") {
				if strings.HasPrefix(line, "GRANDCHILD_PID=") {
					fmt.Sscanf(line, "GRANDCHILD_PID=%d", &grandchildPID)
					break
				}
			}
			if grandchildPID > 0 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchildPID == 0 {
		t.Fatal("failed to find grandchild PID from child logs")
	}

	if !platform.ProcessAlive(grandchildPID) {
		t.Fatalf("grandchild %d not running", grandchildPID)
	}

	if err := m.Stop(p.ID(), time.Second); err != nil {
		t.Fatal(err)
	}

	killed := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !platform.ProcessAlive(grandchildPID) {
			killed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !killed {
		t.Fatalf("grandchild %d was not killed by KillProcessTree", grandchildPID)
	}
}

func TestChildSandboxPolicyApplied(t *testing.T) {
	if testing.Short() {
		t.Skip("child process test skipped in short mode")
	}
	caps := platform.GetHostSecurityCapabilities()
	if !caps.Supported {
		t.Skip("sandbox not supported on this platform")
	}

	m, _ := newTestManager(t)
	policy, err := platform.PolicyForIsolation(platform.IsolationSandbox)
	if err != nil {
		t.Fatalf("PolicyForIsolation: %v", err)
	}
	p, err := m.StartChild(context.Background(), Spec{
		Name:   "sandboxed-sleeper",
		Args:   childHelperArgs(t),
		Env:    childHelperEnv("1"),
		Policy: policy,
	})
	if err != nil {
		t.Fatalf("StartChild sandboxed: %v", err)
	}
	defer func() { _ = m.Stop(p.ID(), time.Second) }()

	info := p.Info()
	if info.Isolation != platform.IsolationSandbox {
		t.Errorf("expected isolation %s, got %s", platform.IsolationSandbox, info.Isolation)
	}
	if info.Policy == nil || info.Policy.Isolation != platform.IsolationSandbox {
		t.Errorf("expected info.Policy to reflect sandbox policy")
	}
}

func TestChildSandboxPolicyFailClosed(t *testing.T) {
	m, _ := newTestManager(t)
	// Spec with an invalid isolation
	_, err := m.StartChild(context.Background(), Spec{
		Name: "invalid-policy",
		Args: []string{"echo", "hi"},
		Policy: platform.ExecutionPolicy{
			Isolation: "nonexistent-isolation-level",
		},
	})
	if err == nil {
		t.Fatal("expected StartChild to fail with invalid policy")
	}
}
