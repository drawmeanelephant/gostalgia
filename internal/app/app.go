// Package app implements the standard-library-only application registry and
// lifecycle manager. Application-facing contracts live in gostalgia/sdk.
package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
	"gostalgia/sdk"
)

type Manifest = sdk.Manifest
type Instance = sdk.Instance
type Factory = sdk.Factory

// Event is published when an application launches or exits.
type Event struct {
	ID    string `json:"id"`
	PID   int32  `json:"pid"`
	State string `json:"state"`
	Err   string `json:"error,omitempty"`
}

func (Event) Type() string { return "app.state" }

type Status struct {
	Manifest Manifest `json:"manifest"`
	Running  bool     `json:"running"`
	PID      int32    `json:"pid,omitempty"`
}

// Registry holds compiled-in factories and installed manifests. JSON alone
// cannot install executable code; builtin registrations win over disk copies.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
	manifests map[string]Manifest
}

func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}, manifests: map[string]Manifest{}}
}

func cloneManifest(m Manifest) Manifest {
	m.Permissions = append([]string(nil), m.Permissions...)
	m.PathGrants = append([]sdk.PathGrant(nil), m.PathGrants...)
	m.Args = append([]string(nil), m.Args...)
	m.DocumentTypes = append([]string(nil), m.DocumentTypes...)
	return m
}

func (r *Registry) RegisterBuiltin(m Manifest, f Factory) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("app: %s: factory is required", m.ID)
	}
	if m.Mode != "" && m.Mode != sdk.ModeInProc {
		return fmt.Errorf("app: compiled-in factories require inproc mode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.manifests[m.ID]; dup {
		return fmt.Errorf("app: %s is already registered", m.ID)
	}
	if _, dup := r.factories[m.Entrypoint]; dup {
		return fmt.Errorf("app: entrypoint %q is already registered", m.Entrypoint)
	}
	r.manifests[m.ID] = cloneManifest(m)
	r.factories[m.Entrypoint] = f
	return nil
}

// RegisterExternal registers an application manifest configured for out-of-process execution.
func (r *Registry) RegisterExternal(m Manifest) error {
	if m.Mode == "" {
		m.Mode = sdk.ModeExternal
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Mode != sdk.ModeExternal {
		return fmt.Errorf("app: external registration requires external mode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.manifests[m.ID]; dup {
		return fmt.Errorf("app: %s is already registered", m.ID)
	}
	r.manifests[m.ID] = cloneManifest(m)
	return nil
}

// ReplaceExternal changes only an external app, never a compiled-in factory.
func (r *Registry) ReplaceExternal(m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Mode != sdk.ModeExternal {
		return fmt.Errorf("app: replacement requires external mode")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.manifests[m.ID]
	if !ok || old.Mode != sdk.ModeExternal {
		return fmt.Errorf("app: %s is not a registered external application", m.ID)
	}
	r.manifests[m.ID] = cloneManifest(m)
	return nil
}

func (r *Registry) RemoveExternal(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.manifests[id]; ok && m.Mode == sdk.ModeExternal {
		delete(r.manifests, id)
	}
}

// LoadManifests validates the entire directory before adding any documents.
func (r *Registry) LoadManifests(fsys fs.FS, dir string) (int, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return 0, fmt.Errorf("app: read manifests from %s: %w", dir, err)
	}
	var pending []Manifest
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return 0, fmt.Errorf("app: read %s/%s: %w", dir, e.Name(), err)
		}
		m, err := sdk.ParseManifest(b)
		if err != nil {
			return 0, fmt.Errorf("app: %s/%s: %w", dir, e.Name(), err)
		}
		pending = append(pending, m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	loaded := 0
	for _, m := range pending {
		if _, exists := r.manifests[m.ID]; !exists {
			r.manifests[m.ID] = cloneManifest(m)
			loaded++
		}
	}
	return loaded, nil
}

func (r *Registry) Manifest(id string) (Manifest, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.manifests[id]
	return cloneManifest(m), ok
}

func (r *Registry) Manifests() []Manifest {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Manifest, 0, len(r.manifests))
	for _, m := range r.manifests {
		out = append(out, cloneManifest(m))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) factory(entrypoint string) (Factory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.factories[entrypoint]
	return f, ok
}

// Manager reserves the app id before invoking user code. No manager lock is
// held across application callbacks or synchronous bus publications.
type Manager struct {
	reg         *Registry
	procs       *process.Manager
	router      *ipc.Router
	bus         *events.Bus
	tokens      *security.TokenStore
	grants      *vfs.GrantStore
	policy      *security.PolicyStore
	user        security.User
	root        string // environment root; masked from sandboxed children
	log         *slog.Logger
	mu          sync.Mutex
	running     map[string]*runningApp
	maintenance map[string]bool
}

type runningApp struct {
	pid        int32 // zero while initializing
	token      string
	cleanupErr error // written before process Done closes
	done       chan struct{}
}

func NewManager(reg *Registry, procs *process.Manager, router *ipc.Router, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		reg:         reg,
		procs:       procs,
		router:      router,
		bus:         bus,
		tokens:      security.NewTokenStore(),
		log:         log,
		running:     map[string]*runningApp{},
		maintenance: map[string]bool{},
	}
}

// SetGrantStore configures the grant store used for scoped filesystem authorizations.
func (m *Manager) SetGrantStore(grants *vfs.GrantStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants = grants
}

// GrantStore returns the configured grant store.
func (m *Manager) GrantStore() *vfs.GrantStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.grants
}

