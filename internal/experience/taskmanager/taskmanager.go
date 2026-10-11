package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/viewport"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/platform"
)

// Caller defines the IPC client interface used by Task Manager.
type Caller interface {
	Call(context.Context, string, any, any) error
}

// Mode represents current view state inside the Task Manager.
type Mode int

const (
	ModeTable Mode = iota
	ModeLogs
	ModeReceipt
)

// ProcessRow represents one process row in the Task Manager table.
type ProcessRow struct {
	ID           int32                  `json:"id"`
	Name         string                 `json:"name"`
	Kind         string                 `json:"kind"`
	State        string                 `json:"state"`
	ExitCode     int                    `json:"exit_code"`
	StartedAt    time.Time              `json:"started_at"`
	ExitedAt     time.Time              `json:"exited_at"`
	RestartCount int                    `json:"restart_count"`
	CrashLoop    bool                   `json:"crash_loop"`
	Resources    platform.ResourceUsage `json:"resources"`
	Error        string                 `json:"error"`
}

// Model manages state and rendering for the live Task Manager.
type Model struct {
	mu          sync.RWMutex
	procs       []ProcessRow
	selected    int
	scroll      int
	mode        Mode
	logsPID     int32
	logsName    string
	logsContent string
	logsScroll  int
	receipt     *receipts.CrashReceipt
	statusMsg   string
	errorMsg    string
	lastUpdate  time.Time
}

// New creates a new Task Manager model.
func New() *Model {
	return &Model{
		procs: make([]ProcessRow, 0),
		mode:  ModeTable,
	}
}

// Mode returns current display mode.
func (m *Model) Mode() Mode {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.mode
}

// SetProcesses updates the process list, preserving or clamping current selection.
func (m *Model) SetProcesses(procs []ProcessRow) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.procs = make([]ProcessRow, len(procs))
	copy(m.procs, procs)
	m.lastUpdate = time.Now()

	// Sort by PID ascending
	sort.Slice(m.procs, func(i, j int) bool {
		return m.procs[i].ID < m.procs[j].ID
	})

	if len(m.procs) == 0 {
		m.selected = 0
		m.scroll = 0
		return
	}

	if m.selected >= len(m.procs) {
		m.selected = len(m.procs) - 1
	}
	if m.selected < 0 {
		m.selected = 0
	}
}

// Processes returns a copy of current processes.
func (m *Model) Processes() []ProcessRow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ProcessRow, len(m.procs))
	copy(out, m.procs)
	return out
}

// SelectedProcess returns the currently selected process row, or nil if none.
func (m *Model) SelectedProcess() *ProcessRow {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.procs) == 0 || m.selected < 0 || m.selected >= len(m.procs) {
		return nil
	}
	p := m.procs[m.selected]
	return &p
}

// SelectedIndex returns the selected index.
func (m *Model) SelectedIndex() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selected
}

// SelectNext moves selection down.
func (m *Model) SelectNext() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModeLogs {
		m.logsScroll++
		return
	}
	if len(m.procs) > 0 && m.selected < len(m.procs)-1 {
		m.selected++
	}
}

// SelectPrev moves selection up.
func (m *Model) SelectPrev() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModeLogs {
		if m.logsScroll > 0 {
			m.logsScroll--
		}
		return
	}
	if m.selected > 0 {
		m.selected--
	}
}

// ShowLogs sets the logs view mode for a process.
func (m *Model) ShowLogs(pid int32, name, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeLogs
	m.logsPID = pid
	m.logsName = ui.Sanitize(name)
	m.logsContent = ui.Sanitize(content)
	m.logsScroll = 0
	m.errorMsg = ""
}

// ShowReceipt opens the crash receipt inspection view.
func (m *Model) ShowReceipt(r *receipts.CrashReceipt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeReceipt
	m.receipt = r
	m.errorMsg = ""
}

// Receipt returns the currently displayed crash receipt if in ModeReceipt.
func (m *Model) Receipt() *receipts.CrashReceipt {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.receipt
}

// UpdateReceipt updates the displayed receipt if currently in ModeReceipt.
func (m *Model) UpdateReceipt(r *receipts.CrashReceipt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModeReceipt && m.receipt != nil && r != nil && m.receipt.ID == r.ID {
		m.receipt = r
	}
}

// CloseModal returns to table view.
func (m *Model) CloseModal() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeTable
	m.receipt = nil
	m.logsContent = ""
}

// SetStatus sets a transient informational status message.
func (m *Model) SetStatus(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusMsg = ui.Sanitize(msg)
	m.errorMsg = ""
}

// SetError sets an error message.
func (m *Model) SetError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.errorMsg = ui.Sanitize(msg)
	m.statusMsg = ""
}

// Status returns current status and error messages.
func (m *Model) Status() (string, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.statusMsg, m.errorMsg
}

