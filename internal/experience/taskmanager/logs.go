package taskmanager

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"bloomfx.dev/bloom"
	"gostalgia/internal/experience/ui"
)

// LogTailLines is how many lines of a process's log the Task Manager asks
// proc/logs for, and the most a process's log buffer keeps.
const LogTailLines = 100

// ViewportID identifies a Task Manager viewport in a solved bloom frame.
type ViewportID string

// LogsViewport is the log tail's viewport.
const LogsViewport ViewportID = "logs"

// compactLogsHeight is the smallest height at which the bordered log tail
// shows a log line under its heading and help. Below it the tail drops its
// chrome so the log text itself stays on screen.
const compactLogsHeight = 7

// processLog is the log tail kept for one opened process: its sanitized
// lines, where the reader is in them, and their search.
type processLog struct {
	buf *bloom.Buffer
	// partial reports that the last line had no newline yet, so the next
	// snapshot may hold it grown.
	partial bool
	// pos is bloom.ViewportEnd exactly while the tail follows new output.
	pos bloom.ViewportPos

	query   string
	re      *regexp.Regexp
	match   bloom.Match
	matched bool
}

func newProcessLog() *processLog {
	return &processLog{buf: bloom.NewBuffer("")}
}

func (l *processLog) following() bool { return l.pos.AtEnd() }

// lines is the buffer's text, a line each.
func (l *processLog) lines() []string {
	out := make([]string, l.buf.Len())
	for i := range out {
		out[i] = l.buf.Line(i)
	}
	return out
}

// reconcile brings the buffer to a proc/logs snapshot of the log's tail.
//
// A snapshot that continues the buffer, its first lines the same as the
// buffer's last, appends only the lines after them, so nothing already
// read moves; an identical snapshot changes nothing, and one that is a
// shorter run of the buffer's last lines keeps the buffer as it is. A last
// line without a newline is taken as still being written, and the next
// snapshot may replace it. Any other snapshot, such as a log truncated or
// rotated since, replaces the buffer, keeping the reader's line number
// where the new lines reach it. Past LogTailLines the oldest lines are
// dropped, as in the transcript.
//
// Snapshots carry no offsets, so a log that repeats the same lines can
// read as unchanged when it has grown by more of them.
func (l *processLog) reconcile(content string) {
	lines := strings.Split(ui.Sanitize(content), "\n")
	partial := lines[len(lines)-1] != ""
	if !partial {
		lines = lines[:len(lines)-1]
	}
	old := l.lines()
	settled := len(old)
	if l.partial {
		settled--
	}
	k := overlap(old[:settled], lines)
	if k == 0 && settled > 0 {
		l.buf.SetLines(lines...)
		l.partial, l.matched = partial, false
		if !l.pos.AtEnd() {
			l.pos = bloom.ViewportPos{Line: min(l.pos.Line, max(0, len(lines)-1)), X: l.pos.X}
		}
		l.evict()
		return
	}
	fresh := lines[k:]
	if l.partial && !slices.Equal(fresh, old[settled:]) {
		l.truncate(settled)
	}
	if l.buf.Len() == settled {
		l.buf.Append(fresh...)
	}
	l.partial = partial
	l.evict()
}

// truncate drops the lines from line n on.
func (l *processLog) truncate(n int) {
	if n == 0 {
		l.buf.Clear()
	} else {
		l.buf.Delete(bloom.Pos{Line: n - 1, Col: math.MaxInt}, l.buf.End())
	}
	if l.matched && l.match.Line >= n {
		l.matched = false
	}
}

// evict drops the oldest lines past LogTailLines. The line at the top stays
// there; one that was dropped leaves the oldest line kept there instead.
func (l *processLog) evict() {
	over := l.buf.Len() - LogTailLines
	if over <= 0 {
		return
	}
	l.buf.Delete(bloom.Pos{}, bloom.Pos{Line: over})
	if !l.pos.AtEnd() {
		if l.pos.Line -= over; l.pos.Line < 0 {
			l.pos.Line, l.pos.Row = 0, 0
		}
	}
	if l.match.Line -= over; l.match.Line < 0 {
		l.matched = false
	}
}

// overlap is the most lines that end old and start lines alike.
func overlap(old, lines []string) int {
	for k := min(len(old), len(lines)); k > 0; k-- {
		if slices.Equal(old[len(old)-k:], lines[:k]) {
			return k
		}
	}
	return 0
}

// setQuery sets the search, matching q's text in any case.
func (l *processLog) setQuery(q string) {
	l.query, l.matched = q, false
	l.re = nil
	if q != "" {
		l.re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(q))
	}
}

