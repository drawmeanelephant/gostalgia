// Package process implements the environment's process model. An
// environment process is an explicit, tracked object with an ID, a name,
// a kind, a state, and a lifecycle — never an anonymous goroutine.
//
// Two kinds exist and are deliberately distinct:
//
//	inproc — application or service logic inside the runtime (a
//	         goroutine with a cancellable context). Logically isolated
//	         only: a panic or deadlock can affect the runtime.
//	child  — a real host child process (os/exec). Isolated by the host
//	         OS, with an exit code, killable from outside.
//
// Nothing in this package pretends a goroutine is an OS process.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/platform"
)

const (
	// DefaultLogCapacity is the default ring buffer capacity per stream (64 KB).
	DefaultLogCapacity    = 64 * 1024
	DefaultMaxRestarts    = 5
	DefaultRestartWindow  = 1 * time.Minute
	DefaultInitialBackoff = 100 * time.Millisecond
	DefaultMaxBackoff     = 5 * time.Second
	DefaultBackoffFactor  = 2.0
	DefaultMaxHistory     = 100
)

type Kind string

const (
	KindInProc Kind = "inproc"
	KindChild  Kind = "child"
)

type State string

const (
	StateStarting   State = "starting"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateFailed     State = "failed"
	StateRestarting State = "restarting"
	StateCrashLoop  State = "crashloop"
)

// RestartPolicy defines if and when a process should be restarted on exit.
type RestartPolicy string

const (
	RestartNever     RestartPolicy = "never"      // default: do not restart
	RestartAlways    RestartPolicy = "always"     // restart regardless of exit reason unless explicitly stopped
	RestartOnFailure RestartPolicy = "on-failure" // restart only when process fails or exits with non-zero code
)

// SupervisionConfig configures restart and crash loop limits.
type SupervisionConfig struct {
	Policy         RestartPolicy `json:"policy"`
	MaxRestarts    int           `json:"max_restarts,omitempty"`    // max restarts within window before crash-loop cutoff (default: 5)
	Window         time.Duration `json:"window,omitempty"`          // sliding window for restart count (default: 1m)
	InitialBackoff time.Duration `json:"initial_backoff,omitempty"` // initial delay before restart (default: 100ms)
	MaxBackoff     time.Duration `json:"max_backoff,omitempty"`     // max delay before restart (default: 5s)
	BackoffFactor  float64       `json:"backoff_factor,omitempty"`  // exponential multiplier (default: 2.0)
}

