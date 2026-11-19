package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"bloomfx.dev/bloom"
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
	mu       sync.RWMutex
	procs    []ProcessRow
	selected int
	scroll   int
	mode     Mode
	logsPID  int32
	logsName string
	// logs holds the log tail of each process opened, kept while the
	// process is in the table.
	logs        map[int32]*processLog
	searching   bool
	searchDraft string
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
	}

	for pid := range m.logs {
		if pid != m.logsPID && !slices.ContainsFunc(m.procs, func(p ProcessRow) bool { return p.ID == pid }) {
			delete(m.logs, pid)
		}
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

// SelectNext moves the table's selection down. The log tail scrolls with
// ScrollLogs instead.
func (m *Model) SelectNext() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModeTable && len(m.procs) > 0 && m.selected < len(m.procs)-1 {
		m.selected++
	}
}

// SelectPrev moves the table's selection up.
func (m *Model) SelectPrev() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModeTable && m.selected > 0 {
		m.selected--
	}
}

// ShowReceipt opens the crash receipt inspection view.
func (m *Model) ShowReceipt(r *receipts.CrashReceipt) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeReceipt
	m.receipt = r
	m.errorMsg = ""
	m.searching, m.searchDraft = false, ""
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
	m.searching, m.searchDraft = false, ""
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
		return bloom.Render(bounds.Width, bounds.Height, m.logsNode(kit, bounds, func() bool { return true }))
	case ModeReceipt:
		if m.receipt != nil {
			return receipts.Render(kit, *m.receipt, bounds)
		}
		return m.renderTable(kit, bounds)
	default:
		return m.renderTable(kit, bounds)
	}
}

func (m *Model) renderTable(kit ui.Kit, bounds ui.Bounds) string {
	if bounds.Height < compactTableHeight {
		return m.renderCompactTable(kit, bounds)
	}
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
	cols := solveColumns(w-markerWidth, m.pidWidth())
	headerRow := columnLine(cols, func(c column) string { return c.title })

	var rows []string
	rows = append(rows, kit.Heading(ui.Truncate(headerRow, w)))
	rows = append(rows, kit.Muted(strings.Repeat("─", min(w, ui.GraphemeWidth(headerRow)))))

	headerCount := len(rows)
	footerReserve := 2 // status and help lines
	availableRows := max(1, h-headerCount-footerReserve)

	rows = append(rows, m.processLines(kit, cols, w, availableRows)...)

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

// compactTableHeight is the smallest height at which the bordered table
// still shows a process row. Below it the table drops its panel chrome and
// footer so the selected process stays identifiable.
const compactTableHeight = 6

// markerWidth is the focus marker and space that prefix every process row.
const markerWidth = 2

// Collapse priorities of the secondary columns; the lowest is dropped first
// when a narrow terminal cannot fit every column's minimum. PID and NAME are
// primary and never collapse: they identify the process the keys act on.
const (
	prioRestarts = 1
	prioMemory   = 2
	prioCPU      = 3
	prioState    = 4
)

// ColumnID identifies a table column in a solved bloom frame.
type ColumnID string

type column struct {
	id       ColumnID
	title    string
	width    int // natural width; the solved width once laid out
	min      int // nonzero: shrinks down to min instead of collapsing
	collapse int // nonzero: Collapsible priority
	cell     func(ProcessRow) string
}

var tableColumns = []column{
	{id: "pid", title: "PID", width: 5, cell: func(p ProcessRow) string { return fmt.Sprint(p.ID) }},
	{id: "name", title: "NAME", width: 20, min: 8, cell: func(p ProcessRow) string { return ui.Sanitize(p.Name) }},
	{id: "state", title: "STATE", width: 14, collapse: prioState, cell: formatState},
	{id: "restarts", title: "RESTARTS", width: 8, collapse: prioRestarts, cell: func(p ProcessRow) string { return fmt.Sprint(p.RestartCount) }},
	{id: "cpu", title: "CPU", width: 8, collapse: prioCPU, cell: func(p ProcessRow) string { return formatCPU(p.Resources) }},
	{id: "memory", title: "MEMORY", width: 10, collapse: prioMemory, cell: func(p ProcessRow) string { return formatMemory(p.Resources) }},
}

// columnsRow is the table's column layout as a bloom row of Collapsible
// slots one cell apart. The PID column widens to pidWidth so no PID is cut.
func columnsRow(pidWidth int) bloom.FlexNode {
	slots := make([]bloom.Child, len(tableColumns))
	for i, c := range tableColumns {
		if c.id == "pid" {
			c.width = max(c.width, pidWidth)
		}
		slot := bloom.Fixed(c.width, nil)
		if c.min > 0 {
			slot = bloom.Fill(nil).Min(c.min).Max(c.width)
		}
		if c.collapse > 0 {
			slot = slot.Collapsible(c.collapse)
		}
		slots[i] = slot.ID(c.id)
	}
	return bloom.Row(slots...).Gap(1)
}

// solveColumns lays the columns out in width cells and returns the visible
// ones at their solved widths.
func solveColumns(width, pidWidth int) []column {
	frame := bloom.Solve(max(0, width), 1, columnsRow(pidWidth))
	var cols []column
	for _, c := range tableColumns {
		if r, ok := frame.Rect(c.id); ok && r.Dx() > 0 {
			c.width = r.Dx()
			cols = append(cols, c)
		}
	}
	return cols
}

// VisibleColumns reports the titles of the columns a table content width
// shows, in order, for PIDs of up to five digits; wider PIDs widen the PID
// column and can drop secondary columns sooner.
func VisibleColumns(width int) []string {
	var titles []string
	for _, c := range solveColumns(width-markerWidth, 0) {
		titles = append(titles, c.title)
	}
	return titles
}

func columnLine(cols []column, text func(column) string) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		s := ui.Truncate(text(c), c.width)
		cells[i] = s + strings.Repeat(" ", max(0, c.width-ui.GraphemeWidth(s)))
	}
	return strings.Join(cells, " ")
}

