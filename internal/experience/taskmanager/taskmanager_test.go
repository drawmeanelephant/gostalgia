package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/platform"
)

type mockCaller struct {
	calls map[string]int
	procs []ProcessRow
	logs  string
	reapN int
	err   error
}

func (m *mockCaller) Call(ctx context.Context, method string, params, out any) error {
	if m.calls == nil {
		m.calls = make(map[string]int)
	}
	m.calls[method]++
	if m.err != nil {
		return m.err
	}

	switch method {
	case "proc/list":
		data, _ := json.Marshal(m.procs)
		return json.Unmarshal(data, out)
	case "proc/stop":
		return nil
	case "proc/reap":
		data, _ := json.Marshal(map[string]any{"reaped": m.reapN})
		return json.Unmarshal(data, out)
	case "proc/logs":
		resp := map[string]any{
			"combined": map[string]any{"content": m.logs},
		}
		data, _ := json.Marshal(resp)
		return json.Unmarshal(data, out)
	}
	return nil
}

func TestProcessTableRendering(t *testing.T) {
	tm := New()

	procs := []ProcessRow{
		{
			ID:           1,
			Name:         "shell-init",
			State:        "running",
			RestartCount: 0,
			Resources: platform.ResourceUsage{
				Supported:   true,
				CPUUserMs:   50,
				CPUSysMs:    10,
				MemoryBytes: 4 * 1024 * 1024,
			},
		},
		{
			ID:           2,
			Name:         "flaky-worker",
			State:        "restarting",
			RestartCount: 2,
			Resources:    platform.ResourceUsage{Supported: false},
		},
		{
			ID:           3,
			Name:         "broken-app",
			State:        "crashloop",
			CrashLoop:    true,
			RestartCount: 5,
			Resources:    platform.ResourceUsage{Supported: false},
		},
		{
			ID:           4,
			Name:         "exited-job",
			State:        "stopped",
			ExitCode:     0,
			RestartCount: 0,
			Resources:    platform.ResourceUsage{Supported: false},
		},
	}
	tm.SetProcesses(procs)

	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	bounds := ui.Bounds{Width: 80, Height: 24}
	view := tm.Render(kit, bounds)

	if view == "" {
		t.Fatal("rendered view is empty")
	}
	if lipgloss.Width(view) > bounds.Width || lipgloss.Height(view) > bounds.Height {
		t.Fatalf("view %dx%d exceeds bounds %dx%d", lipgloss.Width(view), lipgloss.Height(view), bounds.Width, bounds.Height)
	}

	// Verify required columns and headers
	for _, header := range []string{"PID", "NAME", "STATE", "RESTARTS", "CPU", "MEMORY"} {
		if !strings.Contains(view, header) {
			t.Fatalf("missing table header %q", header)
		}
	}

	// Verify state formatting
	if !strings.Contains(view, "running") {
		t.Fatal("missing running state in view")
	}
	if !strings.Contains(view, "backoff") {
		t.Fatal("missing backoff state in view for restarting process")
	}
	if !strings.Contains(view, "crash loop") {
		t.Fatal("missing crash loop state in view for crashloop process")
	}
	if !strings.Contains(view, "stopped") {
		t.Fatal("missing stopped state in view")
	}

	// Verify resource formatting
	if !strings.Contains(view, "60ms") {
		t.Fatal("missing CPU 60ms in view")
	}
	if !strings.Contains(view, "4.0 MB") {
		t.Fatal("missing Memory 4.0 MB in view")
	}
}

func TestProcessSelectionAndNavigation(t *testing.T) {
	tm := New()
	tm.SetProcesses([]ProcessRow{
		{ID: 10, Name: "one"},
		{ID: 20, Name: "two"},
		{ID: 30, Name: "three"},
	})

	if tm.SelectedIndex() != 0 || tm.SelectedProcess().ID != 10 {
		t.Fatalf("expected selection 0 (ID 10), got index %d", tm.SelectedIndex())
	}

	tm.SelectNext()
	if tm.SelectedIndex() != 1 || tm.SelectedProcess().ID != 20 {
		t.Fatalf("expected selection 1 (ID 20), got index %d", tm.SelectedIndex())
	}

	tm.SelectNext()
	tm.SelectNext() // Should clamp at index 2
	if tm.SelectedIndex() != 2 || tm.SelectedProcess().ID != 30 {
		t.Fatalf("expected selection 2 (ID 30), got index %d", tm.SelectedIndex())
	}

	tm.SelectPrev()
	if tm.SelectedIndex() != 1 || tm.SelectedProcess().ID != 20 {
		t.Fatalf("expected selection 1 after prev, got index %d", tm.SelectedIndex())
	}
}