func (s SupervisionConfig) withDefaults() SupervisionConfig {
	cfg := s
	if cfg.MaxRestarts <= 0 {
		cfg.MaxRestarts = DefaultMaxRestarts
	}
	if cfg.Window <= 0 {
		cfg.Window = DefaultRestartWindow
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = DefaultInitialBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	if cfg.BackoffFactor <= 1.0 {
		cfg.BackoffFactor = DefaultBackoffFactor
	}
	return cfg
}

// Spec describes a process to start.
type Spec struct {
	Name      string                   `json:"name"`                // logical name (app id, child label)
	Kind      Kind                     `json:"kind"`                // inproc or child
	SessionID string                   `json:"session,omitempty"`   // owning session
	User      string                   `json:"user,omitempty"`      // owning user
	Caps      []string                 `json:"caps,omitempty"`      // granted capabilities
	Args      []string                 `json:"args,omitempty"`      // child only: program and arguments
	Dir       string                   `json:"dir,omitempty"`       // child only: working directory
	Env       []string                 `json:"env,omitempty"`       // child only; nil defaults to deliberate child environment
	LogLimit  int                      `json:"log_limit,omitempty"` // child only: ring buffer capacity per stream in bytes (default 64KB)
	Restart   SupervisionConfig        `json:"restart,omitempty"`   // supervision and restart policy
	Policy    platform.ExecutionPolicy `json:"policy,omitempty"`    // host execution and sandbox policy

	// ExtraFiles are inherited open files passed to the child as fds 3,4,...
	// (cmd.ExtraFiles). Used to hand confined children a pre-connected IPC
	// channel so they never need filesystem or socket-connect authority.
	// Each file is duped into the child at Start; the parent may close its
	// copy afterwards, but supervised restarts would need a fresh file.
	ExtraFiles []*os.File `json:"-"`
}

// StreamDiagnostics holds bounded buffer content and drop accounting for an output stream.
type StreamDiagnostics struct {
	TotalBytes    int64  `json:"total_bytes"`
	BufferedBytes int    `json:"buffered_bytes"`
	DroppedBytes  int64  `json:"dropped_bytes"`
	Truncated     bool   `json:"truncated"`
	Content       string `json:"content"`
}

// Logs holds captured stdout/stderr diagnostics and lifecycle snapshot for a process.
type Logs struct {
	ID        int32             `json:"id"`
	Name      string            `json:"name"`
	Kind      Kind              `json:"kind"`
	State     State             `json:"state"`
	ExitCode  int               `json:"exit_code,omitempty"`
	StartedAt time.Time         `json:"started_at,omitempty"`
	ExitedAt  time.Time         `json:"exited_at,omitempty"`
	Duration  string            `json:"duration,omitempty"`
	Stdout    StreamDiagnostics `json:"stdout"`
	Stderr    StreamDiagnostics `json:"stderr"`
	Combined  StreamDiagnostics `json:"combined"`
}

// Info is a snapshot of a process's state.
type Info struct {
	ID           int32                     `json:"id"`
	Name         string                    `json:"name"`
	Kind         Kind                      `json:"kind"`
	State        State                     `json:"state"`
	SessionID    string                    `json:"session,omitempty"`
	User         string                    `json:"user,omitempty"`
	Caps         []string                  `json:"caps,omitempty"` // granted capabilities, from the spec
	StartedAt    time.Time                 `json:"started_at,omitempty"`
	ExitedAt     time.Time                 `json:"exited_at,omitempty"`
	Err          string                    `json:"error,omitempty"`
	ExitCode     int                       `json:"exit_code,omitempty"`
	RestartCount int                       `json:"restart_count,omitempty"`
	CrashLoop    bool                      `json:"crash_loop,omitempty"`
	Resources    platform.ResourceUsage    `json:"resources"`
	Isolation    string                    `json:"isolation,omitempty"`
	Policy       *platform.ExecutionPolicy `json:"policy,omitempty"`
}

// HistoryEntry is a bounded record of an exited or reaped process instance.
type HistoryEntry struct {
	ID           int32                  `json:"id"`
	Name         string                 `json:"name"`
	Kind         Kind                   `json:"kind"`
	State        State                  `json:"state"`
	SessionID    string                 `json:"session,omitempty"`
	User         string                 `json:"user,omitempty"`
	ExitCode     int                    `json:"exit_code,omitempty"`
	Err          string                 `json:"error,omitempty"`
	StartedAt    time.Time              `json:"started_at,omitempty"`
	ExitedAt     time.Time              `json:"exited_at,omitempty"`
	Duration     string                 `json:"duration,omitempty"`
	RestartCount int                    `json:"restart_count,omitempty"`
	CrashLoop    bool                   `json:"crash_loop,omitempty"`
	Resources    platform.ResourceUsage `json:"resources"`
	Isolation    string                 `json:"isolation,omitempty"`
}

// Event is published on every state transition.
type Event struct {
	ID    int32  `json:"id"`
	Name  string `json:"name"`
	Kind  Kind   `json:"kind"`
	State State  `json:"state"`
	Err   string `json:"error,omitempty"`
}

func (Event) Type() string { return "proc.state" }

// Process is one environment process.
type Process struct {
	mu           sync.Mutex
	info         Info
	spec         Spec
	masterCtx    context.Context
	masterCancel context.CancelFunc
	runCtx       context.Context
	runCancel    context.CancelFunc
	done         chan struct{}
	cmd          *exec.Cmd // child only
	pid          int       // child host PID
	stdout       *RingBuffer
	stderr       *RingBuffer
	combined     *RingBuffer
	userStopped  bool
	restartTimes []time.Time
}

// ID returns the process ID.
func (p *Process) ID() int32 { return p.info.ID }

// Context returns the process context: canceled when the process is
// stopped. In-proc applications must honor it.
func (p *Process) Context() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runCtx != nil {
		return p.runCtx
	}
	return p.masterCtx
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Info returns a state snapshot.
func (p *Process) Info() Info {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	info.Caps = append([]string(nil), info.Caps...)
	if info.State == StateRunning && p.pid > 0 {
		if res := platform.SampleProcessResources(p.pid); res.Supported {
			info.Resources = res
		}
	}
	return info
}

// Caps returns the capabilities granted to this process (from its spec).
func (p *Process) Caps() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.info.Caps
}

func (p *Process) setState(state State, err error) {
	p.mu.Lock()
	p.info.State = state
	if err != nil {
		p.info.Err = err.Error()
	}
	p.mu.Unlock()
}

// Manager tracks all environment processes.
type Manager struct {
	mu           sync.Mutex
	next         int32
	procs        map[int32]*Process
	history      []HistoryEntry
	maxHistory   int
	reapedCount  int
	shuttingDown atomic.Bool // Supervisors read this while holding process locks.
	bus          *events.Bus
	log          *slog.Logger
}

func NewManager(bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		next:       1,
		procs:      map[int32]*Process{},
		history:    make([]HistoryEntry, 0, DefaultMaxHistory),
		maxHistory: DefaultMaxHistory,
		bus:        bus,
		log:        log,
	}
}

