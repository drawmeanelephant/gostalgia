package notifications

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func TestNotificationsSeenDistinguishedFromRecorded(t *testing.T) {
	mgr := NewManager(10, 3, 5)

	n := Notification{
		Title:   "App Failed",
		Message: "Echo exited with code 1",
		Level:   LevelError,
		Source:  "process",
		PID:     10,
	}

	mgr.Record(n)

	if mgr.UnseenCount() != 1 {
		t.Fatalf("expected 1 unseen, got %d", mgr.UnseenCount())
	}

	hist := mgr.History()
	if len(hist) != 1 {
		t.Fatalf("expected 1 history item, got %d", len(hist))
	}
	if hist[0].Seen {
		t.Fatal("recorded event should have Seen=false initially")
	}

	// User views or acknowledges the notification
	mgr.MarkSeen(hist[0].ID)
	if mgr.UnseenCount() != 0 {
		t.Fatalf("expected 0 unseen after MarkSeen, got %d", mgr.UnseenCount())
	}
	if !mgr.History()[0].Seen {
		t.Fatal("expected history item to have Seen=true")
	}
}

func TestDoNotDisturbSuppressionPreservesHistory(t *testing.T) {
	mgr := NewManager(10, 3, 5)
	mgr.SetDND(true)

	if !mgr.DND() {
		t.Fatal("expected DND to be true")
	}

	// Record several events while DND is active
	for i := 1; i <= 5; i++ {
		mgr.Record(Notification{
			Title:   fmt.Sprintf("Event %d", i),
			Message: fmt.Sprintf("Detail %d", i),
			Level:   LevelWarning,
		})
	}

	// Active toasts must be suppressed completely
	if len(mgr.ActiveToasts()) != 0 {
		t.Fatalf("expected 0 active toasts under DND, got %d", len(mgr.ActiveToasts()))
	}
	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	if toastView := mgr.RenderToasts(kit, 80); toastView != "" {
		t.Fatalf("expected empty toast rendering under DND, got %q", toastView)
	}

	// History MUST NOT be destroyed! All 5 events must be recorded
	hist := mgr.History()
	if len(hist) != 5 {
		t.Fatalf("expected 5 history items preserved, got %d", len(hist))
	}
	if hist[0].Title != "Event 5" || hist[4].Title != "Event 1" {
		t.Fatalf("unexpected history order: %+v", hist)
	}

	// Disabling DND allows new events to generate toasts
	mgr.ToggleDND()
	if mgr.DND() {
		t.Fatal("expected DND to be false after toggle")
	}

	mgr.Record(Notification{
		Title:   "Event 6",
		Message: "Detail 6",
		Level:   LevelInfo,
	})
	if len(mgr.ActiveToasts()) != 1 {
		t.Fatalf("expected 1 active toast after disabling DND, got %d", len(mgr.ActiveToasts()))
	}
}

func TestToastFloodBounding(t *testing.T) {
	maxToasts := 3
	mgr := NewManager(100, maxToasts, 10)

	// Rapidly fire 50 notifications
	for i := 0; i < 50; i++ {
		mgr.Record(Notification{
			Title:   fmt.Sprintf("Crash %d", i),
			Message: fmt.Sprintf("Process %d crashed", i),
			Level:   LevelError,
		})
	}

	// Active toasts must not exceed maxToasts
	toasts := mgr.ActiveToasts()
	if len(toasts) > maxToasts {
		t.Fatalf("active toasts %d exceeds limit %d", len(toasts), maxToasts)
	}

	// Dropped toasts must be accurately tracked
	expectedDrops := uint64(50 - maxToasts)
	if mgr.DroppedToasts() != expectedDrops {
		t.Fatalf("dropped toasts = %d, want %d", mgr.DroppedToasts(), expectedDrops)
	}

	// Render toasts shows the flood suppression notice
	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	rendered := mgr.RenderToasts(kit, 80)
	if !strings.Contains(rendered, "alerts suppressed by flood bounding") {
		t.Fatalf("expected flood notice in toasts rendering:\n%s", rendered)
	}
}

