// Package session implements environment sessions: a logged-in user's
// span of activity, to which applications and processes are attached.
package session

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

// Attachment represents an active client (shell, gctl, etc.) attached to a session.
type Attachment struct {
	ID         string    `json:"id"`
	ClientID   string    `json:"client_id"`
	ClientType string    `json:"client_type"` // e.g. "shell", "gctl", "operator"
	AttachedAt time.Time `json:"attached_at"`
}

// Bounds on session manager state, matching the bounded-everything
// convention used for process history and event history.
const (
	// MaxSessions bounds concurrently tracked sessions; Create errors past it.
	MaxSessions = 64
	// MaxAttachmentsPerSession bounds client attachments on one session.
	MaxAttachmentsPerSession = 32
)

// Session is one user's login session.
type Session struct {
	ID        string        `json:"id"`
	User      security.User `json:"user"`
	StartedAt time.Time     `json:"started_at"`
	StoppedAt time.Time     `json:"stopped_at,omitempty"`

	mu          sync.Mutex
	stopped     bool
	nextAttach  int
	attachments map[string]Attachment
}

// Active reports whether the session is still open.
func (s *Session) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stopped
}

func (s *Session) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		s.stopped = true
		s.StoppedAt = time.Now()
		s.attachments = nil
	}
}

// Attach registers a new client attachment to this session. It returns a
// zero Attachment if the session is stopped or the attachment cap is reached.
func (s *Session) Attach(clientType, clientID string) Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.attachments) >= MaxAttachmentsPerSession {
		return Attachment{}
	}
	if s.attachments == nil {
		s.attachments = make(map[string]Attachment)
	}
	s.nextAttach++
	if clientID == "" {
		clientID = fmt.Sprintf("client-%d", s.nextAttach)
	}
	if clientType == "" {
		clientType = "shell"
	}
	att := Attachment{
		ID:         fmt.Sprintf("att-%d", s.nextAttach),
		ClientID:   clientID,
		ClientType: clientType,
		AttachedAt: time.Now(),
	}
	s.attachments[att.ID] = att
	return att
}

// Detach removes an attachment by ID. Returns true if removed.
func (s *Session) Detach(attachmentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.attachments == nil {
		return false
	}
	if _, ok := s.attachments[attachmentID]; ok {
		delete(s.attachments, attachmentID)
		return true
	}
	return false
}

// Attachments returns a list of active attachments, sorted by ID.
func (s *Session) Attachments() []Attachment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Attachment, 0, len(s.attachments))
	for _, a := range s.attachments {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AttachmentCount returns the number of active attachments.
func (s *Session) AttachmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attachments)
}

// Event is published when a session opens or closes.
type Event struct {
	ID    string `json:"id"`
	User  string `json:"user"`
	State string `json:"state"` // "started" or "stopped"
}

func (Event) Type() string { return "session.state" }

// AttachEvent is published when a client attaches to or detaches from a session.
type AttachEvent struct {
	SessionID    string `json:"session_id"`
	AttachmentID string `json:"attachment_id"`
	ClientType   string `json:"client_type"`
	State        string `json:"state"` // "attached" or "detached"
	ActiveCount  int    `json:"active_count"`
}

func (AttachEvent) Type() string { return "session.attachment" }

// Manager creates and tracks sessions and their workspace stores.
// Workspace stores are keyed by user name, not session: workspace state
// persists per user at /users/<name>/config/workspace.json, so every
// session of one user must share a single store — independent stores over
// the same file would silently lose each other's updates.
type Manager struct {
	mu         sync.Mutex
	next       int
	sessions   map[string]*Session
	workspaces map[string]*WorkspaceStore // keyed by workspaceKey (user name)
	fsys       vfs.FS
	bus        *events.Bus
	log        *slog.Logger
}

func NewManager(bus *events.Bus, log *slog.Logger) *Manager {
	return &Manager{
		sessions:   map[string]*Session{},
		workspaces: map[string]*WorkspaceStore{},
		bus:        bus,
		log:        log,
	}
}

// SetVFS configures the virtual filesystem for workspace persistence.
func (m *Manager) SetVFS(fsys vfs.FS) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fsys = fsys
	for _, ws := range m.workspaces {
		ws.setFS(fsys)
	}
}