func (m *Manager) alloc() int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	return m.next - 1
}

func (m *Manager) publish(p *Process) {
	info := p.Info()
	m.bus.Publish("process", Event{
		ID: info.ID, Name: info.Name, Kind: info.Kind, State: info.State, Err: info.Err,
	})
}

// add registers p, or refuses once shutdown began. Checking while holding
// the registry lock makes the decision atomic with List snapshots: a
// process that slips past this check is always visible to StopAll, and one
// refused here can never linger unseen. Callers must undo a refused start
// (kill spawned children, cancel contexts).
func (m *Manager) add(p *Process) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.shuttingDown.Load() {
		return false
	}
	m.procs[p.info.ID] = p
	return true
}

// StartInProc starts run as an environment process of kind "inproc". The
// process context is canceled by Stop; run must honor it.
func (m *Manager) StartInProc(ctx context.Context, spec Spec, run func(p *Process) error) (*Process, error) {
	if run == nil {
		return nil, errors.New("process: run function is required")
	}
	spec.Kind = KindInProc
	spec.Restart = spec.Restart.withDefaults()
	id := m.alloc()
	masterCtx, masterCancel := context.WithCancel(ipc.WithCapabilities(ctx, security.NewCapabilities(spec.Caps...)))
	runCtx, runCancel := context.WithCancel(masterCtx)
	p := &Process{
		done:         make(chan struct{}),
		spec:         spec,
		masterCtx:    masterCtx,
		masterCancel: masterCancel,
		runCtx:       runCtx,
		runCancel:    runCancel,
	}
	inprocIsolation := spec.Policy.Isolation
	if inprocIsolation == "" {
		inprocIsolation = platform.IsolationInProc
	}
	p.info = Info{
		ID:        id,
		Name:      spec.Name,
		Kind:      spec.Kind,
		State:     StateStarting,
		SessionID: spec.SessionID,
		User:      spec.User,
		StartedAt: time.Now(),
		Caps:      append([]string(nil), spec.Caps...),
		Resources: platform.ResourceUsage{Supported: false},
		Isolation: inprocIsolation,
		Policy:    &spec.Policy,
	}
	if !m.add(p) {
		masterCancel()
		runCancel()
		return nil, errors.New("process: environment is shutting down")
	}
	m.publish(p) // starting
	p.setState(StateRunning, nil)
	m.publish(p)
	m.log.Info("process started", "pid", id, "name", spec.Name, "kind", spec.Kind)

	go m.superviseInProc(p, run)
	return p, nil
}

func (m *Manager) superviseInProc(p *Process, run func(p *Process) error) {
	defer func() {
		p.masterCancel()
		m.recordHistory(p)
		m.autoReap()
		close(p.done)
	}()

	firstRun := true
	for {
		if !firstRun {
			p.mu.Lock()
			if p.userStopped || m.isShuttingDown() {
				p.info.State = StateStopped
				p.mu.Unlock()
				m.publish(p)
				return
			}
			runCtx, runCancel := context.WithCancel(p.masterCtx)
			p.runCtx = runCtx
			p.runCancel = runCancel
			p.info.State = StateRunning
			p.info.StartedAt = time.Now()
			p.mu.Unlock()

			m.publish(p)
			m.log.Info("process restarted", "pid", p.info.ID, "name", p.spec.Name, "restart_count", p.info.RestartCount)
		}
		firstRun = false

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			err = run(p)
		}()

		p.mu.Lock()
		runCancel := p.runCancel
		p.mu.Unlock()
		if runCancel != nil {
			runCancel()
		}

		now := time.Now()
		p.mu.Lock()
		p.info.ExitedAt = now
		userStopped := p.userStopped || m.isShuttingDown()
		failed := err != nil && !userStopped

		if userStopped {
			p.info.State = StateStopped
			p.info.Err = ""
			p.mu.Unlock()
			m.publish(p)
			m.log.Info("process stopped", "pid", p.info.ID, "name", p.spec.Name)
			return
		}

		if failed {
			p.info.State = StateFailed
			p.info.Err = err.Error()
			m.log.Error("process failed", "pid", p.info.ID, "name", p.spec.Name, "err", err)
		} else {
			p.info.State = StateStopped
			p.info.Err = ""
			m.log.Info("process stopped", "pid", p.info.ID, "name", p.spec.Name)
		}

		shouldRestart := false
		if p.spec.Restart.Policy == RestartAlways {
			shouldRestart = true
		} else if p.spec.Restart.Policy == RestartOnFailure && failed {
			shouldRestart = true
		}

		if !shouldRestart {
			p.mu.Unlock()
			m.publish(p)
			return
		}

		// Check crash loop
		p.restartTimes = append(p.restartTimes, now)
		windowStart := now.Add(-p.spec.Restart.Window)
		validIdx := 0
		for _, t := range p.restartTimes {
			if t.After(windowStart) {
				p.restartTimes[validIdx] = t
				validIdx++
			}
		}
		p.restartTimes = p.restartTimes[:validIdx]

		if len(p.restartTimes) > p.spec.Restart.MaxRestarts {
			p.info.State = StateCrashLoop
			p.info.CrashLoop = true
			p.info.Err = fmt.Sprintf("crash loop: exceeded %d restarts within %s", p.spec.Restart.MaxRestarts, p.spec.Restart.Window)
			p.mu.Unlock()
			m.publish(p)
			m.log.Error("process entered crash loop", "pid", p.info.ID, "name", p.spec.Name, "restarts", len(p.restartTimes))
			return
		}

		attempts := len(p.restartTimes) - 1
		backoff := p.spec.Restart.InitialBackoff
		for i := 0; i < attempts; i++ {
			backoff = time.Duration(float64(backoff) * p.spec.Restart.BackoffFactor)
			if backoff > p.spec.Restart.MaxBackoff {
				backoff = p.spec.Restart.MaxBackoff
				break
			}
		}

		p.info.State = StateRestarting
		p.info.RestartCount++
		p.mu.Unlock()
		m.publish(p)
		m.log.Info("process restarting after backoff", "pid", p.info.ID, "name", p.spec.Name, "backoff", backoff, "attempt", p.info.RestartCount)

		select {
		case <-p.masterCtx.Done():
			p.mu.Lock()
			p.info.State = StateStopped
			p.mu.Unlock()
			m.publish(p)
			return
		case <-time.After(backoff):
		}
	}
}