// SetTokenStore configures the token store used for application credentials.
func (m *Manager) SetTokenStore(tokens *security.TokenStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens = tokens
}

// SetRoot configures the host environment root. It is added to
// MaskedPaths for sandboxed children so the operator token in
// runtime.json stays unreachable even if OS sandboxing of reads is
// otherwise incomplete.
func (m *Manager) SetRoot(root string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.root = root
}

// Root returns the configured environment root ("" if unset).
func (m *Manager) Root() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root
}

// SetPolicyStore configures the operator policy store.
func (m *Manager) SetPolicyStore(policy *security.PolicyStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policy = policy
}

// PolicyStore returns the configured operator policy store.
func (m *Manager) PolicyStore() *security.PolicyStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

// TokenStore returns the token store.
func (m *Manager) TokenStore() *security.TokenStore {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens
}

// Registry returns the manifest registry.
func (m *Manager) Registry() *Registry {
	return m.reg
}

// AppToken returns the launch-bound credential issued for a running app.
func (m *Manager) AppToken(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ra, ok := m.running[id]
	if !ok || ra.token == "" {
		return "", false
	}
	return ra.token, true
}

// SetUser configures the active user for issued app tokens and IPC context.
func (m *Manager) SetUser(user security.User) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.user = user
}

// User returns the configured user identity (or guest fallback).
func (m *Manager) User() security.User {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.user.Name == "" {
		return security.User{Name: "guest"}
	}
	return m.user
}

// invoke turns lifecycle panics into process/launch errors, never success.
func invoke(phase string, fn func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("app: %s panicked: %v", phase, p)
		}
	}()
	return fn()
}

// Launch runs Factory -> Init -> Run -> Stop. ctx owns the instance lifetime.
// IPC launchers must supply a runtime-lifetime context, not a request deadline.
func (m *Manager) Launch(ctx context.Context, id string) (*process.Process, error) {
	// Reserve before reading the registry, so maintenance cannot race a stale launch.
	m.mu.Lock()
	if m.procs != nil && m.procs.IsShuttingDown() {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: environment is shutting down")
	}
	if m.maintenance[id] {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: %s is undergoing package maintenance", id)
	}
	if existing, dup := m.running[id]; dup {
		pid := existing.pid
		m.mu.Unlock()
		return nil, fmt.Errorf("app: %s is already running or initializing (pid %d)", id, pid)
	}
	man, ok := m.reg.Manifest(id)
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: unknown application %q", id)
	}
	ra := &runningApp{done: make(chan struct{})}
	m.running[id] = ra
	m.mu.Unlock()
	switch man.Mode {
	case "", sdk.ModeInProc:
		return m.launchInProc(ctx, man, ra)
	case sdk.ModeExternal:
		return m.launchExternal(ctx, man, ra)
	default:
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
		close(ra.done)
		return nil, fmt.Errorf("app: unsupported mode %q", man.Mode)
	}
}