// Create opens a session for user.
func (m *Manager) Create(user security.User) (*Session, error) {
	if user.ID == "" {
		return nil, fmt.Errorf("session: user id is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= MaxSessions {
		return nil, fmt.Errorf("session: session limit reached (%d)", MaxSessions)
	}
	m.next++
	s := &Session{
		ID:          fmt.Sprintf("session-%d", m.next),
		User:        user,
		StartedAt:   time.Now(),
		attachments: make(map[string]Attachment),
	}
	m.sessions[s.ID] = s
	m.bus.Publish("session", Event{ID: s.ID, User: user.Name, State: "started"})
	m.log.Info("session started", "session", s.ID, "user", user.Name)
	return s, nil
}

// Close closes a session by ID and reaps it: the session is removed so its
// id cannot resolve or be mutated after close. The shared per-user
// workspace store is dropped only when the closing session is the user's
// last live session.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
		key := workspaceKey(s)
		lastForUser := true
		for _, other := range m.sessions {
			if workspaceKey(other) == key {
				lastForUser = false
				break
			}
		}
		if lastForUser {
			delete(m.workspaces, key)
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("session: no such session %q", id)
	}
	s.close()
	m.bus.Publish("session", Event{ID: s.ID, User: s.User.Name, State: "stopped"})
	m.log.Info("session stopped", "session", s.ID, "user", s.User.Name)
	return nil
}

// Attach registers a client attachment to a session.
func (m *Manager) Attach(sessionID, clientType, clientID string) (Attachment, error) {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return Attachment{}, fmt.Errorf("session: no such session %q", sessionID)
	}
	if !s.Active() {
		return Attachment{}, fmt.Errorf("session: session %q is not active", sessionID)
	}

	att := s.Attach(clientType, clientID)
	if att.ID == "" {
		return Attachment{}, fmt.Errorf("session: session %q is not active or attachment limit reached", sessionID)
	}
	count := s.AttachmentCount()
	m.bus.Publish("session", AttachEvent{
		SessionID:    s.ID,
		AttachmentID: att.ID,
		ClientType:   att.ClientType,
		State:        "attached",
		ActiveCount:  count,
	})
	m.log.Info("client attached to session", "session", s.ID, "attachment", att.ID, "type", att.ClientType, "count", count)
	return att, nil
}

// Detach unregisters a client attachment from a session.
func (m *Manager) Detach(sessionID, attachmentID string) error {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("session: no such session %q", sessionID)
	}

	if !s.Detach(attachmentID) {
		return fmt.Errorf("session: no such attachment %q in session %q", attachmentID, sessionID)
	}

	count := s.AttachmentCount()
	m.bus.Publish("session", AttachEvent{
		SessionID:    s.ID,
		AttachmentID: attachmentID,
		State:        "detached",
		ActiveCount:  count,
	})
	m.log.Info("client detached from session", "session", s.ID, "attachment", attachmentID, "count", count)
	return nil
}

// workspaceKey identifies the per-user workspace store a session maps to.
func workspaceKey(s *Session) string {
	if s.User.Name != "" {
		return s.User.Name
	}
	return "guest"
}

// Workspace returns the workspace store shared by all live sessions of the
// session's user, lazily initializing it on first use. It returns nil for
// unknown or reaped session ids.
func (m *Manager) Workspace(sessionID string) *WorkspaceStore {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return nil
	}
	key := workspaceKey(s)
	if ws, ok := m.workspaces[key]; ok {
		return ws
	}
	wsPath := fmt.Sprintf("/users/%s/config/workspace.json", key)
	ws := NewWorkspaceStore(m.fsys, wsPath)
	ws.state.SessionID = sessionID
	// Attempt initial load from VFS if available
	_, _ = ws.Load()
	m.workspaces[key] = ws
	return ws
}

// Get returns a session by ID.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// Active lists open sessions, sorted by ID.
func (m *Manager) Active() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s.Active() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForUser lists open sessions for a specific user name, sorted by ID.
func (m *Manager) ForUser(userName string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0)
	for _, s := range m.sessions {
		if s.Active() && s.User.Name == userName {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