// RingBuffer is a concurrency-safe bounded byte ring buffer with drop accounting.
type RingBuffer struct {
	mu       sync.Mutex
	capacity int
	buf      []byte
	total    int64
	dropped  int64
}

// NewRingBuffer allocates a ring buffer with the given byte capacity.
func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	return &RingBuffer{
		capacity: capacity,
		buf:      make([]byte, 0, min(capacity, 4096)),
	}
}

// Write appends p to the ring buffer. If writing p exceeds capacity, the
// oldest bytes are discarded and accounted for in DroppedBytes.
func (r *RingBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	r.total += int64(n)
	if n >= r.capacity {
		r.dropped += int64(len(r.buf)) + int64(n-r.capacity)
		r.buf = append(r.buf[:0], p[n-r.capacity:]...)
		return n, nil
	}
	overflow := len(r.buf) + n - r.capacity
	if overflow > 0 {
		r.dropped += int64(overflow)
		copy(r.buf, r.buf[overflow:])
		r.buf = r.buf[:len(r.buf)-overflow]
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// Snapshot returns a copy of the stream buffer content and diagnostic accounting.
func (r *RingBuffer) Snapshot() StreamDiagnostics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return StreamDiagnostics{
		TotalBytes:    r.total,
		BufferedBytes: len(r.buf),
		DroppedBytes:  r.dropped,
		Truncated:     r.dropped > 0,
		Content:       string(r.buf),
	}
}

// CleanEnv returns an explicit child process environment containing only
// standard system execution variables (PATH, SYSTEMROOT, TMPDIR, etc.) and
// explicit additions. This prevents wholesale inheritance of host secrets or
// operator tokens by child processes.
func CleanEnv(explicit ...string) []string {
	allowlist := map[string]bool{
		"PATH":        true,
		"SYSTEMROOT":  true,
		"SYSTEMDRIVE": true,
		"WINDIR":      true,
		"COMSPEC":     true,
		"PATHEXT":     true,
		"TMPDIR":      true,
		"TEMP":        true,
		"TMP":         true,
		"HOME":        true,
		"USERPROFILE": true,
		"LANG":        true,
		"LC_ALL":      true,
		"TERM":        true,
		"TZ":          true,
	}
	var out []string
	for _, env := range os.Environ() {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) > 0 && allowlist[strings.ToUpper(parts[0])] {
			out = append(out, env)
		}
	}
	out = append(out, explicit...)
	return out
}

