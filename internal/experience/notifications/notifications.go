package notifications

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

type Level string

const (
	LevelInfo    Level = "info"
	LevelWarning Level = "warning"
	LevelError   Level = "error"

	DefaultMaxHistory      = 100
	DefaultMaxActiveToasts = 3
	DefaultToastTicks      = 8 // ~4 seconds at 500ms intervals
)

// Notification represents a system event recorded in history.
// Notifications distinguish accepted/recorded events from claims that the user actually saw them.
type Notification struct {
	ID        string                 `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Title     string                 `json:"title"`
	Message   string                 `json:"message"`
	Level     Level                  `json:"level"`
	Source    string                 `json:"source"`
	PID       int32                  `json:"pid,omitempty"`
	Seen      bool                   `json:"seen"`
	Receipt   *receipts.CrashReceipt `json:"receipt,omitempty"`
	// Badge names the notification in place of its level's label, for
	// notices that are neither crashes nor restart backoffs.
	Badge string `json:"badge,omitempty"`
	// Wrap lets the notification's toast wrap its message over up to
	// maxToastRows rows rather than cut it to one, for a message the reader
	// needs whole, such as how to change a policy.
	Wrap bool `json:"wrap,omitempty"`
}

// Toast represents a transient popup on the user's screen.
type Toast struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Message   string    `json:"message"`
	Level     Level     `json:"level"`
	CreatedAt time.Time `json:"created_at"`
	TicksLeft int       `json:"ticks_left"`
	PID       int32     `json:"pid,omitempty"`
	Badge     string    `json:"badge,omitempty"`
	Wrap      bool      `json:"wrap,omitempty"`
}

// maxToastRows is the most rows a wrapping toast takes.
const maxToastRows = 5

// Manager maintains notification history, active transient toasts, and Do-Not-Disturb preferences.
type Manager struct {
	mu                sync.RWMutex
	maxHistory        int
	maxActiveToasts   int
	defaultToastTicks int
	history           []Notification
	toasts            []Toast
	dnd               bool
	droppedToasts     uint64
	selected          int
	scroll            int
	nextID            uint64
}

// NewManager creates a Notification Center manager.
func NewManager(maxHistory, maxActiveToasts, defaultToastTicks int) *Manager {
	if maxHistory <= 0 {
		maxHistory = DefaultMaxHistory
	}
	if maxActiveToasts <= 0 {
		maxActiveToasts = DefaultMaxActiveToasts
	}
	if defaultToastTicks <= 0 {
		defaultToastTicks = DefaultToastTicks
	}
	return &Manager{
		maxHistory:        maxHistory,
		maxActiveToasts:   maxActiveToasts,
		defaultToastTicks: defaultToastTicks,
		history:           make([]Notification, 0, maxHistory),
		toasts:            make([]Toast, 0, maxActiveToasts),
	}
}

// SetDND enables or disables Do-Not-Disturb.
func (m *Manager) SetDND(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dnd = enabled
	if enabled {
		// Clear transient popups immediately when DND is enabled
		m.toasts = m.toasts[:0]
	}
}

// ToggleDND flips Do-Not-Disturb and returns the new value.
func (m *Manager) ToggleDND() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dnd = !m.dnd
	if m.dnd {
		m.toasts = m.toasts[:0]
	}
	return m.dnd
}

// DND reports whether Do-Not-Disturb is currently active.
func (m *Manager) DND() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dnd
}

// Record records an event in history. If DND is inactive, it also creates a transient toast.
// The event is recorded with Seen=false: recording does NOT imply the user saw it.
func (m *Manager) Record(n Notification) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	n = m.prepareLocked(n)

	// Prepend to history (newest first)
	m.history = append([]Notification{n}, m.history...)
	if len(m.history) > m.maxHistory {
		m.history = m.history[:m.maxHistory]
	}

	// Adjust selection if needed
	if m.selected >= len(m.history) {
		m.selected = max(0, len(m.history)-1)
	}

	return m.toastLocked(n)
}

// Flash shows n as a toast without recording it in history, for feedback
// that matters only as it happens, such as a copy that worked. Under DND it
// shows nothing.
func (m *Manager) Flash(n Notification) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.toastLocked(m.prepareLocked(n))
}

// prepareLocked gives n an ID and time if it has none, sanitizes its text
// and marks it unseen.
func (m *Manager) prepareLocked(n Notification) Notification {
	m.nextID++
	if n.ID == "" {
		n.ID = fmt.Sprintf("ntf-%d-%d", m.nextID, time.Now().UnixNano())
	}
	if n.Timestamp.IsZero() {
		n.Timestamp = time.Now()
	}
	n.Title = ui.Sanitize(n.Title)
	n.Message = ui.Sanitize(n.Message)
	n.Source = ui.Sanitize(n.Source)
	n.Badge = ui.Sanitize(n.Badge)
	n.Seen = false // Explicitly unread / unseen upon arrival
	return n
}

// toastLocked shows n as a toast unless DND is on, and reports whether it
// did.
func (m *Manager) toastLocked(n Notification) bool {
	// Transient toast generation with flood bounding and DND suppression
	if m.dnd {
		// DND suppresses interruption without destroying history
		return false
	}

	// Flood bounding: drop oldest active toast if capacity is exceeded
	if len(m.toasts) >= m.maxActiveToasts {
		m.toasts = m.toasts[1:]
		m.droppedToasts++
	}

	m.toasts = append(m.toasts, Toast{
		ID:        n.ID,
		Title:     n.Title,
		Message:   n.Message,
		Level:     n.Level,
		CreatedAt: n.Timestamp,
		TicksLeft: m.defaultToastTicks,
		PID:       n.PID,
		Badge:     n.Badge,
		Wrap:      n.Wrap,
	})
	return true
}

// Tick decrements countdown on transient toasts and removes expired ones.
func (m *Manager) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()

	var active []Toast
	for _, t := range m.toasts {
		t.TicksLeft--
		if t.TicksLeft > 0 {
			active = append(active, t)
		}
	}
	m.toasts = active
}

// Dismiss removes a notification by ID from history.
func (m *Manager) Dismiss(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	for i, n := range m.history {
		if n.ID == id {
			m.history = append(m.history[:i], m.history[i+1:]...)
			if m.selected >= len(m.history) {
				m.selected = max(0, len(m.history)-1)
			}
			return true
		}
	}
	return false
}

// DismissSelected removes the currently selected notification.
func (m *Manager) DismissSelected() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(m.history) == 0 || m.selected < 0 || m.selected >= len(m.history) {
		return false
	}
	m.history = append(m.history[:m.selected], m.history[m.selected+1:]...)
	if m.selected >= len(m.history) {
		m.selected = max(0, len(m.history)-1)
	}
	return true
}

// Clear removes all notifications from history.
func (m *Manager) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = m.history[:0]
	m.selected = 0
	m.scroll = 0
}

// MarkSeen marks a specific notification as seen.
func (m *Manager) MarkSeen(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.history {
		if m.history[i].ID == id {
			m.history[i].Seen = true
			return
		}
	}
}

// MarkAllSeen marks all current notifications in history as seen.
func (m *Manager) MarkAllSeen() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.history {
		m.history[i].Seen = true
	}
}

// UnseenCount returns the number of notifications not yet seen by the user.
func (m *Manager) UnseenCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for _, n := range m.history {
		if !n.Seen {
			count++
		}
	}
	return count
}

// History returns a snapshot of notification history.
func (m *Manager) History() []Notification {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Notification, len(m.history))
	copy(out, m.history)
	return out
}

// ActiveToasts returns a snapshot of currently visible toasts.
func (m *Manager) ActiveToasts() []Toast {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Toast, len(m.toasts))
	copy(out, m.toasts)
	return out
}

// DroppedToasts returns the count of toasts dropped due to flood bounding.
func (m *Manager) DroppedToasts() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.droppedToasts
}

// Selected returns the currently selected notification, or nil if empty.
func (m *Manager) Selected() *Notification {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.history) == 0 || m.selected < 0 || m.selected >= len(m.history) {
		return nil
	}
	n := m.history[m.selected]
	return &n
}

// SelectedIndex returns the current selection index.
func (m *Manager) SelectedIndex() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.selected
}

// SelectNext moves selection down.
func (m *Manager) SelectNext() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.history) > 0 && m.selected < len(m.history)-1 {
		m.selected++
	}
}

// SelectPrev moves selection up.
func (m *Manager) SelectPrev() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.selected > 0 {
		m.selected--
	}
}

// RenderToasts formats active toasts into bounded lines suitable for overlay or prompt integration.
func (m *Manager) RenderToasts(kit ui.Kit, width int) string {
	toasts, notice := m.ToastRows(kit, width)
	var rows []string
	for _, t := range toasts {
		rows = append(rows, t...)
	}
	if notice != "" {
		rows = append(rows, notice)
	}
	return strings.Join(rows, "\n")
}

// ToastRows formats the active toasts oldest first, each as its rows at
// width, and the flood-bounding notice, if any, that goes beneath them.
func (m *Manager) ToastRows(kit ui.Kit, width int) (toasts [][]string, notice string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.toasts) == 0 || m.dnd || width <= 0 {
		return nil, ""
	}

	for _, t := range m.toasts {
		cue := toastCue(kit, t.Level, t.Badge, t.Title, width)
		title := kit.Heading(t.Title)
		msg := kit.Text(t.Message)

		line := fmt.Sprintf("%s %s%s%s", cue, title, kit.Separator(), msg)
		if t.Wrap {
			wrapped := strings.Split(ansi.Wrap(line, width, ""), "\n")
			if len(wrapped) > maxToastRows {
				wrapped = wrapped[:maxToastRows]
				wrapped[maxToastRows-1] = kit.Truncate(wrapped[maxToastRows-1]+kit.Hint("…"), width)
			}
			toasts = append(toasts, wrapped)
			continue
		}
		toasts = append(toasts, []string{kit.Truncate(line, width)})
	}

	if m.droppedToasts > 0 {
		notice = kit.Truncate(kit.Muted(fmt.Sprintf("(+%d alerts suppressed by flood bounding)", m.droppedToasts)), width)
	}
	return toasts, notice
}

// severityFor is a level's mandatory cue: the symbol and label every surface
// shows, and the theme state that colors it. INFO stays neutral, WARN uses the
// busy state's attention color, and ERROR the failure color.
func severityFor(level Level) (symbol, name string, state theme.State) {
	switch level {
	case LevelError:
		return "■", "ERROR", theme.Error
	case LevelWarning:
		return "▲", "WARN", theme.Busy
	default:
		return "◆", "INFO", theme.Normal
	}
}

// toastCue is a toast's severity cue. Its custom badge yields when keeping it
// would push the title and its separator off the first toast row: the badge
// is supplementary, the severity and title are not.
func toastCue(kit ui.Kit, level Level, custom, title string, width int) string {
	if custom == "" {
		return severityCue(kit, level, "", width)
	}
	symbol, name, state := severityFor(level)
	head := ui.GraphemeWidth(kit.Severity(symbol, name, state, width)) + 1 +
		ui.GraphemeWidth(customText(custom)) + 1 + ui.GraphemeWidth(title) + ui.GraphemeWidth(kit.Separator())
	if head > width {
		return severityCue(kit, level, "", width)
	}
	return severityCue(kit, level, custom, width)
}

// customText is a notification's custom badge as one sanitized line.
func customText(custom string) string {
	return strings.ReplaceAll(ui.Sanitize(custom), "\n", " ")
}

// severityCue renders level's mandatory cue, then the notification's custom
// badge when it has one. The custom badge supplements the severity; it never
// replaces it.
func severityCue(kit ui.Kit, level Level, custom string, width int) string {
	symbol, name, state := severityFor(level)
	cue := kit.Severity(symbol, name, state, width)
	if custom == "" {
		return cue
	}
	custom = customText(custom)
	room := width - ui.GraphemeWidth(cue) - 1
	if room <= 0 {
		return cue
	}
	return cue + " " + kit.StatusText(kit.Truncate(custom, room), state)
}

// severityColumn is the display width of the widest severity cue. The full
// panel reserves it before deciding whether a title still fits beside the
// markers and timestamp.
func severityColumn(kit ui.Kit) int {
	width := 0
	for _, level := range []Level{LevelInfo, LevelWarning, LevelError} {
		symbol, name, state := severityFor(level)
		width = max(width, ui.GraphemeWidth(kit.Severity(symbol, name, state, 64)))
	}
	return width
}

// RenderPanel formats the full notification history panel. The list windows
// by selection rather than adopting bubbles/paginator (issue #185): the
// selected entry must stay visible, and a page index can't express
// "follow the cursor" for receipt, palette, and launcher lists either.
func (m *Manager) RenderPanel(kit ui.Kit, bounds ui.Bounds) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}

	if compactPanel(kit, bounds) {
		return m.renderCompactPanel(kit, bounds)
	}

	contentBounds := kit.PanelContentBounds(bounds)
	w := contentBounds.Width
	bodyHeight := contentBounds.Height

	// Header summary
	dndStatus := "DND OFF"
	if m.dnd {
		dndStatus = "DND ON (toasts suppressed)"
	}
	header := fmt.Sprintf("NOTIFICATIONS: %d total%s%d unread%s%s", len(m.history), kit.Separator(), m.unseenCountLocked(), kit.Separator(), dndStatus)

	if len(m.history) == 0 {
		emptyNotice := kit.Notice(ui.Notice{
			Title:   "Notification Center Clean",
			Message: "No recent system events or crashes recorded.\nDo-Not-Disturb: " + dndStatus,
			State:   theme.Empty,
		}, ui.Bounds{Width: w, Height: bodyHeight})
		return kit.Panel(ui.Panel{
			Title:   "NOTIFICATION CENTER",
			Body:    emptyNotice,
			Focused: true,
		}, bounds)
	}

	start, end := m.window(panelItems(bodyHeight))

	var lines []string
	lines = append(lines, kit.Muted(kit.Truncate(header, w)), "")

	for i := start; i < end; i++ {
		n := m.history[i]
		focused := i == m.selected

		timeStr := n.Timestamp.Format("15:04:05")
		cue := severityCue(kit, n.Level, n.Badge, w)
		titleText := n.Title
		if n.Receipt != nil {
			titleText += " [RECEIPT AVAILABLE]"
		}

		headerLine := fmt.Sprintf("%s%s%s [%s] %s", entryPrefix(kit, focused), seenMarker(kit, n), cue, timeStr, titleText)
		detailLine := fmt.Sprintf("    %s", n.Message)

		if focused {
			headerLine = kit.Selection(kit.Truncate(headerLine, w))
			detailLine = kit.Text(kit.Truncate(detailLine, w))
		} else {
			headerLine = kit.Heading(kit.Truncate(headerLine, w))
			detailLine = kit.Muted(kit.Truncate(detailLine, w))
		}

		lines = append(lines, headerLine, detailLine)
	}

	return kit.Panel(ui.Panel{
		Title:   "NOTIFICATION CENTER",
		Body:    strings.Join(lines, "\n"),
		Focused: true,
	}, bounds)
}

// compactPanelHeight is the smallest height at which the bordered panel
// shows its summary and one whole entry.
const compactPanelHeight = 7

// minTitleCells is how much of an entry's title the bordered panel's
// header row must leave room for after its markers, severity cue and
// timestamp.
const minTitleCells = 12

// compactPanel reports whether bounds are too short for one whole entry or
// too narrow for its title in the bordered panel. The panel then drops its
// chrome, custom badge and timestamp so the selected entry stays readable.
func compactPanel(kit ui.Kit, bounds ui.Bounds) bool {
	if bounds.Height < compactPanelHeight {
		return true
	}
	w := kit.PanelContentBounds(bounds).Width
	header := ui.GraphemeWidth(entryPrefix(kit, true)) + ui.GraphemeWidth(seenMarker(kit, Notification{})) + severityColumn(kit) + len(" [15:04:05] ")
	return w-header < minTitleCells
}

// renderCompactPanel is the Notification Center for a body too short for
// its panel: a one-line summary when there is room, then the entries
// around the selection, each as its severity cue, title and message.
func (m *Manager) renderCompactPanel(kit ui.Kit, bounds ui.Bounds) string {
	w, rows := bounds.Width, bounds.Height
	dndStatus := "DND OFF"
	if m.dnd {
		dndStatus = "DND ON"
	}
	if len(m.history) == 0 {
		return ui.Fit(kit.Muted(kit.Truncate("No notifications"+kit.Separator()+dndStatus, w)), bounds)
	}
	var lines []string
	if rows >= 3 {
		summary := fmt.Sprintf("%d/%d%s%d unread%s%s", m.selected+1, len(m.history), kit.Separator(), m.unseenCountLocked(), kit.Separator(), dndStatus)
		lines = append(lines, kit.Muted(kit.Truncate(summary, w)))
		rows--
	}
	start, end := m.window(max(1, rows/itemRows))
	for i := start; i < end; i++ {
		n := m.history[i]
		symbol, name, state := severityFor(n.Level)
		cue := kit.Severity(symbol, name, state, w)
		headerLine := entryPrefix(kit, i == m.selected) + seenMarker(kit, n) + cue + " " + n.Title
		// The receipt tag yields before the title does: the compact row has
		// room for the selected entry's identity first.
		if n.Receipt != nil && ui.GraphemeWidth(headerLine+" [RECEIPT]") <= w {
			headerLine += " [RECEIPT]"
		}
		headerLine = kit.Truncate(headerLine, w)
		detailLine := kit.Truncate("    "+n.Message, w)
		if i == m.selected {
			lines = append(lines, kit.Selection(headerLine), kit.Text(detailLine))
		} else {
			lines = append(lines, kit.Heading(headerLine), kit.Muted(detailLine))
		}
	}
	return ui.Fit(strings.Join(lines, "\n"), bounds)
}

// itemRows is how many rows an entry takes: its title, then its message.
const itemRows = 2

// panelItems is how many entries the bordered panel shows in a body of
// bodyHeight rows: everything below its summary and the blank row under it.
func panelItems(bodyHeight int) int { return max(1, max(1, bodyHeight-2)/itemRows) }

// window is which entries n of them on screen are, [start, end), windowed
// so the selected one stays visible.
func (m *Manager) window(n int) (start, end int) {
	start = max(0, min(m.selected-n+1, max(0, len(m.history)-n)))
	return start, min(len(m.history), start+n)
}

// EntrySpan is where the Notification Center shows an entry in the bounds it
// was rendered in: the entry's ID and its rows and cells.
type EntrySpan struct {
	ID                  string
	X, Y, Width, Height int
}

// EntrySpans reports the entries RenderPanel shows in bounds, top to
// bottom, as it lays them out.
func (m *Manager) EntrySpans(kit ui.Kit, bounds ui.Bounds) []EntrySpan {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if bounds.Width <= 0 || bounds.Height <= 0 || len(m.history) == 0 {
		return nil
	}
	var x, y, w, bottom, start, end int
	if compactPanel(kit, bounds) {
		rows := bounds.Height
		if rows >= 3 {
			y, rows = 1, rows-1
		}
		w, bottom = bounds.Width, bounds.Height
		start, end = m.window(max(1, rows/itemRows))
	} else {
		cx, cy, content := kit.PanelContentRect(bounds)
		// The summary and a blank row sit above the entries.
		x, y, w, bottom = cx, cy+2, content.Width, cy+content.Height
		start, end = m.window(panelItems(content.Height))
	}
	spans := make([]EntrySpan, 0, end-start)
	for i := start; i < end; i++ {
		top := y + (i-start)*itemRows
		if top >= bottom {
			break
		}
		spans = append(spans, EntrySpan{ID: m.history[i].ID, X: x, Y: top, Width: w, Height: min(itemRows, bottom-top)})
	}
	return spans
}

// Select selects the entry with ID id and reports whether there is one.
func (m *Manager) Select(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, n := range m.history {
		if n.ID == id {
			m.selected = i
			return true
		}
	}
	return false
}

// entryPrefix is the focus marker column of a notification entry.
func entryPrefix(kit ui.Kit, focused bool) string {
	if focused {
		return kit.Theme().Focus.Marker + " "
	}
	return "  "
}

// seenMarker marks an unread notification; read ones keep the column blank.
func seenMarker(kit ui.Kit, n Notification) string {
	if n.Seen {
		return "  "
	}
	sym := "● "
	if kit.ASCIIChrome() || kit.Mode() == ui.Plain {
		sym = "* "
	}
	return kit.StatusText(sym, theme.Success)
}

func (m *Manager) unseenCountLocked() int {
	count := 0
	for _, n := range m.history {
		if !n.Seen {
			count++
		}
	}
	return count
}