func (m *Manager) launchInProc(ctx context.Context, man Manifest, ra *runningApp) (*process.Process, error) {
	id := man.ID
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(ra.done) }) }
	// A failed launch never gets a run goroutine to close ra.done; close it
	// here so a racing Stop returns promptly instead of reporting a spurious
	// cleanup timeout for an app that no longer exists.
	failed := true
	defer func() {
		if failed {
			closeDone()
		}
	}()
	factory, ok := m.reg.factory(man.Entrypoint)
	if !ok {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("app: no factory for entrypoint %q", man.Entrypoint)
	}
	forget := func() {
		m.mu.Lock()
		delete(m.running, id)
		m.mu.Unlock()
	}

	var inst Instance
	if err := invoke("factory", func() (err error) { inst, err = factory(); return err }); err != nil {
		forget()
		return nil, fmt.Errorf("app: create %s: %w", id, err)
	}
	if inst == nil {
		forget()
		return nil, fmt.Errorf("app: %s: factory returned nil", id)
	}

	var appToken string
	caps := append([]string(nil), man.Permissions...)
	if m.tokens != nil {
		var tokErr error
		appToken, tokErr = m.tokens.IssueAppToken(id, 0, "", m.User(), caps...)
		if tokErr != nil {
			forget()
			return nil, fmt.Errorf("app: issue token for %s: %w", id, tokErr)
		}
		m.mu.Lock()
		ra.token = appToken
		m.mu.Unlock()
	}

	life, cancel := context.WithCancel(ctx)
	base := "app/" + id + "/"
	var mu sync.Mutex
	initializing, active := true, true
	registered := false
	routes := map[string]ipc.Handler{}
	var inflight sync.WaitGroup

	// Always replace incoming caps and principal: an operator calling this app
	// must not lend the app operator privileges (the confused-deputy boundary).
	scope := func(parent context.Context) (context.Context, func()) {
		c, stop := context.WithCancel(parent)
		detach := context.AfterFunc(life, stop)
		c = ipc.WithCapabilities(c, security.NewCapabilities(caps...))
		m.mu.Lock()
		curPID := ra.pid
		m.mu.Unlock()
		c = ipc.WithPrincipal(c, security.AppPrincipal(id, curPID, "", m.User()))
		return c, func() { detach(); stop() }
	}
	call := func(parent context.Context, method string, params, out any) error {
		if err := life.Err(); err != nil {
			return fmt.Errorf("app: instance stopped: %w", err)
		}
		c, stop := scope(parent)
		defer stop()
		if err := ipc.RequireCap(c, security.CapIPC); err != nil {
			return err
		}
		raw, err := ipc.Encode(params)
		if err != nil {
			return err
		}
		if err := c.Err(); err != nil {
			return err
		}
		resp := m.router.Dispatch(c, ipc.Request{Method: method, Params: raw})
		if !resp.OK {
			return errors.New(resp.Error)
		}
		if out != nil {
			return json.Unmarshal(resp.Data, out)
		}
		return nil
	}
	handle := func(name string, h sdk.Handler) error {
		mu.Lock()
		defer mu.Unlock()
		if !initializing {
			return fmt.Errorf("app: routes may only be declared during Init")
		}
		if !security.NewCapabilities(caps...).Has(security.CapIPC) {
			return fmt.Errorf("permission denied: missing capability %q", security.CapIPC)
		}
		if !validRouteName(name) {
			return fmt.Errorf("app: invalid route name %q", name)
		}
		method := base + name
		if _, dup := routes[method]; dup {
			return fmt.Errorf("app: duplicate route %q", name)
		}
		if len(routes) >= maxAppRoutes {
			return fmt.Errorf("app: too many routes (limit %d)", maxAppRoutes)
		}
		routes[method] = func(parent context.Context, req ipc.Request) (any, error) {
			if err := ipc.RequireCap(parent, security.CapIPC); err != nil {
				return nil, err
			}
			mu.Lock()
			if !active {
				mu.Unlock()
				return nil, fmt.Errorf("app: instance stopped")
			}
			inflight.Add(1)
			mu.Unlock()
			defer inflight.Done()
			c, stop := scope(parent)
			defer stop()
			if err := c.Err(); err != nil {
				return nil, err
			}
			return h(c, req.Params)
		}
		return nil
	}
	cleanup := func() error {
		mu.Lock()
		active, initializing = false, false
		mu.Unlock()
		cancel()
		if m.tokens != nil && ra.token != "" {
			m.tokens.Revoke(ra.token)
		}
		// Session-bound grants (document handoffs) die with this run,
		// like the launch-bound token above.
		if m.grants != nil {
			m.grants.RevokeAppSession(id)
		}
		if registered {
			m.router.UnhandlePrefix(base)
		}
		stopCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		drained := make(chan struct{})
		go func() { inflight.Wait(); close(drained) }()
		var drainErr error
		select {
		case <-drained:
		case <-stopCtx.Done():
			drainErr = fmt.Errorf("app: handlers did not drain: %w", stopCtx.Err())
		}
		return errors.Join(drainErr, invoke("Stop", func() error { return inst.Stop(stopCtx) }))
	}
	lc := sdk.NewContext(cloneManifest(man), m.log.With("app", id), call, handle)
	err := invoke("Init", func() error { return inst.Init(lc) })
	mu.Lock()
	initializing = false
	mu.Unlock()
	if err == nil {
		err = life.Err()
	}
	if err == nil {
		err = m.router.HandleBatch(routes)
	}
	if err != nil {
		stopErr := cleanup()
		forget()
		return nil, fmt.Errorf("app: init %s: %w", id, errors.Join(err, stopErr))
	}

	registered = true
	ready := make(chan struct{})
	proc, err := m.procs.StartInProc(life, process.Spec{
		Name: id,
		User: m.User().Name,
		Caps: caps,
		Policy: platform.ExecutionPolicy{
			Isolation: platform.IsolationInProc,
		},
	}, func(p *process.Process) (runErr error) {
		<-ready
		defer func() {
			ra.cleanupErr = cleanup()
			runErr = errors.Join(runErr, ra.cleanupErr)
			forget()
			closeDone()
			msg := ""
			if runErr != nil {
				msg = runErr.Error()
			}
			m.bus.Publish("app", Event{ID: id, PID: p.ID(), State: "exited", Err: msg})
		}()
		return invoke("Run", func() error { return inst.Run(p.Context()) })
	})
	if err != nil {
		stopErr := cleanup()
		forget()
		return nil, errors.Join(err, stopErr)
	}
	failed = false // the run goroutine owns ra.done from here on
	m.mu.Lock()
	ra.pid = proc.ID()
	m.mu.Unlock()
	if m.tokens != nil && appToken != "" {
		m.tokens.BindProcess(appToken, proc.ID())
	}
	// Release Run before publishing: event subscribers may synchronously Stop.
	close(ready)
	m.bus.Publish("app", Event{ID: id, PID: proc.ID(), State: "launched"})
	m.log.Info("application launched", "app", id, "pid", proc.ID())
	return proc, nil
}

