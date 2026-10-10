package session

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

type recorder struct {
	mu   sync.Mutex
	seen []Event
}

func (r *recorder) add(e events.Envelope) {
	if ev, ok := e.Payload.(Event); ok {
		r.mu.Lock()
		r.seen = append(r.seen, ev)
		r.mu.Unlock()
	}
}

func (r *recorder) count(state string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.seen {
		if ev.State == state {
			n++
		}
	}
	return n
}

func newTestManager(t *testing.T) (*Manager, *recorder) {
	t.Helper()
	bus := events.NewBus()
	rec := &recorder{}
	bus.Subscribe("*", func(e events.Envelope) { rec.add(e) })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(bus, log), rec
}

func TestCreateAndCloseSession(t *testing.T) {
	m, rec := newTestManager(t)
	s, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Active() {
		t.Fatal("fresh session is not active")
	}
	if s.ID == "" {
		t.Fatal("session id is empty")
	}
	if err := m.Close(s.ID); err != nil {
		t.Fatal(err)
	}
	if s.Active() {
		t.Fatal("closed session is still active")
	}
	if rec.count("started") != 1 || rec.count("stopped") != 1 {
		t.Fatalf("events: started=%d stopped=%d, want 1 and 1", rec.count("started"), rec.count("stopped"))
	}
	if len(m.Active()) != 0 {
		t.Fatal("closed session still listed as active")
	}
}

// #90: Close must reap the session and its workspace — ids must not
// resolve afterwards, and the maps must not grow forever.
func TestCloseReapsSessionState(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	if ws := m.Workspace(s.ID); ws == nil {
		t.Fatal("no workspace for a live session")
	}
	if err := m.Close(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(s.ID); ok {
		t.Fatal("closed session still resolves via Get")
	}
	if ws := m.Workspace(s.ID); ws != nil {
		t.Fatal("workspace still resolves for a closed session")
	}
	if err := m.Close(s.ID); err == nil {
		t.Fatal("closing an already-reaped session succeeded, want error")
	}
}

// #90: unknown session ids must not materialize workspaces.
func TestWorkspaceUnknownSessionReturnsNil(t *testing.T) {
	m, _ := newTestManager(t)
	if ws := m.Workspace("session-ghost"); ws != nil {
		t.Fatal("workspace created for a nonexistent session")
	}
}

// #90: session count is bounded like every other runtime resource.
func TestSessionCountBound(t *testing.T) {
	m, _ := newTestManager(t)
	for i := 0; i < MaxSessions; i++ {
		if _, err := m.Create(security.User{ID: fmt.Sprintf("u-%d", i), Name: "u"}); err != nil {
			t.Fatalf("create %d failed: %v", i, err)
		}
	}
	if _, err := m.Create(security.User{ID: "u-overflow"}); err == nil {
		t.Fatal("create beyond MaxSessions succeeded, want error")
	}
}

// #90: attachments per session are bounded.
func TestAttachmentCountBound(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxAttachmentsPerSession; i++ {
		if _, err := m.Attach(s.ID, "shell", ""); err != nil {
			t.Fatalf("attach %d failed: %v", i, err)
		}
	}
	if _, err := m.Attach(s.ID, "shell", ""); err == nil {
		t.Fatal("attach beyond MaxAttachmentsPerSession succeeded, want error")
	}
	if got := s.AttachmentCount(); got != MaxAttachmentsPerSession {
		t.Fatalf("attachments = %d, want %d", got, MaxAttachmentsPerSession)
	}
}

func TestCloseUnknownSession(t *testing.T) {
	m, _ := newTestManager(t)
	if err := m.Close("nope"); err == nil {
		t.Fatal("closing unknown session succeeded, want error")
	}
}

func TestCreateRequiresUserID(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Create(security.User{}); err == nil {
		t.Fatal("creating session for empty user succeeded, want error")
	}
}

func TestSessionsGetDistinctIDs(t *testing.T) {
	m, _ := newTestManager(t)
	a, _ := m.Create(security.User{ID: "u-1", Name: "one"})
	b, _ := m.Create(security.User{ID: "u-2", Name: "two"})
	if a.ID == b.ID {
		t.Fatalf("two sessions share id %q", a.ID)
	}
	if len(m.Active()) != 2 {
		t.Fatalf("active = %d, want 2", len(m.Active()))
	}
}

func TestSessionAttachments(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}

	// Attach client 1
	att1, err := m.Attach(s.ID, "shell", "client-term-1")
	if err != nil {
		t.Fatal(err)
	}
	if att1.ID == "" {
		t.Fatal("attachment id is empty")
	}
	if s.AttachmentCount() != 1 {
		t.Fatalf("attachments count = %d, want 1", s.AttachmentCount())
	}

	// Attach client 2 (multi-client)
	att2, err := m.Attach(s.ID, "gctl", "client-ctrl-2")
	if err != nil {
		t.Fatal(err)
	}
	if s.AttachmentCount() != 2 {
		t.Fatalf("attachments count = %d, want 2", s.AttachmentCount())
	}

	atts := s.Attachments()
	if len(atts) != 2 {
		t.Fatalf("attachments len = %d, want 2", len(atts))
	}
	if atts[0].ID != att1.ID || atts[1].ID != att2.ID {
		t.Fatalf("unexpected attachment list: %+v", atts)
	}

	// Detach client 1
	if err := m.Detach(s.ID, att1.ID); err != nil {
		t.Fatal(err)
	}
	if s.AttachmentCount() != 1 {
		t.Fatalf("after detach 1, count = %d, want 1", s.AttachmentCount())
	}
	// Session must still be active
	if !s.Active() {
		t.Fatal("session unexpectedly marked inactive after single client detach")
	}

	// Detach client 2
	if err := m.Detach(s.ID, att2.ID); err != nil {
		t.Fatal(err)
	}
	if s.AttachmentCount() != 0 {
		t.Fatalf("after detach 2, count = %d, want 0", s.AttachmentCount())
	}
	// Session must still be active even with 0 attached clients (headless/background runtime)
	if !s.Active() {
		t.Fatal("session unexpectedly marked inactive when all clients detached")
	}

	// Detaching unknown attachment should fail
	if err := m.Detach(s.ID, "unknown-att"); err == nil {
		t.Fatal("detaching unknown attachment succeeded, want error")
	}
}