// pidWidth is the widest PID in the table, in cells.
func (m *Model) pidWidth() int {
	w := 0
	for _, p := range m.procs {
		w = max(w, len(fmt.Sprint(p.ID)))
	}
	return w
}

// window is which processes n rows show, [start, end), windowed so the
// selected row stays visible.
func (m *Model) window(n int) (start, end int) {
	start = max(0, min(m.selected-n+1, max(0, len(m.procs)-n)))
	return start, min(len(m.procs), start+n)
}

// processLines renders up to n process rows, windowed so the selected row
// stays visible.
func (m *Model) processLines(kit ui.Kit, cols []column, w, n int) []string {
	start, end := m.window(n)
	var lines []string
	for i := start; i < end; i++ {
		p := m.procs[i]
		line := columnLine(cols, func(c column) string { return c.cell(p) })
		if i == m.selected {
			lines = append(lines, kit.Selection(ui.Truncate(kit.Theme().Focus.Marker+" "+line, w)))
		} else {
			lines = append(lines, kit.Text(ui.Truncate("  "+line, w)))
		}
	}
	return lines
}

// RowSpan is where the table shows a process's row in the bounds it was
// rendered in: the row's PID, its line and the cells across it.
type RowSpan struct {
	PID         int32
	X, Y, Width int
}

// TableRows reports the process rows Render shows in bounds, top to
// bottom, as it lays them out. Only the table has rows; the log tail and a
// receipt have none.
func (m *Model) TableRows(kit ui.Kit, bounds ui.Bounds) []RowSpan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if bounds.Width <= 0 || bounds.Height <= 0 || len(m.procs) == 0 ||
		m.mode == ModeLogs || (m.mode == ModeReceipt && m.receipt != nil) {
		return nil
	}
	x, y, w, n, bottom := 0, 0, bounds.Width, bounds.Height, bounds.Height
	if bounds.Height < compactTableHeight {
		if n >= 2 {
			y, n = 1, n-1
		}
	} else {
		cx, cy, content := kit.PanelContentRect(bounds)
		// The header and its rule sit above the rows, the status and help
		// lines below them, as renderTable lays them out.
		x, y, w, n, bottom = cx, cy+2, content.Width, max(1, content.Height-4), cy+content.Height
	}
	start, end := m.window(n)
	spans := make([]RowSpan, 0, end-start)
	for i := start; i < end && y+i-start < bottom; i++ {
		spans = append(spans, RowSpan{PID: m.procs[i].ID, X: x, Y: y + i - start, Width: w})
	}
	return spans
}

// SelectPID selects the row of process pid in the table and reports
// whether the table has it.
func (m *Model) SelectPID(pid int32) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode != ModeTable {
		return false
	}
	for i, p := range m.procs {
		if p.ID == pid {
			m.selected = i
			return true
		}
	}
	return false
}

// SetLogsPos scrolls the open log tail to pos, as ScrollLogs does, such as
// for the mouse wheel. A tail that is not following stays where it is
// scrolled to, its end included, until ToggleFollow.
func (m *Model) SetLogsPos(pos bloom.ViewportPos) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.openLog(); l != nil {
		l.pos = pos
	}
}

// LogsInView is the text of the open log tail's lines that f, a frame
// solved with it, shows, a line each and without styles, or "" while none
// is open.
func (m *Model) LogsInView(f *bloom.Frame) string {
	v, ok := f.Viewport(LogsViewport)
	m.mu.RLock()
	defer m.mu.RUnlock()
	l := m.openLog()
	if !ok || l == nil || v.Rows <= 0 || l.buf.Len() == 0 {
		return ""
	}
	return ui.ViewportText(v, l.buf)
}

// renderCompactTable keeps the table usable in a body too short for its
// panel: a header when there is room, then the rows around the selection.
func (m *Model) renderCompactTable(kit ui.Kit, bounds ui.Bounds) string {
	w := bounds.Width
	if len(m.procs) == 0 {
		return ui.Fit(kit.Muted("No Processes Found"), bounds)
	}
	cols := solveColumns(w-markerWidth, m.pidWidth())
	var lines []string
	n := bounds.Height
	if n >= 2 {
		lines = append(lines, kit.Heading(ui.Truncate(columnLine(cols, func(c column) string { return c.title }), w)))
		n--
	}
	lines = append(lines, m.processLines(kit, cols, w, n)...)
	return ui.Fit(strings.Join(lines, "\n"), bounds)
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