func TestLogTailViewing(t *testing.T) {
	tm := New()
	logs := "starting daemon\nlistening on :8080\nclient connected\n"
	tm.ShowLogs(5, "daemon", logs)

	if tm.Mode() != ModeLogs {
		t.Fatalf("expected ModeLogs, got %v", tm.Mode())
	}

	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	bounds := ui.Bounds{Width: 80, Height: 24}
	view := tm.Render(kit, bounds)

	if !strings.Contains(view, "LOG TAIL: daemon (PID 5)") {
		t.Fatal("missing log tail header")
	}
	if !strings.Contains(view, "listening on :8080") {
		t.Fatal("missing log content in log viewer")
	}

	tm.CloseModal()
	if tm.Mode() != ModeTable {
		t.Fatalf("expected ModeTable after CloseModal, got %v", tm.Mode())
	}
}

func TestReceiptInspection(t *testing.T) {
	tm := New()
	receipt := receipts.New(8, "crasher", "crashloop", 137, "out of memory", 5, "fatal: killed")
	tm.ShowReceipt(&receipt)

	if tm.Mode() != ModeReceipt {
		t.Fatalf("expected ModeReceipt, got %v", tm.Mode())
	}

	kit := ui.New(theme.Nostalgia(), ui.ANSI256)
	bounds := ui.Bounds{Width: 80, Height: 24}
	view := tm.Render(kit, bounds)

	if !strings.Contains(view, "CRASH RECEIPT") {
		t.Fatal("missing crash receipt in view")
	}
	if !strings.Contains(view, "crasher") {
		t.Fatal("missing crasher name in view")
	}
	if !strings.Contains(view, "out of memory") {
		t.Fatal("missing reason in view")
	}

	tm.CloseModal()
	if tm.Mode() != ModeTable {
		t.Fatalf("expected ModeTable after CloseModal, got %v", tm.Mode())
	}
}