// seek moves to the next match after the current one, or the previous
// one before it, wrapping at the ends, from line top when there is none
// yet, and scrolls it into view.
func (l *processLog) seek(v bloom.ViewportView, forward bool) {
	if l.re == nil {
		return
	}
	var (
		m  bloom.Match
		ok bool
	)
	switch {
	case forward && l.matched:
		if m, ok = l.buf.Find(l.re, l.match.Line, l.match.End); !ok {
			m, ok = l.buf.Find(l.re, 0, 0)
		}
	case forward:
		if m, ok = l.buf.Find(l.re, v.First, 0); !ok {
			m, ok = l.buf.Find(l.re, 0, 0)
		}
	case l.matched:
		if m, ok = l.buf.FindBefore(l.re, l.match.Line, l.match.Start); !ok {
			m, ok = l.buf.FindBefore(l.re, l.buf.Len()-1, math.MaxInt)
		}
	default:
		if m, ok = l.buf.FindBefore(l.re, v.First, 0); !ok {
			m, ok = l.buf.FindBefore(l.re, l.buf.Len()-1, math.MaxInt)
		}
	}
	l.match, l.matched = m, ok
	if ok && v.Lines > 0 {
		l.pos = v.Reveal(m.Line)
	}
}

// matchStatus counts the matches and says which is current.
func (l *processLog) matchStatus() string {
	if l.re == nil {
		return ""
	}
	total, current := 0, 0
	for i := range l.buf.Len() {
		for _, loc := range l.re.FindAllStringIndex(l.buf.Text(i), -1) {
			total++
			if l.matched && i == l.match.Line && loc[0] == l.match.Start {
				current = total
			}
		}
	}
	switch {
	case total == 0:
		return fmt.Sprintf("No match for %q", l.query)
	case current == 0:
		return fmt.Sprintf("%d matches for %q · n next · N previous", total, l.query)
	}
	return fmt.Sprintf("Match %d of %d for %q · n next · N previous", current, total, l.query)
}

// ShowLogs opens the log tail of a process with a proc/logs snapshot of
// it. A process opened before keeps its lines, position and search.
func (m *Model) ShowLogs(pid int32, name, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mode = ModeLogs
	m.logsPID = pid
	m.logsName = ui.Sanitize(name)
	m.searching = false
	m.errorMsg = ""
	l, ok := m.logs[pid]
	if !ok {
		l = newProcessLog()
		if m.logs == nil {
			m.logs = make(map[int32]*processLog)
		}
		m.logs[pid] = l
	}
	l.reconcile(content)
}

// UpdateLogs brings an opened process's log tail to a newer snapshot. It
// reports false for a process whose logs were never opened, or were
// dropped, and changes nothing then.
func (m *Model) UpdateLogs(pid int32, content string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.logs[pid]
	if ok {
		l.reconcile(content)
	}
	return ok
}

// LogsPID reports the process whose log tail is open.
func (m *Model) LogsPID() (int32, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.logsPID, m.mode == ModeLogs
}

// openLog is the open log tail, or nil.
func (m *Model) openLog() *processLog {
	if m.mode != ModeLogs {
		return nil
	}
	return m.logs[m.logsPID]
}

// ScrollLogs scrolls the open log tail to where move takes it from where f,
// a frame solved with it, showed it. A tail that is not following stays
// where it is scrolled to, its end included, until ToggleFollow.
func (m *Model) ScrollLogs(f *bloom.Frame, move func(bloom.ViewportView) bloom.ViewportPos) {
	v, ok := f.Viewport(LogsViewport)
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.openLog(); l != nil && ok {
		l.pos = move(v)
	}
}

// LogsPos is where the open log tail is scrolled to.
func (m *Model) LogsPos() bloom.ViewportPos {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if l := m.openLog(); l != nil {
		return l.pos
	}
	return bloom.ViewportPos{}
}

// Following reports whether the open log tail follows new output.
func (m *Model) Following() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l := m.openLog()
	return l != nil && l.following()
}

// ToggleFollow starts the open log tail following new output from its end,
// or stops it where f, a frame solved with it, shows it.
func (m *Model) ToggleFollow(f *bloom.Frame) {
	v, _ := f.Viewport(LogsViewport)
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.openLog()
	switch {
	case l == nil:
	case l.following():
		l.pos = bloom.ViewportPos{Line: v.First, X: v.Pos.X}
	default:
		l.pos = bloom.ViewportEnd
	}
}

// StartSearch starts typing a search of the open log tail.
func (m *Model) StartSearch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.openLog(); l != nil {
		m.searching, m.searchDraft = true, l.query
	}
}

// Searching reports whether a search is being typed; it takes every key
// until it is committed or cancelled.
func (m *Model) Searching() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.searching
}

// SearchInput types sanitized text into the search.
func (m *Model) SearchInput(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.searching {
		m.searchDraft += strings.ReplaceAll(ui.Sanitize(text), "\n", " ")
	}
}