// DefaultChildEnv builds a deliberate, sanitized environment for child processes
// rather than inheriting arbitrary host credentials (tokens, secrets, private keys).
func DefaultChildEnv(spec Spec, id int32) []string {
	env := CleanEnv(
		"GOSTALGIA_ENV=1",
		fmt.Sprintf("GOSTALGIA_PID=%d", id),
		fmt.Sprintf("GOSTALGIA_PROCESS_ID=%d", id),
		fmt.Sprintf("GOSTALGIA_PROCESS_NAME=%s", spec.Name),
	)
	if spec.User != "" {
		env = append(env, "USER="+spec.User)
	}
	if spec.SessionID != "" {
		env = append(env, "GOSTALGIA_SESSION="+spec.SessionID)
	}
	for _, kv := range spec.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || (isSensitiveEnvKey(k) && !strings.HasPrefix(strings.ToUpper(k), "GOSTALGIA_")) {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func isSensitiveEnvKey(k string) bool {
	upper := strings.ToUpper(k)
	for _, bad := range []string{"SECRET", "TOKEN", "PASSWORD", "PASSWD", "PRIVATE_KEY", "CREDENTIAL", "AUTH_SOCK"} {
		if strings.Contains(upper, bad) {
			return true
		}
	}
	return false
}

func (m *Manager) buildChildCmd(ctx context.Context, spec Spec, id int32, stdout, stderr, combined io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, spec.Args[0], spec.Args[1:]...)
	cmd.Dir = spec.Dir
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = DefaultChildEnv(spec, id)
	cmd.ExtraFiles = spec.ExtraFiles
	platform.SetupProcessTree(cmd)
	cmd.Cancel = func() error {
		return platform.KillProcessTree(cmd)
	}
	cmd.Stdout = io.MultiWriter(stdout, combined)
	cmd.Stderr = io.MultiWriter(stderr, combined)
	return cmd
}

// StartChild starts spec.Args as a real host child process. The child is
// killed when its context is canceled (i.e. by Stop). Standard output and
// standard error are captured into bounded ring buffers with drop accounting.
func (m *Manager) StartChild(ctx context.Context, spec Spec) (*Process, error) {
	if len(spec.Args) == 0 {
		return nil, errors.New("process: child spec requires args")
	}
	spec.Kind = KindChild
	spec.Restart = spec.Restart.withDefaults()
	id := m.alloc()

	masterCtx, masterCancel := context.WithCancel(ipc.WithCapabilities(ctx, security.NewCapabilities(spec.Caps...)))
	capacity := spec.LogLimit
	if capacity <= 0 {
		capacity = DefaultLogCapacity
	}
	stdoutBuf := NewRingBuffer(capacity)
	stderrBuf := NewRingBuffer(capacity)
	combinedBuf := NewRingBuffer(capacity)

	p := &Process{
		done:         make(chan struct{}),
		spec:         spec,
		masterCtx:    masterCtx,
		masterCancel: masterCancel,
		stdout:       stdoutBuf,
		stderr:       stderrBuf,
		combined:     combinedBuf,
	}
	childIsolation := spec.Policy.Isolation
	if childIsolation == "" {
		childIsolation = platform.IsolationTrusted
	}
	p.info = Info{
		ID:        id,
		Name:      spec.Name,
		Kind:      spec.Kind,
		State:     StateStarting,
		SessionID: spec.SessionID,
		User:      spec.User,
		StartedAt: time.Now(),
		Caps:      append([]string(nil), spec.Caps...),
		Resources: platform.ResourceUsage{Supported: false},
		Isolation: childIsolation,
		Policy:    &spec.Policy,
	}

	runCtx, runCancel := context.WithCancel(p.masterCtx)
	cmd := m.buildChildCmd(runCtx, spec, id, stdoutBuf, stderrBuf, combinedBuf)
	postHook, err := platform.ConfigureSandbox(cmd, spec.Policy)
	if err != nil {
		runCancel()
		masterCancel()
		return nil, fmt.Errorf("process: configure sandbox for %s: %w", spec.Name, err)
	}
	if err := cmd.Start(); err != nil {
		runCancel()
		masterCancel()
		return nil, fmt.Errorf("process: start %s: %w", spec.Args[0], err)
	}
	if postHook != nil {
		if err := postHook(cmd.Process.Pid); err != nil {
			_ = platform.KillProcessTree(cmd)
			runCancel()
			masterCancel()
			return nil, fmt.Errorf("process: apply post-start sandbox policy for %s: %w", spec.Name, err)
		}
	}

	p.runCtx = runCtx
	p.runCancel = runCancel
	p.cmd = cmd
	p.pid = cmd.Process.Pid
	p.info.State = StateRunning
	if !m.add(p) {
		// Shutdown began while the child was being spawned: it is in no
		// registry and no supervisor is watching, so kill it ourselves.
		_ = platform.KillProcessTree(cmd)
		masterCancel()
		runCancel()
		return nil, errors.New("process: environment is shutting down")
	}
	m.publish(p) // starting
	m.publish(p) // running
	m.log.Info("child process started", "pid", id, "name", spec.Name)

	go m.superviseChild(p)
	return p, nil
}

func (m *Manager) superviseChild(p *Process) {
	defer func() {
		p.masterCancel()
		m.recordHistory(p)
		m.autoReap()
		close(p.done)
	}()

	firstRun := true
	for {
		if !firstRun {
			p.mu.Lock()
			if p.userStopped || m.isShuttingDown() {
				p.info.State = StateStopped
				p.mu.Unlock()
				m.publish(p)
				return
			}
			runCtx, runCancel := context.WithCancel(p.masterCtx)
			cmd := m.buildChildCmd(runCtx, p.spec, p.info.ID, p.stdout, p.stderr, p.combined)
			postHook, sandboxErr := platform.ConfigureSandbox(cmd, p.spec.Policy)
			if sandboxErr != nil {
				runCancel()
				p.cmd = nil
				p.pid = 0
				p.info.State = StateFailed
				p.info.Err = fmt.Sprintf("restart sandbox configuration failed: %v", sandboxErr)
				p.mu.Unlock()
				m.publish(p)
				m.log.Error("child process restart sandbox failed", "pid", p.info.ID, "err", sandboxErr)
				return
			}
			if err := cmd.Start(); err != nil {
				runCancel()
				p.cmd = nil
				p.pid = 0
				p.info.State = StateFailed
				p.info.Err = fmt.Sprintf("restart failed: %v", err)
				p.mu.Unlock()
				m.publish(p)
				m.log.Error("child process restart failed", "pid", p.info.ID, "err", err)
				return
			}
			if postHook != nil {
				if err := postHook(cmd.Process.Pid); err != nil {
					_ = platform.KillProcessTree(cmd)
					runCancel()
					p.cmd = nil
					p.pid = 0
					p.info.State = StateFailed
					p.info.Err = fmt.Sprintf("restart post-start sandbox policy failed: %v", err)
					p.mu.Unlock()
					m.publish(p)
					m.log.Error("child process restart post-start sandbox policy failed", "pid", p.info.ID, "err", err)
					return
				}
			}
			p.runCtx = runCtx
			p.runCancel = runCancel
			p.cmd = cmd
			p.pid = cmd.Process.Pid
			p.info.State = StateRunning
			p.info.StartedAt = time.Now()
			p.mu.Unlock()

			m.publish(p)
			m.log.Info("child process restarted", "pid", p.info.ID, "name", p.spec.Name, "restart_count", p.info.RestartCount)
		}
		firstRun = false

		p.mu.Lock()
		cmd := p.cmd
		runCancel := p.runCancel
		p.mu.Unlock()

		stopWatcher := make(chan struct{})
		if p.spec.Policy.DenyDescendants && cmd != nil && cmd.Process != nil && cmd.Process.Pid > 0 {
			targetPID := cmd.Process.Pid
			go func() {
				ticker := time.NewTicker(2 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopWatcher:
						return
					case <-ticker.C:
						platform.KillDescendants(targetPID)
					}
				}
			}()
		}

		err := cmd.Wait()
		close(stopWatcher)
		_ = platform.KillProcessTree(cmd)
		runCancel()

		now := time.Now()
		p.mu.Lock()
		p.info.ExitedAt = now
		if cmd.ProcessState != nil {
			p.info.ExitCode = cmd.ProcessState.ExitCode()
			p.info.Resources = platform.SampleProcessState(cmd.ProcessState)
		}

		userStopped := p.userStopped || m.isShuttingDown()
		failed := (err != nil || (cmd.ProcessState != nil && cmd.ProcessState.ExitCode() != 0)) && !userStopped

		if userStopped {
			p.info.State = StateStopped
			p.info.Err = ""
			p.mu.Unlock()
			m.publish(p)
			m.log.Info("child process stopped", "pid", p.info.ID, "name", p.spec.Name)
			return
		}

		if failed {
			p.info.State = StateFailed
			if cmd.ProcessState != nil {
				p.info.Err = fmt.Sprintf("exit code %d: %v", cmd.ProcessState.ExitCode(), err)
			} else if err != nil {
				p.info.Err = err.Error()
			}
			m.log.Info("child process failed", "pid", p.info.ID, "name", p.spec.Name, "exit_code", p.info.ExitCode, "err", p.info.Err)
		} else {
			p.info.State = StateStopped
			p.info.Err = ""
			m.log.Info("child process exited cleanly", "pid", p.info.ID, "name", p.spec.Name, "exit_code", p.info.ExitCode)
		}

		shouldRestart := false
		if p.spec.Restart.Policy == RestartAlways {
			shouldRestart = true
		} else if p.spec.Restart.Policy == RestartOnFailure && failed {
			shouldRestart = true
		}

		if !shouldRestart {
			p.mu.Unlock()
			m.publish(p)
			return
		}

		// Check crash loop
		p.restartTimes = append(p.restartTimes, now)
		windowStart := now.Add(-p.spec.Restart.Window)
		validIdx := 0
		for _, t := range p.restartTimes {
			if t.After(windowStart) {
				p.restartTimes[validIdx] = t
				validIdx++
			}
		}
		p.restartTimes = p.restartTimes[:validIdx]

		if len(p.restartTimes) > p.spec.Restart.MaxRestarts {
			p.info.State = StateCrashLoop
			p.info.CrashLoop = true
			p.info.Err = fmt.Sprintf("crash loop: exceeded %d restarts within %s", p.spec.Restart.MaxRestarts, p.spec.Restart.Window)
			p.mu.Unlock()
			m.publish(p)
			m.log.Error("child process entered crash loop", "pid", p.info.ID, "name", p.spec.Name, "restarts", len(p.restartTimes))
			return
		}

		// Exponential backoff
		attempts := len(p.restartTimes) - 1
		backoff := p.spec.Restart.InitialBackoff
		for i := 0; i < attempts; i++ {
			backoff = time.Duration(float64(backoff) * p.spec.Restart.BackoffFactor)
			if backoff > p.spec.Restart.MaxBackoff {
				backoff = p.spec.Restart.MaxBackoff
				break
			}
		}

		p.info.State = StateRestarting
		p.info.RestartCount++
		p.mu.Unlock()
		m.publish(p)
		m.log.Info("child restarting after backoff", "pid", p.info.ID, "name", p.spec.Name, "backoff", backoff, "attempt", p.info.RestartCount)

		select {
		case <-p.masterCtx.Done():
			p.mu.Lock()
			p.info.State = StateStopped
			p.mu.Unlock()
			m.publish(p)
			return
		case <-time.After(backoff):
		}
	}
}

