package shell

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

type mockSessionCaller struct {
	mu           sync.Mutex
	calls        []string
	sessionID    string
	attachmentID string
	history      []string
	cwd          string
	activeView   string
	detachCalled bool
}

func (m *mockSessionCaller) Call(ctx context.Context, method string, params any, result any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, method)
	switch method {
	case "session/attach":
		if result != nil {
			resp := map[string]any{
				"session_id":    m.sessionID,
				"attachment_id": m.attachmentID,
				"active_count":  1,
				"workspace": map[string]any{
					"cwd":         m.cwd,
					"active_view": m.activeView,
					"history":     m.history,
				},
			}
			b, _ := json.Marshal(resp)
			_ = json.Unmarshal(b, result)
		}
		return nil
	case "session/detach":
		m.detachCalled = true
		return nil
	case "session/workspace/get":
		if result != nil {
			resp := map[string]any{
				"cwd":         m.cwd,
				"active_view": m.activeView,
				"history":     m.history,
			}
			b, _ := json.Marshal(resp)
			_ = json.Unmarshal(b, result)
		}
		return nil
	case "session/workspace/clear":
		m.history = nil
		if result != nil {
			resp := map[string]any{
				"history": []string{},
			}
			b, _ := json.Marshal(resp)
			_ = json.Unmarshal(b, result)
		}
		return nil
	case "session/workspace/set":
		if pMap, ok := params.(map[string]any); ok {
			if cwd, ok := pMap["cwd"].(string); ok && cwd != "" {
				m.cwd = cwd
			}
			if cmd, ok := pMap["cmd"].(string); ok && cmd != "" {
				m.history = append(m.history, cmd)
			}
		}
		return nil
	case "session/get":
		if result != nil {
			resp := map[string]any{
				"id": m.sessionID,
				"user": map[string]any{
					"id":   "guest",
					"name": "Guest User",
				},
				"started_at": time.Now().Add(-10 * time.Minute),
				"active":     true,
				"attachments": []map[string]any{
					{
						"id":          m.attachmentID,
						"client_id":   "client-1",
						"client_type": "shell",
						"attached_at": time.Now().Add(-5 * time.Minute),
					},
				},
			}
			b, _ := json.Marshal(resp)
			_ = json.Unmarshal(b, result)
		}
		return nil
	case "app/list", "sys/status", "fs/list":
		return nil
	default:
		return nil
	}
}

func (m *mockSessionCaller) isDetachCalled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.detachCalled
}

func TestShellAttachedModeAndDetach(t *testing.T) {
	mock := &mockSessionCaller{
		sessionID:    "sess-test-123",
		attachmentID: "att-test-456",
		cwd:          "/users/guest/documents",
		history:      []string{"dir", "recents"},
	}

	model := New(context.Background(), mock, nil)
	model.SetAttached(true)

	if !model.Attached() {
		t.Fatal("expected model.Attached() to be true")
	}

	// Trigger resultMsg simulating session attach response
	model.Update(resultMsg{
		sessionID:        mock.sessionID,
		attachmentID:     mock.attachmentID,
		activeCount:      1,
		cwd:              mock.cwd,
		workspaceHistory: mock.history,
	})

	if model.cwd != "/users/guest/documents" {
		t.Fatalf("expected cwd /users/guest/documents, got %s", model.cwd)
	}
	if len(model.history) != 2 || model.history[0] != "dir" {
		t.Fatalf("expected restored history [dir, recents], got %v", model.history)
	}

	// Verify ATTACHED badge in View()
	view := model.View().Content
	if !strings.Contains(view, "ATTACHED") {
		t.Fatalf("expected 'ATTACHED' in view, got: %s", view)
	}

	model.setMode(modePrompt)
	view = model.View().Content
	if !strings.Contains(view, "DETACH") {
		t.Fatalf("expected 'DETACH' in help bindings, got: %s", view)
	}

	// Run detach command
	res := execute(context.Background(), mock, model.cwd, "detach")
	if !res.detach {
		t.Fatalf("expected res.detach to be true")
	}
	if !res.quit {
		t.Fatalf("expected res.quit to be true")
	}

	// Process resultMsg with detach = true in attached mode
	_, cmd := model.Update(res)
	if cmd == nil || cmd() != tea.Quit() {
		t.Fatal("expected tea.Quit command on detach")
	}
	if !mock.isDetachCalled() {
		t.Fatal("expected session/detach to be called")
	}
}

func TestShellOwnedModeDetachRejection(t *testing.T) {
	mock := &mockSessionCaller{}
	model := New(context.Background(), mock, nil)
	model.SetAttached(false)

	// In owned mode, detach command is rejected with helpful guidance
	res := execute(context.Background(), mock, model.cwd, "detach")
	_, cmd := model.Update(res)
	if cmd != nil {
		t.Fatal("expected detach to NOT quit in owned mode")
	}

	// Check transcript for error
	found := false
	for _, tr := range model.transcript {
		if strings.Contains(tr.text, "cannot detach from owned session") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected cannot detach explanation in transcript")
	}
}

func TestShellHistoryAndSessionCommands(t *testing.T) {
	mock := &mockSessionCaller{
		sessionID:    "sess-999",
		attachmentID: "att-888",
		cwd:          "/users/guest",
		history:      []string{"dir", "apps", "echo hi"},
	}

	// Test 'history' command
	res := execute(context.Background(), mock, "/users/guest", "history")
	if res.err != nil {
		t.Fatalf("history command failed: %v", res.err)
	}
	if !strings.Contains(res.text, "dir") || !strings.Contains(res.text, "apps") {
		t.Fatalf("expected history entries in output, got: %s", res.text)
	}

	// Test 'session' command
	res = execute(context.Background(), mock, "/users/guest", "session")
	if res.err != nil {
		t.Fatalf("session command failed: %v", res.err)
	}
	if !strings.Contains(res.text, "sess-999") || !strings.Contains(res.text, "Guest User") {
		t.Fatalf("expected session detail in output, got: %s", res.text)
	}

	// Test 'history clear' command
	res = execute(context.Background(), mock, "/users/guest", "history clear")
	if res.err != nil {
		t.Fatalf("history clear failed: %v", res.err)
	}
	if !res.clearHistory {
		t.Fatal("expected clearHistory flag")
	}
	model := New(context.Background(), mock, nil)
	model.history = []string{"dir", "apps"}
	model.Update(res)
	if len(model.history) != 0 {
		t.Fatalf("expected empty model history after clear, got %v", model.history)
	}
}