type rpcWireMessage struct {
	ID     int64           `json:"id"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	OK     bool            `json:"ok,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func writeWireMessage(conn io.Writer, mu *sync.Mutex, msg rpcWireMessage) error {
	mu.Lock()
	defer mu.Unlock()
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = conn.Write(b)
	return err
}

// maxWireLine bounds one child wire frame, matching the 4 MiB line cap the
// ipc server enforces (internal/ipc/server.go). maxChildInFlight bounds
// concurrent child-initiated dispatches, matching the ipc server's
// per-connection in-flight limit.
const (
	maxWireLine      = 4 << 20
	maxChildInFlight = 32
)

// maxAppRoutes bounds how many routes one application may register, both
// for in-proc apps (declared during Init) and external apps (declared in
// the ready handshake). Real applications declare a handful; the bound
// keeps a buggy or hostile external app from amplifying one <=4 MiB ready
// frame into hundreds of megabytes of route-table memory. maxRouteNameLen
// bounds a single route name; names are identifiers matching
// [a-z][a-z0-9_-]*, the same shape sdk enforces for entrypoints and
// in-proc route declarations.
const (
	maxAppRoutes    = 1024
	maxRouteNameLen = 64
)

var errWireFrameTooLarge = errors.New("app: wire frame exceeds 4 MiB limit")