func TestToastTransientCountdownAndExpiration(t *testing.T) {
	mgr := NewManager(10, 3, 2) // 2 ticks lifetime

	mgr.Record(Notification{
		Title:   "Starting",
		Message: "Process starting",
		Level:   LevelInfo,
	})

	if len(mgr.ActiveToasts()) != 1 {
		t.Fatalf("expected 1 toast, got %d", len(mgr.ActiveToasts()))
	}

	// Tick 1
	mgr.Tick()
	if len(mgr.ActiveToasts()) != 1 {
		t.Fatalf("expected toast still alive after 1 tick, got %d", len(mgr.ActiveToasts()))
	}

	// Tick 2 (expires)
	mgr.Tick()
	if len(mgr.ActiveToasts()) != 0 {
		t.Fatalf("expected toast to expire after 2 ticks, got %d", len(mgr.ActiveToasts()))
	}
}

func TestDismissAndClear(t *testing.T) {
	mgr := NewManager(10, 3, 5)

	mgr.Record(Notification{ID: "n1", Title: "One", Level: LevelInfo})
	mgr.Record(Notification{ID: "n2", Title: "Two", Level: LevelInfo})
	mgr.Record(Notification{ID: "n3", Title: "Three", Level: LevelInfo})

	if len(mgr.History()) != 3 {
		t.Fatalf("expected 3, got %d", len(mgr.History()))
	}

	mgr.Dismiss("n2")
	if len(mgr.History()) != 2 {
		t.Fatalf("expected 2 after dismissing n2, got %d", len(mgr.History()))
	}
	for _, n := range mgr.History() {
		if n.ID == "n2" {
			t.Fatal("n2 still present after dismiss")
		}
	}

	mgr.Clear()
	if len(mgr.History()) != 0 {
		t.Fatalf("expected 0 after Clear, got %d", len(mgr.History()))
	}
}

func TestSanitizationInNotifications(t *testing.T) {
	mgr := NewManager(10, 3, 5)

	n := Notification{
		Title:   "Crash \x1b[31malert\x1b[0m",
		Message: "Evil message\a with escape\x1b[2J",
		Source:  "service\tname",
		Level:   LevelError,
	}
	mgr.Record(n)

	saved := mgr.History()[0]
	if strings.Contains(saved.Title, "\x1b") {
		t.Fatal("title contains ANSI escape")
	}
	if strings.Contains(saved.Message, "\a") || strings.Contains(saved.Message, "\x1b") {
		t.Fatal("message contains control char or ANSI escape")
	}
}

func TestRenderPanel(t *testing.T) {
	mgr := NewManager(10, 3, 5)
	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	bounds := ui.Bounds{Width: 80, Height: 24}

	// Empty panel
	emptyView := mgr.RenderPanel(kit, bounds)
	if !strings.Contains(emptyView, "Notification Center Clean") {
		t.Fatal("empty panel missing clean notice")
	}

	// Populated panel with receipt
	receipt := receipts.New(12, "worker", "failed", 1, "bad state", 0, "logs")
	mgr.Record(Notification{
		Title:     "Worker Crashed",
		Message:   "Worker exited unexpectedly",
		Level:     LevelError,
		PID:       12,
		Receipt:   &receipt,
		Timestamp: time.Now(),
	})

	popView := mgr.RenderPanel(kit, bounds)
	if !strings.Contains(popView, "Worker Crashed") {
		t.Fatal("populated panel missing title")
	}
	if !strings.Contains(popView, "RECEIPT AVAILABLE") {
		t.Fatal("populated panel missing receipt tag")
	}
	if lipgloss.Width(popView) > bounds.Width || lipgloss.Height(popView) > bounds.Height {
		t.Fatalf("panel %dx%d exceeds %dx%d", lipgloss.Width(popView), lipgloss.Height(popView), bounds.Width, bounds.Height)
	}
}