func (p *Process) isStopping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.userStopped || p.info.State == StateStopping
}

// Stop asks a process to stop and waits up to timeout for it to exit.
// It marks the process as user-stopped to guarantee that restart policies are suppressed.
func (m *Manager) Stop(id int32, timeout time.Duration) error {
	m.mu.Lock()
	p, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("process: no such process %d", id)
	}

	p.mu.Lock()
	p.userStopped = true
	state := p.info.State
	switch state {
	case StateStopped, StateFailed, StateCrashLoop:
		p.mu.Unlock()
		select {
		case <-p.done:
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("process: %d is stopped but done chan not closed within %s", id, timeout)
		}
	case StateStopping:
		p.mu.Unlock()
		select {
		case <-p.done:
			return nil
		case <-time.After(timeout):
			return fmt.Errorf("process: %d is stopping but has not exited within %s", id, timeout)
		}
	}

	p.info.State = StateStopping
	cmd := p.cmd
	p.mu.Unlock()
	m.publish(p)

	p.masterCancel()
	if cmd != nil {
		_ = platform.KillProcessTree(cmd)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("process: %d (%s) did not stop within %s", id, p.Info().Name, timeout)
	}
}

// StopAll stops every live process, best effort. Used at shutdown.
// It sets shuttingDown = true to ensure no supervised processes restart.
func (m *Manager) StopAll(timeout time.Duration) {
	m.BeginShutdown()

	for _, info := range m.List() {
		if info.State != StateRunning && info.State != StateStarting && info.State != StateRestarting {
			continue
		}
		if err := m.Stop(info.ID, timeout); err != nil {
			m.log.Warn("process shutdown problem", "pid", info.ID, "err", err)
		}
	}
}

