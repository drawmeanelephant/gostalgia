package receipts

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func TestCrashReceiptCreationAndSanitization(t *testing.T) {
	rawLogs := "starting app\x1b[31m\npanic: bad memory access\x1b[0m\nstack trace\n"
	name := "bad-app\x1b[2J"
	reason := "exit code 1: \aerror occurred"

	r := New(42, name, "failed", 1, reason, 2, rawLogs)

	if r.PID != 42 {
		t.Fatalf("expected PID 42, got %d", r.PID)
	}
	if strings.Contains(r.Name, "\x1b") {
		t.Fatal("name contains ANSI escape sequence")
	}
	if strings.Contains(r.Reason, "\a") {
		t.Fatal("reason contains control character")
	}
	if strings.Contains(r.LogExcerpt, "\x1b") {
		t.Fatal("log excerpt contains ANSI escape sequence")
	}
	if r.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", r.ExitCode)
	}
	if r.RestartCount != 2 {
		t.Fatalf("expected restarts 2, got %d", r.RestartCount)
	}
	if !strings.HasPrefix(r.ID, "CR-42-") {
		t.Fatalf("unexpected ID format: %s", r.ID)
	}
}

func TestCrashReceiptLogExcerptBounding(t *testing.T) {
	// Generate 100 lines of logs
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, fmt.Sprintf("line %03d: log message", i))
	}
	raw := strings.Join(lines, "\n")
	r := New(10, "flooder", "crashloop", 137, "crash loop", 5, raw)

	if !r.Truncated {
		t.Fatal("expected truncated flag to be true")
	}
	excerptLines := strings.Split(r.LogExcerpt, "\n")
	if len(excerptLines) > MaxExcerptLines {
		t.Fatalf("excerpt has %d lines, want <= %d", len(excerptLines), MaxExcerptLines)
	}
	// Check that the latest lines are preserved
	if !strings.Contains(r.LogExcerpt, "line 099") {
		t.Fatal("expected latest line 099 to be present in excerpt")
	}
}

func TestStoreBoundingAndLookup(t *testing.T) {
	store := NewStore(3)

	r1 := New(1, "app-1", "failed", 1, "err 1", 0, "log 1")
	r2 := New(2, "app-2", "failed", 1, "err 2", 1, "log 2")
	r3 := New(1, "app-1", "crashloop", 1, "crashloop", 3, "log 3")
	r4 := New(4, "app-4", "failed", 2, "err 4", 0, "log 4")

	store.Add(r1)
	store.Add(r2)
	store.Add(r3)
	if store.Count() != 3 {
		t.Fatalf("count = %d, want 3", store.Count())
	}

	store.Add(r4)
	if store.Count() != 3 {
		t.Fatalf("count after eviction = %d, want 3", store.Count())
	}

	// r1 should have been evicted
	if _, ok := store.Get(r1.ID); ok {
		t.Fatal("r1 should have been evicted")
	}
	// r4, r3, r2 should be present
	list := store.List()
	if len(list) != 3 {
		t.Fatalf("list len = %d, want 3", len(list))
	}
	if list[0].ID != r4.ID || list[1].ID != r3.ID || list[2].ID != r2.ID {
		t.Fatalf("unexpected order in list: %+v", list)
	}

	// GetByPID should return most recent receipt for PID 1 (which is r3)
	latestPID1, ok := store.GetByPID(1)
	if !ok || latestPID1.ID != r3.ID {
		t.Fatalf("expected r3 for PID 1, got %+v", latestPID1)
	}
}

func TestRenderReceipt(t *testing.T) {
	r := New(7, "demo-service", "failed", 1, "panic in main", 1, "line 1\nline 2\npanic: nil pointer dereference\n")
	kit := ui.New(theme.Nostalgia(), ui.ANSI256)

	bounds := ui.Bounds{Width: 80, Height: 24}
	view := Render(kit, r, bounds)

	if view == "" {
		t.Fatal("render returned empty string")
	}
	if lipgloss.Width(view) > bounds.Width || lipgloss.Height(view) > bounds.Height {
		t.Fatalf("rendered bounds %dx%d exceed %dx%d", lipgloss.Width(view), lipgloss.Height(view), bounds.Width, bounds.Height)
	}
	if !strings.Contains(view, "CRASH RECEIPT") {
		t.Fatal("missing title in rendered receipt")
	}
	if !strings.Contains(view, "demo-service") {
		t.Fatal("missing process name in rendered receipt")
	}
	if !strings.Contains(view, "Exit code: 1") {
		t.Fatal("missing exit code in rendered receipt")
	}
	if !strings.Contains(view, "panic: nil pointer dereference") {
		t.Fatal("missing log excerpt in rendered receipt")
	}
}