func TestWorkspaceStatePersistenceAndSanitization(t *testing.T) {
	memFS := vfs.NewMem()
	ws := NewWorkspaceStore(memFS, "/users/guest/config/workspace.json")

	// Verify defaults
	st := ws.Get()
	if st.CurrentDir != "/users/guest" {
		t.Fatalf("initial cwd = %q, want /users/guest", st.CurrentDir)
	}
	if st.ActiveView != "prompt" {
		t.Fatalf("initial active view = %q, want prompt", st.ActiveView)
	}

	// Modify CWD and view
	if err := ws.SetCurrentDir("/users/guest/documents"); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetActiveView("tasks"); err != nil {
		t.Fatal(err)
	}

	// Add normal commands
	_ = ws.AppendHistory("ls")
	_ = ws.AppendHistory("cd documents")
	_ = ws.AppendHistory("cat notes.txt")

	// Add sensitive commands (must be excluded or redacted)
	_ = ws.AppendHistory("auth login user pass")
	_ = ws.AppendHistory("token 0123456789abcdef0123456789abcdef")
	_ = ws.AppendHistory("connect --token=changeme")
	_ = ws.AppendHistory("service start --password=changeme")

	st = ws.Get()
	if st.CurrentDir != "/users/guest/documents" {
		t.Fatalf("updated cwd = %q, want /users/guest/documents", st.CurrentDir)
	}
	if st.ActiveView != "tasks" {
		t.Fatalf("updated active view = %q, want tasks", st.ActiveView)
	}

	// Check history: sensitive auth/token commands must NOT appear in history
	for _, h := range st.History {
		if strings.HasPrefix(h, "auth ") || strings.HasPrefix(h, "token ") {
			t.Fatalf("sensitive auth/token command leaked into history: %q", h)
		}
		if strings.Contains(h, "0123456789abcdef0123456789abcdef") {
			t.Fatalf("raw hex token leaked into history: %q", h)
		}
		if strings.Contains(h, "changeme") {
			t.Fatalf("raw password leaked into history: %q", h)
		}
	}

	// Ensure saved file on VFS exists and contains no tokens
	raw, err := memFS.ReadFile("users/guest/config/workspace.json")
	if err != nil {
		t.Fatalf("workspace.json not found on VFS: %v", err)
	}
	if strings.Contains(string(raw), "0123456789abcdef0123456789abcdef") {
		t.Fatal("persisted JSON contains raw token")
	}
	if strings.Contains(string(raw), "SuperSecretPassword123") {
		t.Fatal("persisted JSON contains raw password")
	}

	// Load into a new store instance to test recovery
	ws2 := NewWorkspaceStore(memFS, "/users/guest/config/workspace.json")
	loaded, err := ws2.Load()
	if err != nil {
		t.Fatalf("failed to load workspace state: %v", err)
	}
	if loaded.CurrentDir != "/users/guest/documents" {
		t.Fatalf("loaded cwd = %q, want /users/guest/documents", loaded.CurrentDir)
	}
	if loaded.ActiveView != "tasks" {
		t.Fatalf("loaded active view = %q, want tasks", loaded.ActiveView)
	}
	if len(loaded.History) == 0 {
		t.Fatal("loaded history is empty")
	}

	// Test history clear
	if err := ws2.ClearHistory(); err != nil {
		t.Fatal(err)
	}
	if len(ws2.Get().History) != 0 {
		t.Fatalf("history not empty after clear: %+v", ws2.Get().History)
	}
}