// Shutdown stops all processes and marks the manager as shutting down.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.StopAll(timeout)
}

func (m *Manager) isShuttingDown() bool {
	return m.shuttingDown.Load()
}

// IsShuttingDown reports whether the manager is draining (set by
// BeginShutdown or StopAll). Coordinated start paths — application launch —
// consult it to refuse new work during the shutdown sequence.
func (m *Manager) IsShuttingDown() bool {
	return m.shuttingDown.Load()
}

// BeginShutdown marks the manager as draining before any process is
// stopped: supervised processes stop restarting, StartInProc/StartChild
// registrations are refused, and callers checking IsShuttingDown see the
// shutdown across its whole sequence. StopAll implies it.
func (m *Manager) BeginShutdown() {
	m.shuttingDown.Store(true)
}

// Get returns a process by ID.
func (m *Manager) Get(id int32) (*Process, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.procs[id]
	return p, ok
}

// List returns snapshots of all known processes, sorted by ID. Exited
// processes remain listed until reaping.
func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.procs))
	for _, p := range m.procs {
		out = append(out, p.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Count returns the number of live (running or starting) processes.
func (m *Manager) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.procs {
		switch p.Info().State {
		case StateRunning, StateStarting, StateRestarting:
			n++
		}
	}
	return n
}

func (m *Manager) recordHistory(p *Process) {
	m.mu.Lock()
	defer m.mu.Unlock()

	info := p.Info()
	duration := ""
	if !info.StartedAt.IsZero() {
		if !info.ExitedAt.IsZero() {
			duration = formatDuration(info.ExitedAt.Sub(info.StartedAt))
		} else {
			duration = formatDuration(time.Since(info.StartedAt))
		}
	}

	entry := HistoryEntry{
		ID:           info.ID,
		Name:         info.Name,
		Kind:         info.Kind,
		State:        info.State,
		SessionID:    info.SessionID,
		User:         info.User,
		ExitCode:     info.ExitCode,
		Err:          info.Err,
		StartedAt:    info.StartedAt,
		ExitedAt:     info.ExitedAt,
		Duration:     duration,
		RestartCount: info.RestartCount,
		CrashLoop:    info.CrashLoop,
		Resources:    info.Resources,
		Isolation:    info.Isolation,
	}

	m.history = append(m.history, entry)
	if len(m.history) > m.maxHistory {
		m.history = m.history[len(m.history)-m.maxHistory:]
	}
}

// History returns snapshots of exited and reaped processes, up to maxHistory entries,
// ordered newest first.
func (m *Manager) History() []HistoryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]HistoryEntry, len(m.history))
	for i, e := range m.history {
		out[len(m.history)-1-i] = e
	}
	return out
}

