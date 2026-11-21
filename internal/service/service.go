// Package service is the service lifecycle framework. A service is a
// long-lived environment component with explicit states, dependencies,
// and ordered start/stop. The manager starts services in dependency
// order, rolls back on failure, and stops them in reverse.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/config"
	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
	"gostalgia/internal/session"
	"gostalgia/internal/vfs"
)

// State is a service lifecycle state.
type State string

const (
	StateCreated  State = "created"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
)

// Event is published on every service state transition.
type Event struct {
	Name  string `json:"name"`
	State State  `json:"state"`
	Err   string `json:"error,omitempty"`
}

func (Event) Type() string { return "svc.state" }

// stopTimeout bounds a single service's Stop.
const stopTimeout = 10 * time.Second

// NotificationRecord is a user-facing notification from a service or app.
type NotificationRecord struct {
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Level     string    `json:"level"`
	Source    string    `json:"source"`
	PID       int32     `json:"pid,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// NotificationSink records user-facing notifications. It is the boundary
// between core services and the experience-layer notification pipeline.
type NotificationSink interface {
	Record(NotificationRecord) bool
}

// Context carries the runtime wiring to services so they never import the
// runtime package itself. Fields are populated once, before StartAll.
type Context struct {
	Root          string                // environment root directory
	Version       string                // environment version
	Config        *config.Store         // system configuration
	Layered       *config.LayeredStore  // layered configuration store
	Events        *events.Bus           // event bus
	Log           *slog.Logger          // runtime logger
	Router        *ipc.Router           // IPC router (in-proc + socket)
	VFS           vfs.FS                // environment filesystem
	Procs         *process.Manager      // process manager
	Apps          *app.Manager          // application manager
	Sessions      *session.Manager      // session manager
	Profiles      *profile.Manager      // profile manager
	Tokens        *security.TokenStore  // credential store for IPC authentication
	Policy        *security.PolicyStore // operator policy store
	Notifications NotificationSink      // user-facing notification pipeline

	Token    string // Operator IPC auth token (recorded in runtime.json)
	Endpoint string // IPC endpoint, set by the ipc service once listening
	BootedAt time.Time
	Services *Manager // set by the runtime once the manager exists

	// Shutdown requests a graceful environment shutdown. It is safe to
	// call from any goroutine and blocks until shutdown completes.
	Shutdown func(reason string)
}

// Service is one environment service.
type Service interface {
	Name() string
	// Depends lists service names that must start first.
	Depends() []string
	// Init wires dependencies (called in dependency order, before Start).
	Init(ctx *Context) error
	// Start brings the service into running state.
	Start(ctx context.Context) error
	// Stop releases resources; must return within a bounded time.
	Stop(ctx context.Context) error
}

// Status is a service state snapshot.
type Status struct {
	Name    string   `json:"name"`
	State   State    `json:"state"`
	Depends []string `json:"depends,omitempty"`
}

// Manager owns the set of services and their lifecycle.
type Manager struct {
	ctx      *Context
	bus      *events.Bus
	log      *slog.Logger
	mu       sync.Mutex
	services map[string]Service
	states   map[string]State
	order    []string // registration order
	started  []string // start order (reverse of stop order)
}

func NewManager(ctx *Context, bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		ctx:      ctx,
		bus:      bus,
		log:      log,
		services: map[string]Service{},
		states:   map[string]State{},
	}
}

// Register adds a service. Names must be unique.
func (m *Manager) Register(s Service) error {
	name := s.Name()
	if name == "" {
		return errors.New("service: name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.services[name]; dup {
		return fmt.Errorf("service: %q is already registered", name)
	}
	m.services[name] = s
	m.states[name] = StateCreated
	m.order = append(m.order, name)
	return nil
}

// Get returns the registered service with the given name.
func (m *Manager) Get(name string) (Service, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.services[name]
	return s, ok
}

// StartAll initializes and starts every service in dependency order. On
// failure it stops the services already started (in reverse) and returns
// the error.
func (m *Manager) StartAll(ctx context.Context) error {
	order, err := m.topoOrder()
	if err != nil {
		return err
	}
	for _, name := range order {
		s := m.services[name]
		m.setState(name, StateStarting, nil)
		if err := s.Init(m.ctx); err != nil {
			m.setState(name, StateFailed, err)
			m.rollback()
			return fmt.Errorf("service %s: init: %w", name, err)
		}
		if err := s.Start(ctx); err != nil {
			m.setState(name, StateFailed, err)
			m.rollback()
			return fmt.Errorf("service %s: start: %w", name, err)
		}
		m.setState(name, StateRunning, nil)
		m.mu.Lock()
		m.started = append(m.started, name)
		m.mu.Unlock()
	}
	return nil
}

// StopAll stops every started service in reverse start order. Errors are
// collected; one bad service does not prevent the others from stopping.
func (m *Manager) StopAll(ctx context.Context) error {
	m.mu.Lock()
	started := make([]string, len(m.started))
	copy(started, m.started)
	m.mu.Unlock()

	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		name := started[i]
		m.setState(name, StateStopping, nil)
		sctx, cancel := context.WithTimeout(ctx, stopTimeout)
		err := m.services[name].Stop(sctx)
		cancel()
		if err != nil {
			m.setState(name, StateFailed, err)
			errs = append(errs, fmt.Errorf("service %s: stop: %w", name, err))
			m.log.Error("service failed to stop", "service", name, "err", err)
			continue
		}
		m.setState(name, StateStopped, nil)
	}
	m.mu.Lock()
	m.started = nil
	m.mu.Unlock()
	return errors.Join(errs...)
}

// State returns the current state of one service.
func (m *Manager) State(name string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[name]
	if !ok {
		return "", fmt.Errorf("service: no such service %q", name)
	}
	return state, nil
}

// Snapshot lists all services with their states, in registration order.
func (m *Manager) Snapshot() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.order))
	for _, name := range m.order {
		s := m.services[name]
		out = append(out, Status{Name: name, State: m.states[name], Depends: s.Depends()})
	}
	return out
}

func (m *Manager) setState(name string, state State, err error) {
	m.mu.Lock()
	m.states[name] = state
	m.mu.Unlock()
	ev := Event{Name: name, State: state}
	if err != nil {
		ev.Err = err.Error()
	}
	m.bus.Publish("service", ev)
	m.log.Info("service state", "service", name, "state", state, "err", ev.Err)
}

func (m *Manager) rollback() {
	m.mu.Lock()
	started := make([]string, len(m.started))
	copy(started, m.started)
	m.started = nil
	m.mu.Unlock()
	for i := len(started) - 1; i >= 0; i-- {
		name := started[i]
		m.setState(name, StateStopping, nil)
		sctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		err := m.services[name].Stop(sctx)
		cancel()
		if err != nil {
			m.setState(name, StateFailed, err)
		} else {
			m.setState(name, StateStopped, nil)
		}
	}
}

// topoOrder returns a Kahn topological sort of services, ties broken by
// registration order for deterministic boots.
func (m *Manager) topoOrder() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	indegree := map[string]int{}
	dependents := map[string][]string{}
	for _, name := range m.order {
		if _, seen := indegree[name]; !seen {
			indegree[name] = 0
		}
		for _, dep := range m.services[name].Depends() {
			if _, ok := m.services[dep]; !ok {
				return nil, fmt.Errorf("service %s depends on unknown service %q", name, dep)
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	// Registration order — not name order — defines the ready queue's
	// tie-break, so services start in the order the runtime registered
	// them, dependencies aside.
	var ready []string
	for _, name := range m.order {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	index := make(map[string]int, len(m.order))
	for i, name := range m.order {
		index[name] = i
	}
	byRegistration := func(i, j int) bool { return index[ready[i]] < index[ready[j]] }

	var order []string
	for len(ready) > 0 {
		name := ready[0]
		ready = ready[1:]
		order = append(order, name)
		for _, dep := range dependents[name] {
			indegree[dep]--
			if indegree[dep] == 0 {
				ready = append(ready, dep)
				sort.Slice(ready, byRegistration)
			}
		}
	}
	if len(order) != len(m.order) {
		var stuck []string
		for _, name := range m.order {
			if indegree[name] > 0 {
				stuck = append(stuck, name)
			}
		}
		return nil, fmt.Errorf("service dependency cycle involving %v", stuck)
	}
	return order, nil
}