// FetchProcesses queries proc/list over IPC and returns parsed rows.
func FetchProcesses(ctx context.Context, client Caller) ([]ProcessRow, error) {
	var raw []struct {
		ID           int32                  `json:"id"`
		Name         string                 `json:"name"`
		Kind         string                 `json:"kind"`
		State        string                 `json:"state"`
		ExitCode     int                    `json:"exit_code"`
		StartedAt    time.Time              `json:"started_at"`
		ExitedAt     time.Time              `json:"exited_at"`
		RestartCount int                    `json:"restart_count"`
		CrashLoop    bool                   `json:"crash_loop"`
		Resources    platform.ResourceUsage `json:"resources"`
		Error        string                 `json:"error"`
	}
	if err := client.Call(ctx, "proc/list", nil, &raw); err != nil {
		return nil, err
	}

	rows := make([]ProcessRow, len(raw))
	for i, p := range raw {
		rows[i] = ProcessRow{
			ID:           p.ID,
			Name:         p.Name,
			Kind:         p.Kind,
			State:        p.State,
			ExitCode:     p.ExitCode,
			StartedAt:    p.StartedAt,
			ExitedAt:     p.ExitedAt,
			RestartCount: p.RestartCount,
			CrashLoop:    p.CrashLoop,
			Resources:    p.Resources,
			Error:        p.Error,
		}
	}
	return rows, nil
}

// StopProcess sends an authenticated IPC stop request.
func StopProcess(ctx context.Context, client Caller, pid int32) error {
	var resp json.RawMessage
	return client.Call(ctx, "proc/stop", map[string]any{"id": pid}, &resp)
}

// ReapProcesses sends an authenticated IPC reap request.
func ReapProcesses(ctx context.Context, client Caller) (int, error) {
	var resp struct {
		Reaped int `json:"reaped"`
	}
	if err := client.Call(ctx, "proc/reap", map[string]any{}, &resp); err != nil {
		return 0, err
	}
	return resp.Reaped, nil
}

// FetchLogs retrieves bounded child log tail for a PID over IPC.
func FetchLogs(ctx context.Context, client Caller, pid int32, tail int) (string, error) {
	params := map[string]any{"id": pid, "stream": "combined"}
	if tail > 0 {
		params["tail"] = tail
	}
	var resp struct {
		Combined struct {
			Content string `json:"content"`
		} `json:"combined"`
		Stdout struct {
			Content string `json:"content"`
		} `json:"stdout"`
		Stderr struct {
			Content string `json:"content"`
		} `json:"stderr"`
	}
	if err := client.Call(ctx, "proc/logs", params, &resp); err != nil {
		return "", err
	}
	if resp.Combined.Content != "" {
		return resp.Combined.Content, nil
	}
	if resp.Stderr.Content != "" {
		return resp.Stderr.Content, nil
	}
	return resp.Stdout.Content, nil
}

// Render formats the Task Manager view depending on the current mode.
func (m *Model) Render(kit ui.Kit, bounds ui.Bounds) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}

	switch m.mode {
	case ModeLogs:
		return m.renderLogs(kit, bounds)
	case ModeReceipt:
		if m.receipt != nil {
			return receipts.Render(kit, *m.receipt, bounds)
		}
		return m.renderTable(kit, bounds)
	default:
		return m.renderTable(kit, bounds)
	}
}

func (m *Model) renderLogs(kit ui.Kit, bounds ui.Bounds) string {
	contentBounds := kit.PanelContentBounds(bounds)
	w := contentBounds.Width
	h := contentBounds.Height

	var lines []string
	lines = append(lines, kit.Heading(fmt.Sprintf("CHILD LOG TAIL: %s (PID %d)", m.logsName, m.logsPID)))
	lines = append(lines, kit.Muted("Esc or Enter returns to process table · ↑↓ scroll"))
	lines = append(lines, "")

	rawLines := strings.Split(m.logsContent, "\n")
	if len(rawLines) == 0 || (len(rawLines) == 1 && rawLines[0] == "") {
		lines = append(lines, kit.Muted("(no output captured yet)"))
	} else {
		availableRows := max(1, h-len(lines))
		// The log tail delegates its clamp to bubbles/viewport (issue #185);
		// logsScroll stays the canonical offset so SelectPrev/SelectNext
		// and tests keep the same contract.
		vp := viewport.New(viewport.WithHeight(availableRows))
		vp.SetContentLines(rawLines)
		vp.SetYOffset(m.logsScroll)
		scroll := vp.YOffset()
		visible := rawLines[scroll:min(len(rawLines), scroll+availableRows)]
		for _, l := range visible {
			lines = append(lines, kit.Text(ui.Truncate("  "+l, w)))
		}
	}

	return kit.Panel(ui.Panel{
		Title:   fmt.Sprintf("LOGS · PID %d", m.logsPID),
		Body:    strings.Join(lines, "\n"),
		Focused: true,
	}, bounds)
}