// HistoryByID searches the bounded history buffer for a process with the given ID.
func (m *Manager) HistoryByID(id int32) (HistoryEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.history) - 1; i >= 0; i-- {
		if m.history[i].ID == id {
			return m.history[i], true
		}
	}
	return HistoryEntry{}, false
}

// Reap removes inactive processes (stopped, failed, or crashloop) from the active process table.
// Returns the count of processes reaped.
func (m *Manager) Reap() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	reaped := 0
	for id, p := range m.procs {
		p.mu.Lock()
		state := p.info.State
		p.mu.Unlock()
		switch state {
		case StateStopped, StateFailed, StateCrashLoop:
			delete(m.procs, id)
			reaped++
		}
	}
	m.reapedCount += reaped
	return reaped
}

func (m *Manager) autoReap() {
	m.mu.Lock()
	defer m.mu.Unlock()
	inactiveCount := 0
	for _, p := range m.procs {
		p.mu.Lock()
		st := p.info.State
		p.mu.Unlock()
		if st == StateStopped || st == StateFailed || st == StateCrashLoop {
			inactiveCount++
		}
	}
	limit := m.maxHistory * 2
	if limit < 10 {
		limit = 10
	}
	if inactiveCount > limit {
		for id, p := range m.procs {
			p.mu.Lock()
			st := p.info.State
			p.mu.Unlock()
			if st == StateStopped || st == StateFailed || st == StateCrashLoop {
				delete(m.procs, id)
				m.reapedCount++
				inactiveCount--
				if inactiveCount <= m.maxHistory {
					break
				}
			}
		}
	}
}

// SetMaxHistory configures the maximum number of history entries retained.
func (m *Manager) SetMaxHistory(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n <= 0 {
		n = DefaultMaxHistory
	}
	m.maxHistory = n
	if len(m.history) > m.maxHistory {
		m.history = m.history[len(m.history)-m.maxHistory:]
	}
}

// Logs returns a snapshot of process logs and stream diagnostics.
func (p *Process) Logs() Logs {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	var stdoutDiag, stderrDiag, combinedDiag StreamDiagnostics
	if p.stdout != nil {
		stdoutDiag = p.stdout.Snapshot()
	}
	if p.stderr != nil {
		stderrDiag = p.stderr.Snapshot()
	}
	if p.combined != nil {
		combinedDiag = p.combined.Snapshot()
	}
	duration := ""
	if !info.StartedAt.IsZero() {
		if !info.ExitedAt.IsZero() {
			duration = formatDuration(info.ExitedAt.Sub(info.StartedAt))
		} else {
			duration = formatDuration(time.Since(info.StartedAt))
		}
	}
	return Logs{
		ID:        info.ID,
		Name:      info.Name,
		Kind:      info.Kind,
		State:     info.State,
		ExitCode:  info.ExitCode,
		StartedAt: info.StartedAt,
		ExitedAt:  info.ExitedAt,
		Duration:  duration,
		Stdout:    stdoutDiag,
		Stderr:    stderrDiag,
		Combined:  combinedDiag,
	}
}

// Logs returns the captured output and diagnostics for a process.
func (m *Manager) Logs(id int32) (Logs, bool) {
	p, ok := m.Get(id)
	if ok {
		return p.Logs(), true
	}
	entry, ok := m.HistoryByID(id)
	if !ok {
		return Logs{}, false
	}
	return Logs{
		ID:        entry.ID,
		Name:      entry.Name,
		Kind:      entry.Kind,
		State:     entry.State,
		ExitCode:  entry.ExitCode,
		StartedAt: entry.StartedAt,
		ExitedAt:  entry.ExitedAt,
		Duration:  entry.Duration,
	}, true
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}