// validRouteName reports whether name is a valid local route identifier:
// [a-z] followed by [a-z0-9_-], at most maxRouteNameLen bytes.
func validRouteName(name string) bool {
	if len(name) == 0 || len(name) > maxRouteNameLen || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// readWireLine reads one newline-delimited frame, aborting once it exceeds
// maxWireLine. Unlike ReadBytes it cannot buffer an unterminated line
// without bound.
func readWireLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		frag, err := r.ReadSlice('\n')
		line = append(line, frag...)
		if err == bufio.ErrBufferFull {
			if len(line) > maxWireLine {
				return nil, errWireFrameTooLarge
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(line) > maxWireLine {
			return nil, errWireFrameTooLarge
		}
		return line, nil
	}
}

// childWireLoop pumps newline-delimited frames from an external app child to
// the runtime router until the connection fails. An oversized frame is a
// protocol violation and closes the connection.
//
// Child requests dispatch under a bounded in-flight budget. Overflow is
// rejected with an error response rather than queued on this goroutine: the
// loop is the only reader, and call responses share the same connection, so
// a reader stalled on a full budget would deadlock handlers that are
// themselves waiting on child replies.
func childWireLoop(conn net.Conn, reader *bufio.Reader, writeMu *sync.Mutex, pendingMu *sync.Mutex, pendingCalls map[int64]chan rpcWireMessage, dispatch func(rpcWireMessage) rpcWireMessage, log *slog.Logger) {
	// The wire is dead on exit: close the connection and fail every pending
	// call so route invokers surface the drop instead of hanging.
	defer func() {
		_ = conn.Close()
		pendingMu.Lock()
		for id, ch := range pendingCalls {
			delete(pendingCalls, id)
			close(ch)
		}
		pendingMu.Unlock()
	}()

	inFlight := make(chan struct{}, maxChildInFlight)
	for {
		l, rErr := readWireLine(reader)
		if rErr != nil {
			if errors.Is(rErr, errWireFrameTooLarge) {
				log.Warn("app: child sent oversized wire frame; closing connection", "err", rErr)
			} else if !errors.Is(rErr, io.EOF) && !errors.Is(rErr, net.ErrClosed) {
				log.Debug("app: child wire read failed", "err", rErr)
			}
			return
		}
		l = bytes.TrimSpace(l)
		if len(l) == 0 {
			continue
		}
		var msg rpcWireMessage
		if err := json.Unmarshal(l, &msg); err != nil {
			continue
		}
		if msg.Method == "" {
			pendingMu.Lock()
			ch, ok := pendingCalls[msg.ID]
			pendingMu.Unlock()
			if ok {
				select {
				case ch <- msg:
				default:
				}
			}
			continue
		}
		select {
		case inFlight <- struct{}{}:
		default:
			_ = writeWireMessage(conn, writeMu, rpcWireMessage{
				ID:    msg.ID,
				Error: "app: too many in-flight child requests",
			})
			continue
		}
		go func(req rpcWireMessage) {
			defer func() { <-inFlight }()
			_ = writeWireMessage(conn, writeMu, dispatch(req))
		}(msg)
	}
}

func (m *Manager) launchExternal(ctx context.Context, man Manifest, ra *runningApp) (*process.Process, error) {
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(ra.done) }) }
	// Every failure return below forgets the app but would leave ra.done
	// open forever; closing it here lets a racing Stop return promptly
	// instead of waiting out its whole timeout for a nonexistent app.
	failed := true
	defer func() {
		if failed {
			closeDone()
		}
	}()
	forget := func() {
		m.mu.Lock()
		delete(m.running, man.ID)
		m.mu.Unlock()
	}

	caps := append([]string(nil), man.Permissions...)
	var appToken string
	if m.tokens != nil {
		var tokErr error
		appToken, tokErr = m.tokens.IssueAppToken(man.ID, 0, "", m.User(), caps...)
		if tokErr != nil {
			forget()
			return nil, fmt.Errorf("app: issue token for %s: %w", man.ID, tokErr)
		}
		m.mu.Lock()
		ra.token = appToken
		m.mu.Unlock()
	}

	life, cancel := context.WithCancel(ctx)
	childArgs := append([]string(nil), man.Args...)
	execPath := man.Executable
	if runtime.GOOS == "windows" && filepath.Ext(execPath) == "" {
		if _, err := os.Stat(execPath + ".exe"); err == nil {
			execPath += ".exe"
		}
	}
	policy, pErr := platform.PolicyForIsolation(man.EffectiveIsolation())
	if pErr != nil {
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: resolve isolation for %s: %w", man.ID, pErr)
	}

	hasNetCap := false
	for _, c := range man.Permissions {
		if c == sdk.CapNetEgress {
			hasNetCap = true
			break
		}
	}
	m.mu.Lock()
	pStore := m.policy
	m.mu.Unlock()
	netAllowed := false
	if pStore != nil && pStore.Get().Network.Enabled {
		netAllowed = true
	}
	if hasNetCap && netAllowed {
		policy.DenyNetwork = false
	} else if policy.Isolation == platform.IsolationSandbox || policy.Isolation == platform.IsolationStrict {
		policy.DenyNetwork = true
	}

	childEnv := []string{
		"GOSTALGIA_APP_TOKEN=" + appToken,
		"GOSTALGIA_TOKEN=" + appToken,
		"GOSTALGIA_APP_ID=" + man.ID,
		fmt.Sprintf("GOSTALGIA_PROTOCOL_VERSION=%d", sdk.ProtocolVersion),
	}

	// Sandboxed children get their IPC channel as an inherited pre-connected
	// descriptor (fd 3): the sandbox profile can then deny every socket
	// connect/bind/listen, including remote unix sockets, which Seatbelt and
	// network namespaces cannot scope to a single path. Trusted children keep
	// the listener + dial flow (and Windows has no fd-passing anyway).
	sandboxed := policy.Isolation == platform.IsolationSandbox || policy.Isolation == platform.IsolationStrict
	var err error
	var conn net.Conn
	var childFile *os.File
	var ln net.Listener
	var endpoint string
	if sandboxed {
		conn, childFile, err = platform.ChildIPC()
		if err != nil {
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			return nil, fmt.Errorf("app: child ipc for %s: %w", man.ID, err)
		}
		childEnv = append(childEnv, "GOSTALGIA_IPC_FD=3")
		if root := m.Root(); root != "" {
			policy.MaskedPaths = append(policy.MaskedPaths, root)
		}
	} else {
		ln, endpoint, err = platform.ListenChildIPC(man.ID)
		if err != nil {
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			return nil, fmt.Errorf("app: listen child ipc for %s: %w", man.ID, err)
		}
		childEnv = append(childEnv, "GOSTALGIA_ENDPOINT="+endpoint)
	}

	// teardownChannel releases every half of the child channel on failure;
	// after the handshake succeeds the conn is owned by the pump and closed
	// by cleanup.
	dropConn := func() {
		if conn != nil {
			_ = conn.Close()
		}
		if childFile != nil {
			_ = childFile.Close()
			childFile = nil
		}
	}
	teardownChannel := func() {
		if ln != nil {
			_ = ln.Close()
		}
		dropConn()
		platform.RemoveChildSocket(endpoint)
	}

	spec := process.Spec{
		Name:   man.ID,
		User:   m.User().Name,
		Args:   append([]string{execPath}, childArgs...),
		Caps:   caps,
		Env:    childEnv,
		Policy: policy,
	}
	if childFile != nil {
		spec.ExtraFiles = []*os.File{childFile}
	}

	proc, err := m.procs.StartChild(life, spec)
	if err != nil {
		teardownChannel()
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: start %s: %w", man.ID, err)
	}
	// The child now holds its own copy of the IPC descriptor (ExtraFiles is
	// duped into the child at Start). Release the parent's copy so the peer
	// end is fully owned by the child — a dead child then produces EOF on
	// conn instead of a silent timeout.
	if childFile != nil {
		_ = childFile.Close()
		childFile = nil
	}

	m.mu.Lock()
	ra.pid = proc.ID()
	m.mu.Unlock()
	if m.tokens != nil && appToken != "" {
		m.tokens.BindProcess(appToken, proc.ID())
	}

	if ln != nil {
		type acceptRes struct {
			conn net.Conn
			err  error
		}
		acceptCh := make(chan acceptRes, 1)
		go func() {
			c, aErr := ln.Accept()
			acceptCh <- acceptRes{conn: c, err: aErr}
		}()

		select {
		case <-time.After(5 * time.Second):
			teardownChannel()
			_ = m.procs.Stop(proc.ID(), 2*time.Second)
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			return nil, fmt.Errorf("app: %s: handshake timed out waiting for connection", man.ID)
		case <-proc.Done():
			teardownChannel()
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			info := proc.Info()
			logs, _ := m.procs.Logs(proc.ID())
			detail := info.Err
			if logs.Stderr.Content != "" {
				detail += fmt.Sprintf(" (stderr: %s)", strings.TrimSpace(logs.Stderr.Content))
			}
			return nil, fmt.Errorf("app: %s exited before connecting: %s", man.ID, detail)
		case res := <-acceptCh:
			if res.err != nil {
				teardownChannel()
				_ = m.procs.Stop(proc.ID(), 2*time.Second)
				if m.tokens != nil && appToken != "" {
					m.tokens.Revoke(appToken)
				}
				cancel()
				forget()
				return nil, fmt.Errorf("app: %s: accept connection: %w", man.ID, res.err)
			}
			conn = res.conn
		}
		_ = ln.Close()
		platform.RemoveChildSocket(endpoint)
	} else {
		// The fd channel is already connected; the child may still die
		// before handshaking, so honor that case up front.
		select {
		case <-proc.Done():
			teardownChannel()
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			info := proc.Info()
			logs, _ := m.procs.Logs(proc.ID())
			detail := info.Err
			if logs.Stderr.Content != "" {
				detail += fmt.Sprintf(" (stderr: %s)", strings.TrimSpace(logs.Stderr.Content))
			}
			return nil, fmt.Errorf("app: %s exited before connecting: %s", man.ID, detail)
		default:
		}
	}

	var writeMu sync.Mutex
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)

	// Read auth request
	line, err := readWireLine(reader)
	if err != nil {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		logs, _ := m.procs.Logs(proc.ID())
		detail := ""
		if logs.Stderr.Content != "" {
			detail = fmt.Sprintf(" (stderr: %s)", strings.TrimSpace(logs.Stderr.Content))
		}
		return nil, fmt.Errorf("app: %s: read auth handshake: %w%s", man.ID, err, detail)
	}
	var authReq rpcWireMessage
	if err := json.Unmarshal(line, &authReq); err != nil || authReq.Method != "auth" {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: expected auth method, got %q", man.ID, authReq.Method)
	}
	var authParams struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(authReq.Params, &authParams)
	if appToken == "" || authParams.Token == "" ||
		subtle.ConstantTimeCompare([]byte(authParams.Token), []byte(appToken)) != 1 {
		_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
			ID:    authReq.ID,
			OK:    false,
			Error: "invalid application token",
		})
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: invalid token in handshake", man.ID)
	}
	if err := writeWireMessage(conn, &writeMu, rpcWireMessage{
		ID:   authReq.ID,
		OK:   true,
		Data: json.RawMessage(`{"authenticated":true}`),
	}); err != nil {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: write auth response: %w", man.ID, err)
	}

	// Read app/ready request
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err = readWireLine(reader)
	if err != nil {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: read ready handshake: %w", man.ID, err)
	}
	var readyReq rpcWireMessage
	if err := json.Unmarshal(line, &readyReq); err != nil || readyReq.Method != "app/ready" {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: expected app/ready method, got %q", man.ID, readyReq.Method)
	}
	var readyParams struct {
		ProtocolVersion int      `json:"protocol_version"`
		AppID           string   `json:"app_id"`
		Routes          []string `json:"routes"`
	}
	if err := json.Unmarshal(readyReq.Params, &readyParams); err != nil {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: invalid ready params: %w", man.ID, err)
	}
	if readyParams.ProtocolVersion != sdk.ProtocolVersion {
		_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
			ID:    readyReq.ID,
			OK:    false,
			Error: fmt.Sprintf("unsupported protocol version %d", readyParams.ProtocolVersion),
		})
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: unsupported protocol version %d", man.ID, readyParams.ProtocolVersion)
	}
	if readyParams.AppID != "" && readyParams.AppID != man.ID {
		_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
			ID:    readyReq.ID,
			OK:    false,
			Error: fmt.Sprintf("mismatched app_id %q", readyParams.AppID),
		})
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: mismatched app_id %q", man.ID, readyParams.AppID)
	}
	if len(readyParams.Routes) > maxAppRoutes {
		_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
			ID:    readyReq.ID,
			OK:    false,
			Error: fmt.Sprintf("too many routes declared (%d > %d)", len(readyParams.Routes), maxAppRoutes),
		})
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: too many routes declared (%d > %d)", man.ID, len(readyParams.Routes), maxAppRoutes)
	}
	for _, rName := range readyParams.Routes {
		if !validRouteName(rName) {
			_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
				ID:    readyReq.ID,
				OK:    false,
				Error: fmt.Sprintf("invalid route name %q", rName),
			})
			dropConn()
			_ = m.procs.Stop(proc.ID(), 2*time.Second)
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			return nil, fmt.Errorf("app: %s: invalid route name %q", man.ID, rName)
		}
	}
	if len(readyParams.Routes) > 0 && !security.NewCapabilities(caps...).Has(security.CapIPC) {
		_ = writeWireMessage(conn, &writeMu, rpcWireMessage{
			ID:    readyReq.ID,
			OK:    false,
			Error: fmt.Sprintf("permission denied: missing capability %q", security.CapIPC),
		})
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: permission denied: missing capability %q", man.ID, security.CapIPC)
	}
	if err := writeWireMessage(conn, &writeMu, rpcWireMessage{
		ID:   readyReq.ID,
		OK:   true,
		Data: json.RawMessage(`{"ready":true}`),
	}); err != nil {
		dropConn()
		_ = m.procs.Stop(proc.ID(), 2*time.Second)
		if m.tokens != nil && appToken != "" {
			m.tokens.Revoke(appToken)
		}
		cancel()
		forget()
		return nil, fmt.Errorf("app: %s: write ready response: %w", man.ID, err)
	}
	_ = conn.SetReadDeadline(time.Time{})

	base := "app/" + man.ID + "/"
	var mu sync.Mutex
	active := true
	registered := false
	routes := map[string]ipc.Handler{}
	var inflight sync.WaitGroup
	var msgSeq int64 = 1000
	var pendingMu sync.Mutex
	pendingCalls := map[int64]chan rpcWireMessage{}

	scope := func(parent context.Context) (context.Context, func()) {
		c, stop := context.WithCancel(parent)
		detach := context.AfterFunc(life, stop)
		c = ipc.WithCapabilities(c, security.NewCapabilities(caps...))
		m.mu.Lock()
		curPID := ra.pid
		m.mu.Unlock()
		c = ipc.WithPrincipal(c, security.AppPrincipal(man.ID, curPID, "", m.User()))
		return c, func() { detach(); stop() }
	}

	for _, rName := range readyParams.Routes {
		routeMethod := base + rName
		localName := rName
		routes[routeMethod] = func(parent context.Context, req ipc.Request) (any, error) {
			if err := ipc.RequireCap(parent, security.CapIPC); err != nil {
				return nil, err
			}
			mu.Lock()
			if !active {
				mu.Unlock()
				return nil, fmt.Errorf("app: instance stopped")
			}
			inflight.Add(1)
			mu.Unlock()
			defer inflight.Done()

			c, stop := scope(parent)
			defer stop()
			if err := c.Err(); err != nil {
				return nil, err
			}

			callID := atomic.AddInt64(&msgSeq, 1)
			resCh := make(chan rpcWireMessage, 1)
			pendingMu.Lock()
			pendingCalls[callID] = resCh
			pendingMu.Unlock()
			defer func() {
				pendingMu.Lock()
				delete(pendingCalls, callID)
				pendingMu.Unlock()
			}()

			if err := writeWireMessage(conn, &writeMu, rpcWireMessage{
				ID:     callID,
				Method: localName,
				Params: req.Params,
			}); err != nil {
				return nil, fmt.Errorf("app: send to child: %w", err)
			}

			select {
			case <-c.Done():
				return nil, c.Err()
			case <-life.Done():
				return nil, fmt.Errorf("app: instance stopped")
			case res, ok := <-resCh:
				if !ok {
					return nil, fmt.Errorf("app: connection closed")
				}
				if !res.OK {
					return nil, errors.New(res.Error)
				}
				if len(res.Data) > 0 && !bytes.Equal(res.Data, []byte("null")) {
					return json.RawMessage(res.Data), nil
				}
				return nil, nil
			}
		}
	}

	if len(routes) > 0 {
		if err := m.router.HandleBatch(routes); err != nil {
			dropConn()
			_ = m.procs.Stop(proc.ID(), 2*time.Second)
			if m.tokens != nil && appToken != "" {
				m.tokens.Revoke(appToken)
			}
			cancel()
			forget()
			return nil, fmt.Errorf("app: %s: handle routes: %w", man.ID, err)
		}
		registered = true
	}

	var cleanupOnce sync.Once
	cleanup := func() error {
		cleanupOnce.Do(func() {
			mu.Lock()
			active = false
			mu.Unlock()
			cancel()
			dropConn()
			if m.tokens != nil && ra.token != "" {
				m.tokens.Revoke(ra.token)
			}
			// Session-bound grants (document handoffs) die with this run.
			if m.grants != nil {
				m.grants.RevokeAppSession(man.ID)
			}
			if registered {
				m.router.UnhandlePrefix(base)
			}
			platform.RemoveChildSocket(endpoint)
			drained := make(chan struct{})
			go func() { inflight.Wait(); close(drained) }()
			select {
			case <-drained:
			case <-time.After(3 * time.Second):
			}
		})
		return nil
	}

	dispatch := func(req rpcWireMessage) rpcWireMessage {
		childCtx := ipc.WithCapabilities(life, security.NewCapabilities(caps...))
		m.mu.Lock()
		curPID := ra.pid
		m.mu.Unlock()
		childCtx = ipc.WithPrincipal(childCtx, security.AppPrincipal(man.ID, curPID, "", m.User()))
		resp := m.router.Dispatch(childCtx, ipc.Request{ID: req.ID, Method: req.Method, Params: req.Params})
		return rpcWireMessage{
			ID:    resp.ID,
			OK:    resp.OK,
			Data:  resp.Data,
			Error: resp.Error,
		}
	}
	go childWireLoop(conn, reader, &writeMu, &pendingMu, pendingCalls, dispatch, m.log)

	failed = false // the proc-done watcher owns ra.done from here on
	go func() {
		<-proc.Done()
		ra.cleanupErr = cleanup()
		forget()
		closeDone()
		info := proc.Info()
		m.bus.Publish("app", Event{ID: man.ID, PID: proc.ID(), State: "exited", Err: info.Err})
		m.log.Info("external application exited", "app", man.ID, "pid", proc.ID(), "err", info.Err)
	}()

	m.bus.Publish("app", Event{ID: man.ID, PID: proc.ID(), State: "launched"})
	m.log.Info("external application launched", "app", man.ID, "pid", proc.ID())
	return proc, nil
}