// #144: sessions of the same user share one workspace store — the store
// persists per user, so per-session stores would clobber each other's
// interleaved updates on disk.
func TestWorkspaceSharedAcrossUserSessions(t *testing.T) {
	m, _ := newTestManager(t)
	a, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.Create(security.User{ID: "u-other", Name: "other"})
	if err != nil {
		t.Fatal(err)
	}

	wsA := m.Workspace(a.ID)
	wsB := m.Workspace(b.ID)
	if wsA == nil || wsB == nil {
		t.Fatal("no workspace for a live session")
	}
	if wsA != wsB {
		t.Fatal("sessions of one user got independent workspace stores")
	}
	if wsOther := m.Workspace(other.ID); wsOther == wsA || wsOther == nil {
		t.Fatal("distinct users must not share a workspace store")
	}

	// Interleaved appends through the two sessions all survive.
	if err := wsA.AppendHistory("from-a"); err != nil {
		t.Fatal(err)
	}
	if err := wsB.AppendHistory("from-b"); err != nil {
		t.Fatal(err)
	}
	hist := wsA.Get().History
	for _, want := range []string{"from-a", "from-b"} {
		found := false
		for _, h := range hist {
			if h == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("history missing %q: %v", want, hist)
		}
	}

	// Closing one of the user's sessions keeps the shared store alive for
	// the other; closing the last one reaps it.
	if err := m.Close(a.ID); err != nil {
		t.Fatal(err)
	}
	if m.Workspace(b.ID) != wsB {
		t.Fatal("shared workspace reaped while a same-user session is still live")
	}
	if err := m.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	if ws := m.Workspace(b.ID); ws != nil {
		t.Fatal("workspace still resolves after the user's last session closed")
	}
}

// SetVFS swaps each store's filesystem under the store lock so it cannot
// race an in-flight save. Run under -race.
func TestSetVFSDoesNotRaceWorkspaceWrites(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create(security.User{ID: "u-guest", Name: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	ws := m.Workspace(s.ID)
	if ws == nil {
		t.Fatal("no workspace for a live session")
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = ws.AppendHistory(fmt.Sprintf("cmd-%d-%d", i, j))
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 10; j++ {
			m.SetVFS(vfs.NewMem())
		}
	}()
	wg.Wait()
}

func TestWorkspaceCorruptedFileRecovery(t *testing.T) {
	memFS := vfs.NewMem()
	_ = memFS.MkdirAll("users/guest/config")
	// Write partial/corrupted JSON
	if err := memFS.WriteFile("users/guest/config/workspace.json", []byte("{not valid json..."), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := NewWorkspaceStore(memFS, "/users/guest/config/workspace.json")
	st, err := ws.Load()
	if err == nil {
		t.Fatal("expected error on corrupted json load, got nil")
	}
	// Must still return valid fallback default state without panic
	if st == nil {
		t.Fatal("expected non-nil default state on corrupt load")
	}
	if st.CurrentDir != "/users/guest" {
		t.Fatalf("fallback cwd = %q, want /users/guest", st.CurrentDir)
	}
	if st.ActiveView != "prompt" {
		t.Fatalf("fallback active view = %q, want prompt", st.ActiveView)
	}

	// Saving new state must overwrite cleanly
	if err := ws.SetCurrentDir("/users/guest/safe"); err != nil {
		t.Fatalf("failed to save after corruption: %v", err)
	}
	wsRecovered := NewWorkspaceStore(memFS, "/users/guest/config/workspace.json")
	rec, err := wsRecovered.Load()
	if err != nil {
		t.Fatalf("failed to load recovered state: %v", err)
	}
	if rec.CurrentDir != "/users/guest/safe" {
		t.Fatalf("recovered cwd = %q, want /users/guest/safe", rec.CurrentDir)
	}
}
