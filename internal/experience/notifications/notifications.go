package notifications

import (
	"fmt"
	"strings"
	"sync"
	"time"

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
}

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
	n.Seen = false // Explicitly unread / unseen upon arrival

	// Prepend to history (newest first)
	m.history = append([]Notification{n}, m.history...)
	if len(m.history) > m.maxHistory {
		m.history = m.history[:m.maxHistory]
	}

	// Adjust selection if needed
	if m.selected >= len(m.history) {
		m.selected = max(0, len(m.history)-1)
	}

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
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.toasts) == 0 || m.dnd || width <= 0 {
		return ""
	}

	var rows []string
	for _, t := range m.toasts {
		st := theme.Normal
		badgeText := "INFO"
		switch t.Level {
		case LevelError:
			st = theme.Error
			badgeText = "CRASH"
		case LevelWarning:
			st = theme.Busy
			badgeText = "BACKOFF"
		}

		badge := kit.Badge(badgeText, st, width)
		title := kit.Heading(t.Title)
		msg := kit.Text(t.Message)

		line := fmt.Sprintf("%s %s · %s", badge, title, msg)
		rows = append(rows, ui.Truncate(line, width))
	}

	if m.droppedToasts > 0 {
		notice := kit.Muted(fmt.Sprintf("(+%d alerts suppressed by flood bounding)", m.droppedToasts))
		rows = append(rows, ui.Truncate(notice, width))
	}

	return strings.Join(rows, "\n")
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
	header := fmt.Sprintf("NOTIFICATIONS: %d total · %d unread · %s", len(m.history), m.unseenCountLocked(), dndStatus)

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

	availableRows := max(1, bodyHeight-2)
	itemHeight := 2 // Each notification takes 2 lines (header + message)
	visibleItems := max(1, availableRows/itemHeight)

	start := max(0, min(m.selected-visibleItems+1, max(0, len(m.history)-visibleItems)))
	end := min(len(m.history), start+visibleItems)

	var lines []string
	lines = append(lines, kit.Muted(ui.Truncate(header, w)), "")

	for i := start; i < end; i++ {
		n := m.history[i]
		focused := i == m.selected

		badgeState := theme.Normal
		badgeLabel := "INFO"
		switch n.Level {
		case LevelError:
			badgeState = theme.Error
			badgeLabel = "CRASH"
		case LevelWarning:
			badgeState = theme.Busy
			badgeLabel = "BACKOFF"
		}

		timeStr := n.Timestamp.Format("15:04:05")
		badge := kit.Badge(badgeLabel, badgeState, w)
		titleText := n.Title
		if n.Receipt != nil {
			titleText += " [RECEIPT AVAILABLE]"
		}

		headerLine := fmt.Sprintf("%s%s%s [%s] %s", entryPrefix(kit, focused), seenMarker(kit, n), badge, timeStr, titleText)
		detailLine := fmt.Sprintf("    %s", n.Message)

		if focused {
			headerLine = kit.Selection(ui.Truncate(headerLine, w))
			detailLine = kit.Text(ui.Truncate(detailLine, w))
		} else {
			headerLine = kit.Heading(ui.Truncate(headerLine, w))
			detailLine = kit.Muted(ui.Truncate(detailLine, w))
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
// header row must leave room for after its markers, badge and timestamp.
const minTitleCells = 12

// compactPanel reports whether bounds are too short for one whole entry or
// too narrow for its title in the bordered panel. The panel then drops its
// chrome, badge and timestamp so the selected entry stays readable.
func compactPanel(kit ui.Kit, bounds ui.Bounds) bool {
	if bounds.Height < compactPanelHeight {
		return true
	}
	w := kit.PanelContentBounds(bounds).Width
	header := ui.GraphemeWidth(entryPrefix(kit, true)) + len("* ") + ui.GraphemeWidth(kit.Badge("BACKOFF", theme.Busy, w)) + len(" [15:04:05] ")
	return w-header < minTitleCells
}

// renderCompactPanel is the Notification Center for a body too short for
// its panel: a one-line summary when there is room, then the entries
// around the selection, each as its title and message.
func (m *Manager) renderCompactPanel(kit ui.Kit, bounds ui.Bounds) string {
	w, rows := bounds.Width, bounds.Height
	dndStatus := "DND OFF"
	if m.dnd {
		dndStatus = "DND ON"
	}
	if len(m.history) == 0 {
		return ui.Fit(kit.Muted(ui.Truncate("No notifications · "+dndStatus, w)), bounds)
	}
	var lines []string
	if rows >= 3 {
		summary := fmt.Sprintf("%d/%d · %d unread · %s", m.selected+1, len(m.history), m.unseenCountLocked(), dndStatus)
		lines = append(lines, kit.Muted(ui.Truncate(summary, w)))
		rows--
	}
	visibleItems := max(1, rows/2)
	start := max(0, min(m.selected-visibleItems+1, max(0, len(m.history)-visibleItems)))
	end := min(len(m.history), start+visibleItems)
	for i := start; i < end; i++ {
		n := m.history[i]
		titleText := n.Title
		if n.Receipt != nil {
			titleText += " [RECEIPT]"
		}
		headerLine := ui.Truncate(entryPrefix(kit, i == m.selected)+seenMarker(kit, n)+titleText, w)
		detailLine := ui.Truncate("    "+n.Message, w)
		if i == m.selected {
			lines = append(lines, kit.Selection(headerLine), kit.Text(detailLine))
		} else {
			lines = append(lines, kit.Heading(headerLine), kit.Muted(detailLine))
		}
	}
	return ui.Fit(strings.Join(lines, "\n"), bounds)
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
	if kit.Theme().Name == "monochrome" || kit.Mode() == ui.Plain {
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