// SearchBackspace deletes the last character typed into the search.
func (m *Model) SearchBackspace() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if clusters := ui.GraphemeClusters(m.searchDraft); m.searching && len(clusters) > 0 {
		m.searchDraft = strings.Join(clusters[:len(clusters)-1], "")
	}
}

// CancelSearch stops typing a search, keeping the one before it.
func (m *Model) CancelSearch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.searching, m.searchDraft = false, ""
}

// CommitSearch searches the open log tail for what was typed and moves to
// the first match from the top of the tail as f, a frame solved with it,
// shows it. An empty search clears the highlights.
func (m *Model) CommitSearch(f *bloom.Frame) {
	v, _ := f.Viewport(LogsViewport)
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.openLog()
	if l == nil || !m.searching {
		return
	}
	m.searching = false
	l.setQuery(m.searchDraft)
	m.searchDraft = ""
	l.seek(v, true)
}

// SeekMatch moves to the next match of the open log tail's search, or the
// previous one, and scrolls it into view of the tail as f, a frame solved
// with it, shows it.
func (m *Model) SeekMatch(f *bloom.Frame, forward bool) {
	v, _ := f.Viewport(LogsViewport)
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.openLog(); l != nil && !m.searching {
		l.seek(v, forward)
	}
}

// LogsNode is the open log tail as a bloom node sized by its slot: a
// bordered panel, or below compactLogsHeight rows just a heading over the
// log text.
func (m *Model) LogsNode(kit ui.Kit) bloom.Child {
	return ui.SizedNode(func(ctx bloom.Context) bloom.Child {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.logsNode(kit, ui.Bounds{Width: ctx.Width, Height: ctx.Height})
	})
}

// logsNode is LogsNode in bounds, read under m.mu.
func (m *Model) logsNode(kit ui.Kit, bounds ui.Bounds) bloom.Child {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return nil
	}
	l := m.logs[m.logsPID]
	if l == nil {
		l = newProcessLog()
	}
	follow := ""
	if l.following() {
		follow = " · FOLLOW"
	}
	status := l.matchStatus()
	if m.searching {
		status = "Search: " + m.searchDraft + "_  Enter find · Esc cancel"
	}
	line := func(text func(string) string, s string) bloom.Child {
		return bloom.Fixed(1, kit.Leaf(func(b ui.Bounds) string { return text(ui.Truncate(s, b.Width)) }))
	}
	var rows []bloom.Child
	if bounds.Height < compactLogsHeight {
		switch {
		case bounds.Height >= 3 && status != "":
			rows = append(rows, line(kit.Heading, fmt.Sprintf("LOGS · PID %d · %s%s", m.logsPID, m.logsName, follow)), line(kit.Muted, status))
		case bounds.Height >= 2 && status != "":
			rows = append(rows, line(kit.Heading, status))
		case bounds.Height >= 2:
			rows = append(rows, line(kit.Heading, fmt.Sprintf("LOGS · PID %d · %s%s", m.logsPID, m.logsName, follow)))
		}
		return bloom.Column(append(rows, bloom.Fill(m.logText(kit, l)))...)
	}
	help := status
	if help == "" {
		help = "Esc back · ↑↓ PgUp PgDn scroll · f follow · / search"
	}
	rows = append(rows,
		line(kit.Heading, fmt.Sprintf("CHILD LOG TAIL: %s (PID %d)%s", m.logsName, m.logsPID, follow)),
		line(kit.Muted, help),
		bloom.Fixed(1, nil),
		bloom.Fill(m.logText(kit, l)),
	)
	return kit.PanelNode(ui.Panel{Title: fmt.Sprintf("LOGS · PID %d", m.logsPID), Focused: true}, bloom.Column(rows...))
}

// logText is the log's lines in a viewport, the current match's line
// marked in the gutter, or a notice while there are none.
func (m *Model) logText(kit ui.Kit, l *processLog) bloom.Child {
	if l.buf.Len() == 0 {
		return kit.Leaf(func(b ui.Bounds) string { return kit.Muted(ui.Truncate("(no output captured yet)", b.Width)) })
	}
	marker := kit.Theme().Focus.Marker + " "
	current, matched := l.match.Line, l.matched
	vp := bloom.Viewport(l.buf).At(l.pos).Style(kit.TextStyle()).ID(LogsViewport).
		Gutter(markerWidth, func(row bloom.GutterRow) string {
			if matched && row.Line == current && !row.Continued {
				return kit.Text(marker)
			}
			return kit.Text("  ")
		})
	if l.following() {
		// Only while following: scrolling back to the end of a tail that
		// does not follow leaves it where it is, until f.
		vp = vp.Follow()
	}
	if l.re != nil {
		vp = vp.Marks(bloom.MarkMatches(l.re, kit.SelectionStyle()))
	}
	return vp
}