func TestIPCControls(t *testing.T) {
	ctx := context.Background()
	caller := &mockCaller{
		procs: []ProcessRow{
			{ID: 1, Name: "service-a", State: "running"},
		},
		logs:  "worker log output\n",
		reapN: 3,
	}

	// Fetch processes
	rows, err := FetchProcesses(ctx, caller)
	if err != nil {
		t.Fatalf("FetchProcesses failed: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "service-a" {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	// Stop process
	if err := StopProcess(ctx, caller, 1); err != nil {
		t.Fatalf("StopProcess failed: %v", err)
	}
	if caller.calls["proc/stop"] != 1 {
		t.Fatal("proc/stop was not called")
	}

	// Reap processes
	reaped, err := ReapProcesses(ctx, caller)
	if err != nil {
		t.Fatalf("ReapProcesses failed: %v", err)
	}
	if reaped != 3 {
		t.Fatalf("expected 3 reaped, got %d", reaped)
	}
	if caller.calls["proc/reap"] != 1 {
		t.Fatal("proc/reap was not called")
	}

	// Fetch logs
	logText, err := FetchLogs(ctx, caller, 1, 20)
	if err != nil {
		t.Fatalf("FetchLogs failed: %v", err)
	}
	if !strings.Contains(logText, "worker log output") {
		t.Fatalf("unexpected logs: %q", logText)
	}
}

func TestResourceFormatting(t *testing.T) {
	tests := []struct {
		res     platform.ResourceUsage
		wantCPU string
		wantMem string
	}{
		{
			res:     platform.ResourceUsage{Supported: false},
			wantCPU: "-",
			wantMem: "-",
		},
		{
			res:     platform.ResourceUsage{Supported: true, CPUUserMs: 15, CPUSysMs: 5, MemoryBytes: 512 * 1024},
			wantCPU: "20ms",
			wantMem: "512 KB",
		},
		{
			res:     platform.ResourceUsage{Supported: true, CPUUserMs: 1200, CPUSysMs: 300, MemoryBytes: 15 * 1024 * 1024},
			wantCPU: "1.5s",
			wantMem: "15.0 MB",
		},
	}

	for _, tc := range tests {
		gotCPU := formatCPU(tc.res)
		gotMem := formatMemory(tc.res)
		if gotCPU != tc.wantCPU {
			t.Errorf("formatCPU(%+v) = %q, want %q", tc.res, gotCPU, tc.wantCPU)
		}
		if gotMem != tc.wantMem {
			t.Errorf("formatMemory(%+v) = %q, want %q", tc.res, gotMem, tc.wantMem)
		}
	}
}

// Secondary columns collapse lowest priority first as the table narrows,
// and come back in reverse as it widens; PID and NAME always stay.
func TestColumnsCollapseInPriorityOrder(t *testing.T) {
	var dropped []string
	prev := VisibleColumns(200)
	if strings.Join(prev, " ") != "PID NAME STATE RESTARTS CPU MEMORY" {
		t.Fatalf("wide table shows %v", prev)
	}
	for w := 200; w >= 0; w-- {
		cols := VisibleColumns(w)
		if w >= 2+5+1+8 && (len(cols) < 2 || cols[0] != "PID" || cols[1] != "NAME") {
			t.Fatalf("width %d dropped a primary column: %v", w, cols)
		}
		for _, c := range prev {
			if !slices.Contains(cols, c) && c != "PID" && c != "NAME" {
				dropped = append(dropped, c)
			}
		}
		for _, c := range cols {
			if !slices.Contains(prev, c) {
				t.Fatalf("width %d restored %s while narrowing", w, c)
			}
		}
		prev = cols
	}
	if strings.Join(dropped, " ") != "RESTARTS MEMORY CPU STATE" {
		t.Fatalf("collapse order %v", dropped)
	}
	var restored []string
	prev = VisibleColumns(0)
	for w := 0; w <= 200; w++ {
		cols := VisibleColumns(w)
		for _, c := range cols {
			if !slices.Contains(prev, c) && c != "PID" && c != "NAME" {
				restored = append(restored, c)
			}
		}
		prev = cols
	}
	if strings.Join(restored, " ") != "STATE CPU MEMORY RESTARTS" {
		t.Fatalf("restore order %v", restored)
	}
}

// The PID column widens for long IDs instead of cutting the identifying
// column.
func TestWidePIDsAreNotTruncated(t *testing.T) {
	tm := New()
	tm.SetProcesses([]ProcessRow{
		{ID: 7, Name: "short", State: "running"},
		{ID: 1234567, Name: "long-pid", State: "running"},
	})
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	for _, b := range []ui.Bounds{{Width: 80, Height: 20}, {Width: 30, Height: 4}} {
		if out := tm.Render(kit, b); !strings.Contains(out, "1234567") {
			t.Fatalf("%+v: PID cut:\n%s", b, out)
		}
	}
}

// Below the bordered table's height the selected process stays on screen
// under a header, without panel chrome.
func TestCompactTableKeepsSelection(t *testing.T) {
	tm := New()
	var procs []ProcessRow
	for i := range 6 {
		procs = append(procs, ProcessRow{ID: int32(10 + i), Name: fmt.Sprintf("proc-%d", i), State: "running"})
	}
	tm.SetProcesses(procs)
	for range 4 {
		tm.SelectNext()
	}
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	for _, b := range []ui.Bounds{{Width: 26, Height: 3}, {Width: 26, Height: 1}, {Width: 40, Height: 5}} {
		out := tm.Render(kit, b)
		lines := strings.Split(out, "\n")
		if len(lines) != b.Height || lipgloss.Width(out) != b.Width {
			t.Fatalf("%+v: rendered %dx%d", b, lipgloss.Width(out), len(lines))
		}
		if !strings.Contains(out, kit.Theme().Focus.Marker+" 14") || !strings.Contains(out, "proc-4") {
			t.Fatalf("%+v: selected process missing:\n%s", b, out)
		}
		if b.Height > 1 && !strings.HasPrefix(lines[0], "PID") {
			t.Fatalf("%+v: no header:\n%s", b, out)
		}
	}
}