// Stop waits for Run, handler draining, Stop, and route/state retraction.
func (m *Manager) Stop(id string, timeout time.Duration) error {
	m.mu.Lock()
	ra, ok := m.running[id]
	pid := int32(0)
	if ok {
		pid = ra.pid
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("app: %s is not running", id)
	}
	if pid == 0 {
		return fmt.Errorf("app: %s is still initializing", id)
	}
	if err := m.procs.Stop(pid, timeout); err != nil {
		return err
	}
	select {
	case <-ra.done:
	case <-time.After(timeout):
		return fmt.Errorf("app: %s cleanup timed out", id)
	}
	return ra.cleanupErr
}

// BeginMaintenance blocks new launches until release is called. Initializing
// instances fail closed; running instances lose credentials before being stopped.
func (m *Manager) BeginMaintenance(id string, timeout time.Duration) (release func(), err error) {
	m.mu.Lock()
	if m.maintenance[id] {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: %s is already undergoing maintenance", id)
	}
	ra, running := m.running[id]
	if running && ra.pid == 0 {
		m.mu.Unlock()
		return nil, fmt.Errorf("app: %s is still initializing", id)
	}
	m.maintenance[id] = true
	tokens := m.tokens
	grants := m.grants
	m.mu.Unlock()
	var once sync.Once
	release = func() {
		once.Do(func() {
			m.mu.Lock()
			delete(m.maintenance, id)
			m.mu.Unlock()
		})
	}
	if tokens != nil {
		tokens.RevokeApp(id)
	}
	if grants != nil {
		grants.RevokeAppSession(id)
	}
	if running {
		if err := m.Stop(id, timeout); err != nil {
			// A concurrent natural exit is already safe.
			if m.IsRunning(id) {
				release()
				return nil, err
			}
		}
	}
	return release, nil
}
func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.running[id]
	return ok
}

func (m *Manager) Running() map[string]int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int32, len(m.running))
	for id, ra := range m.running {
		out[id] = ra.pid
	}
	return out
}

func (m *Manager) List() []Status {
	running := m.Running()
	out := make([]Status, 0)
	for _, man := range m.reg.Manifests() {
		pid, ok := running[man.ID]
		out = append(out, Status{Manifest: man, Running: ok, PID: pid})
	}
	return out
}