func (m *Model) renderTable(kit ui.Kit, bounds ui.Bounds) string {
	contentBounds := kit.PanelContentBounds(bounds)
	w := contentBounds.Width
	h := contentBounds.Height

	if len(m.procs) == 0 {
		emptyNotice := kit.Notice(ui.Notice{
			Title:   "No Processes Found",
			Message: "No environment or child processes are currently registered.",
			State:   theme.Empty,
		}, ui.Bounds{Width: w, Height: h})
		return kit.Panel(ui.Panel{
			Title:   "TASK MANAGER",
			Body:    emptyNotice,
			Focused: true,
		}, bounds)
	}

	// bubbles/table was evaluated and lost (issue #185): it marks the
	// selected row only through style, which renders nothing in plain and
	// monochrome modes where the contract requires the theme's text focus
	// marker, and its self-sizing columns fight the fixed DOS column
	// budget below.
	// Columns:
	// PID (5) | NAME (20) | STATE (14) | RESTARTS (8) | CPU (8) | MEMORY (10)
	headerRow := fmt.Sprintf("%-5s %-20s %-14s %-8s %-8s %-10s",
		"PID", "NAME", "STATE", "RESTARTS", "CPU", "MEMORY")

	var rows []string
	rows = append(rows, kit.Heading(ui.Truncate(headerRow, w)))
	rows = append(rows, kit.Muted(strings.Repeat("─", min(w, len(headerRow)))))

	headerCount := len(rows)
	footerReserve := 2 // status and help lines
	availableRows := max(1, h-headerCount-footerReserve)

	start := max(0, min(m.selected-availableRows+1, max(0, len(m.procs)-availableRows)))
	end := min(len(m.procs), start+availableRows)

	for i := start; i < end; i++ {
		p := m.procs[i]
		focused := i == m.selected

		stateStr := formatState(p)
		cpuStr := formatCPU(p.Resources)
		memStr := formatMemory(p.Resources)
		nameStr := ui.Truncate(ui.Sanitize(p.Name), 20)

		line := fmt.Sprintf("%-5d %-20s %-14s %-8d %-8s %-10s",
			p.ID, nameStr, stateStr, p.RestartCount, cpuStr, memStr)

		if focused {
			line = kit.Selection(ui.Truncate(kit.Theme().Focus.Marker+" "+line, w))
		} else {
			line = kit.Text(ui.Truncate("  "+line, w))
		}
		rows = append(rows, line)
	}

	// Pad empty space if needed
	for len(rows) < h-footerReserve {
		rows = append(rows, "")
	}

	// Status / Error message
	if m.errorMsg != "" {
		sym := kit.Theme().Status(theme.Error).Symbol
		if sym != "" {
			sym += " "
		}
		rows = append(rows, kit.StatusText(ui.Truncate(sym+m.errorMsg, w), theme.Error))
	} else if m.statusMsg != "" {
		sym := kit.Theme().Status(theme.Success).Symbol
		if sym != "" {
			sym += " "
		}
		rows = append(rows, kit.StatusText(ui.Truncate(sym+m.statusMsg, w), theme.Success))
	} else {
		liveCount := 0
		for _, p := range m.procs {
			if p.State == "running" || p.State == "starting" || p.State == "restarting" {
				liveCount++
			}
		}
		rows = append(rows, kit.Muted(ui.Truncate(fmt.Sprintf("%d total processes (%d active)", len(m.procs), liveCount), w)))
	}

	return kit.Panel(ui.Panel{
		Title:   "LIVE TASK MANAGER",
		Body:    strings.Join(rows, "\n"),
		Focused: true,
	}, bounds)
}

func formatState(p ProcessRow) string {
	if p.CrashLoop {
		return "crash loop"
	}
	switch p.State {
	case "restarting":
		return "backoff"
	case "running":
		return "running"
	case "stopped":
		return "stopped"
	case "failed":
		return "failed"
	case "starting":
		return "starting"
	case "stopping":
		return "stopping"
	default:
		return p.State
	}
}

func formatCPU(res platform.ResourceUsage) string {
	if !res.Supported {
		return "-"
	}
	totalMs := res.CPUUserMs + res.CPUSysMs
	if totalMs >= 1000 {
		return fmt.Sprintf("%.1fs", float64(totalMs)/1000.0)
	}
	return fmt.Sprintf("%dms", totalMs)
}

func formatMemory(res platform.ResourceUsage) string {
	if !res.Supported {
		return "-"
	}
	bytes := res.MemoryBytes
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	if bytes >= gb {
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(gb))
	}
	if bytes >= mb {
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
	}
	if bytes >= kb {
		return fmt.Sprintf("%d KB", bytes/kb)
	}
	return fmt.Sprintf("%d B", bytes)
}
