package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

type testNotifSink struct{ mgr *notifications.Manager }

func (s *testNotifSink) Record(r service.NotificationRecord) bool {
	return s.mgr.Record(notifications.Notification{
		Title:     r.Title,
		Message:   r.Message,
		Level:     notifications.Level(r.Level),
		Source:    r.Source,
		PID:       r.PID,
		Timestamp: r.Timestamp,
	})
}

func setupNotify(t *testing.T) (*ipc.Router, *notifications.Manager, *NotifyService) {
	t.Helper()
	r := ipc.NewRouter()
	mgr := notifications.NewManager(100, 3, 8)
	ctx := &service.Context{
		Router:        r,
		Notifications: &testNotifSink{mgr},
	}
	svc := NewNotify()
	if err := svc.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	return r, mgr, svc
}

func dispatch(r *ipc.Router, caps []string, principal security.Principal, params any) ipc.Response {
	b, _ := json.Marshal(params)
	ctx := ipc.WithCapabilities(context.Background(), security.NewCapabilities(caps...))
	ctx = ipc.WithPrincipal(ctx, principal)
	return r.Dispatch(ctx, ipc.Request{ID: 1, Method: "notify/post", Params: b})
}

func TestNotifyPost_Success(t *testing.T) {
	r, mgr, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.timer", 7, "", security.User{Name: "guest"})
	resp := dispatch(r, []string{security.CapNotify}, p, map[string]string{
		"severity": "info",
		"title":    "Pomodoro done",
		"body":     "Take a five-minute break.",
	})
	if !resp.OK {
		t.Fatalf("post rejected: %s", resp.Error)
	}

	history := mgr.History()
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0].Title != "Pomodoro done" {
		t.Fatalf("title mismatch: %q", history[0].Title)
	}
	if history[0].Source != "com.example.timer" {
		t.Fatalf("expected app attribution %q, got %q", "com.example.timer", history[0].Source)
	}
	if history[0].PID != 7 {
		t.Fatalf("expected PID 7, got %d", history[0].PID)
	}
	if len(mgr.ActiveToasts()) != 1 {
		t.Fatalf("expected 1 active toast, got %d", len(mgr.ActiveToasts()))
	}
}

func TestNotifyPost_MissingCapability(t *testing.T) {
	r, _, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.timer", 7, "", security.User{Name: "guest"})
	resp := dispatch(r, []string{security.CapIPC}, p, map[string]string{
		"severity": "info",
		"title":    "Pomodoro done",
		"body":     "Take a break.",
	})
	if resp.OK {
		t.Fatal("expected permission denied for missing notify capability")
	}
	if !strings.Contains(resp.Error, "missing capability") {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

func TestNotifyPost_OnlyApplications(t *testing.T) {
	r, _, _ := setupNotify(t)
	p := security.OperatorPrincipal(security.User{Name: "operator"})
	resp := dispatch(r, []string{security.CapNotify}, p, map[string]string{
		"severity": "info",
		"title":    "Ops",
		"body":     "This is not allowed.",
	})
	if resp.OK {
		t.Fatal("expected rejection for operator caller")
	}
	if !strings.Contains(resp.Error, "app principal required") {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}

func TestNotifyPost_MalformedAndOversized(t *testing.T) {
	r, _, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.timer", 7, "", security.User{Name: "guest"})

	tests := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"empty title", map[string]string{"severity": "info", "title": "", "body": "body"}, "title is required"},
		{"empty body", map[string]string{"severity": "info", "title": "title", "body": "  "}, "body is required"},
		{"bad severity", map[string]string{"severity": "critical", "title": "title", "body": "body"}, "severity"},
		{"long title", map[string]string{"severity": "info", "title": strings.Repeat("x", maxNotifyTitle+1), "body": "body"}, "title exceeds"},
		{"long body", map[string]string{"severity": "info", "title": "title", "body": strings.Repeat("x", maxNotifyBody+1)}, "body exceeds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := dispatch(r, []string{security.CapNotify}, p, tc.params)
			if resp.OK {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(resp.Error, tc.want) {
				t.Fatalf("expected error containing %q, got %q", tc.want, resp.Error)
			}
		})
	}
}

func TestNotifyPost_DND(t *testing.T) {
	r, mgr, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.timer", 7, "", security.User{Name: "guest"})

	mgr.SetDND(true)
	resp := dispatch(r, []string{security.CapNotify}, p, map[string]string{
		"severity": "warning",
		"title":    "DND test",
		"body":     "This should be recorded but not toasted.",
	})
	if !resp.OK {
		t.Fatalf("post rejected: %s", resp.Error)
	}
	if len(mgr.History()) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(mgr.History()))
	}
	if len(mgr.ActiveToasts()) != 0 {
		t.Fatalf("expected 0 toasts under DND, got %d", len(mgr.ActiveToasts()))
	}

	mgr.SetDND(false)
	resp = dispatch(r, []string{security.CapNotify}, p, map[string]string{
		"severity": "info",
		"title":    "Now toasted",
		"body":     "DND is off.",
	})
	if !resp.OK {
		t.Fatalf("post rejected: %s", resp.Error)
	}
	if len(mgr.ActiveToasts()) != 1 {
		t.Fatalf("expected 1 toast after DND off, got %d", len(mgr.ActiveToasts()))
	}
}

func TestNotifyPost_FloodBounds(t *testing.T) {
	r, mgr, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.flood", 9, "", security.User{Name: "guest"})

	for i := range 10 {
		resp := dispatch(r, []string{security.CapNotify}, p, map[string]string{
			"severity": "info",
			"title":    fmt.Sprintf("Alert %d", i),
			"body":     "Flood body.",
		})
		if !resp.OK {
			t.Fatalf("post %d rejected: %s", i, resp.Error)
		}
	}

	if len(mgr.ActiveToasts()) != 3 {
		t.Fatalf("expected 3 active toasts, got %d", len(mgr.ActiveToasts()))
	}
	if mgr.DroppedToasts() == 0 {
		t.Fatal("expected some dropped toasts from flood bounding")
	}
	if len(mgr.History()) != 10 {
		t.Fatalf("expected 10 history entries, got %d", len(mgr.History()))
	}
}

func TestNotifyPost_RevokedCapability(t *testing.T) {
	r, _, _ := setupNotify(t)
	p := security.AppPrincipal("com.example.timer", 7, "", security.User{Name: "guest"})

	// First post succeeds.
	resp := dispatch(r, []string{security.CapNotify}, p, map[string]string{
		"severity": "info",
		"title":    "Still running",
		"body":     "App is alive.",
	})
	if !resp.OK {
		t.Fatalf("first post rejected: %s", resp.Error)
	}

	// After the app stops, its grant is revoked; subsequent posts are denied.
	resp = dispatch(r, []string{}, p, map[string]string{
		"severity": "info",
		"title":    "After stop",
		"body":     "Should fail.",
	})
	if resp.OK {
		t.Fatal("expected rejection after capability revocation")
	}
	if !strings.Contains(resp.Error, "missing capability") {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
}
