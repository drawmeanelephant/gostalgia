package taskmanager

import (
	"context"
	"encoding/json"
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
