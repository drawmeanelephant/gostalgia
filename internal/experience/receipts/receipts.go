package receipts

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

const (
	DefaultStoreCapacity = 50
	MaxExcerptLines      = 25
	MaxExcerptBytes      = 4096
)

var receiptSeq uint64

// CrashReceipt contains diagnostic evidence when a process fails.
type CrashReceipt struct {
	ID         string    `json:"id"`
	PID        int32     `json:"pid"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	ExitCode   int       `json:"exit_code"`
	Timestamp  time.Time `json:"timestamp"`
	Reason     string    `json:"reason"`
	LogExcerpt string    `json:"log_excerpt"`
	Truncated  bool      `json:"truncated"`
}

// New creates a new CrashReceipt with bounded, sanitized diagnostic excerpt.
func New(pid int32, name, state string, exitCode int, reason string, rawLogs string) CrashReceipt {
	now := time.Now()
	seq := atomic.AddUint64(&receiptSeq, 1)
	id := fmt.Sprintf("CR-%d-%d-%d", pid, now.UnixNano(), seq)

	excerpt, truncated := boundExcerpt(rawLogs, MaxExcerptLines, MaxExcerptBytes)
	return CrashReceipt{
		ID:         id,
		PID:        pid,
		Name:       ui.Sanitize(name),
		State:      ui.Sanitize(state),
		ExitCode:   exitCode,
		Timestamp:  now,
		Reason:     ui.Sanitize(reason),
		LogExcerpt: ui.Sanitize(excerpt),
		Truncated:  truncated,
	}
}

func boundExcerpt(s string, maxLines, maxBytes int) (string, bool) {
	if s == "" {
		return "(no logs captured)", false
	}
	truncated := false
	if len(s) > maxBytes {
		s = s[len(s)-maxBytes:]
		truncated = true
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	return strings.Join(lines, "\n"), truncated
}

// Store holds a bounded list of recent crash receipts.
type Store struct {
	mu       sync.RWMutex
	capacity int
	receipts []CrashReceipt
}

// NewStore creates a bounded crash receipt store.
func NewStore(capacity int) *Store {
	if capacity <= 0 {
		capacity = DefaultStoreCapacity
	}
	return &Store{
		capacity: capacity,
		receipts: make([]CrashReceipt, 0, capacity),
	}
}

// Add appends a receipt, evicting the oldest if capacity is reached.
func (s *Store) Add(r CrashReceipt) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Prepend newest receipt
	s.receipts = append([]CrashReceipt{r}, s.receipts...)
	if len(s.receipts) > s.capacity {
		s.receipts = s.receipts[:s.capacity]
	}
}

// List returns all receipts in reverse-chronological order (newest first).
func (s *Store) List() []CrashReceipt {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CrashReceipt, len(s.receipts))
	copy(out, s.receipts)
	return out
}

// Get returns a receipt by its unique ID.
func (s *Store) Get(id string) (CrashReceipt, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.receipts {
		if r.ID == id {
			return r, true
		}
	}
	return CrashReceipt{}, false
}

// GetByPID returns the most recent receipt for a process ID.
func (s *Store) GetByPID(pid int32) (CrashReceipt, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.receipts {
		if r.PID == pid {
			return r, true
		}
	}
	return CrashReceipt{}, false
}

// UpdateExcerpt updates log excerpt for an existing receipt.
func (s *Store) UpdateExcerpt(id, rawLogs string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.receipts {
		if s.receipts[i].ID == id {
			excerpt, truncated := boundExcerpt(rawLogs, MaxExcerptLines, MaxExcerptBytes)
			s.receipts[i].LogExcerpt = ui.Sanitize(excerpt)
			s.receipts[i].Truncated = truncated
			return true
		}
	}
	return false
}

// Count returns the number of receipts stored.
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.receipts)
}

// Clear removes all receipts from the store.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = s.receipts[:0]
}

// Render formats a crash receipt inside bounded terminal space using the component kit.
func Render(kit ui.Kit, r CrashReceipt, bounds ui.Bounds) string {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}
	contentBounds := kit.PanelContentBounds(bounds)
	w := contentBounds.Width
	heading := fmt.Sprintf("CRASH RECEIPT: %s (PID %d)", r.Name, r.PID)
	// A heading cut before the PID loses the receipt's identity, so a
	// narrow receipt is compact however tall it is.
	if contentBounds.Height < identityRows(r) || ui.GraphemeWidth(heading) > w {
		return renderCompact(kit, r, bounds)
	}

	timeStr := r.Timestamp.Format("2006-01-02 15:04:05")
	var headerLines []string
	headerLines = append(headerLines, kit.Heading(heading))
	separator := kit.Separator()
	headerLines = append(headerLines, kit.Muted(fmt.Sprintf("Time: %s%sExit code: %d%sState: %s", timeStr, separator, r.ExitCode, separator, r.State)))
	if r.Reason != "" {
		headerLines = append(headerLines, kit.StatusText("Reason: "+r.Reason, theme.Error))
	}
	headerLines = append(headerLines, "")
	headerLines = append(headerLines, kit.Heading("DIAGNOSTIC LOG EXCERPT:"))

	headerText := strings.Join(headerLines, "\n")
	headerHeight := len(strings.Split(headerText, "\n"))
	logRoom := max(1, contentBounds.Height-headerHeight-1)

	excerptLines := strings.Split(r.LogExcerpt, "\n")
	if len(excerptLines) > logRoom {
		excerptLines = excerptLines[len(excerptLines)-logRoom:]
	}

	var formattedLogs []string
	for _, l := range excerptLines {
		formattedLogs = append(formattedLogs, kit.Text("  "+kit.Truncate(l, max(1, w-2))))
	}
	if r.Truncated {
		formattedLogs = append(formattedLogs, kit.Muted("  [... prior output truncated ...]"))
	}

	body := headerText + "\n" + strings.Join(formattedLogs, "\n")
	return kit.Panel(ui.Panel{
		Title:   fmt.Sprintf("CRASH RECEIPT #%s", r.ID),
		Body:    body,
		State:   theme.Error,
		Focused: true,
	}, bounds)
}

// identityRows is how many rows the bordered receipt needs to show who
// crashed and why: its heading, time line and reason when present.
func identityRows(r CrashReceipt) int {
	n := 2
	if r.Reason != "" {
		n++
	}
	return n
}

// renderCompact is the receipt for a body too short for its panel: the
// process and PID, then the reason, then the exit status, receipt ID and
// the end of the log excerpt as far as room allows.
func renderCompact(kit ui.Kit, r CrashReceipt, bounds ui.Bounds) string {
	w := bounds.Width
	separator := kit.Separator()
	status := fmt.Sprintf("Exit code: %d%sState: %s", r.ExitCode, separator, r.State)
	lines := []string{kit.Heading(kit.Truncate(fmt.Sprintf("%s (PID %d)%sCRASH RECEIPT", r.Name, r.PID, separator), w))}
	if r.Reason != "" {
		lines = append(lines, kit.StatusText(kit.Truncate("Reason: "+r.Reason, w), theme.Error))
	}
	lines = append(lines,
		kit.Muted(kit.Truncate(status, w)),
		kit.Muted(kit.Truncate(fmt.Sprintf("#%s%s%s", r.ID, separator, r.Timestamp.Format("2006-01-02 15:04:05")), w)))
	if room := bounds.Height - len(lines); room > 0 {
		excerpt := strings.Split(r.LogExcerpt, "\n")
		for _, l := range excerpt[max(0, len(excerpt)-room):] {
			lines = append(lines, kit.Text(kit.Truncate("  "+l, w)))
		}
	}
	return ui.Fit(strings.Join(lines, "\n"), bounds)
}

// FormatText formats a crash receipt as human-readable plain text lines.
func FormatText(r CrashReceipt) string {
	timeStr := r.Timestamp.Format("2006-01-02 15:04:05")
	var lines []string
	lines = append(lines, fmt.Sprintf("CRASH RECEIPT: %s (PID %d) · Exit code: %d · State: %s", r.Name, r.PID, r.ExitCode, r.State))
	lines = append(lines, fmt.Sprintf("Time: %s", timeStr))
	if r.Reason != "" {
		lines = append(lines, "Reason: "+r.Reason)
	}
	lines = append(lines, "--- Log Excerpt ---")
	if r.LogExcerpt != "" {
		lines = append(lines, r.LogExcerpt)
	} else {
		lines = append(lines, "(no logs captured)")
	}
	if r.Truncated {
		lines = append(lines, "[... prior output truncated ...]")
	}
	return strings.Join(lines, "\n")
}
