package taskmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
			ID:    1,
			Name:  "shell-init",
			State: "running",
			Resources: platform.ResourceUsage{
				Supported:   true,
				CPUUserMs:   50,
				CPUSysMs:    10,
				MemoryBytes: 4 * 1024 * 1024,
			},
		},
		{
			ID:        2,
			Name:      "flaky-worker",
			State:     "failed",
			ExitCode:  1,
			Resources: platform.ResourceUsage{Supported: false},
		},
		{
			ID:        3,
			Name:      "broken-app",
			State:     "stopping",
			Resources: platform.ResourceUsage{Supported: false},
		},
		{
			ID:        4,
			Name:      "exited-job",
			State:     "stopped",
			ExitCode:  0,
			Resources: platform.ResourceUsage{Supported: false},
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
	for _, header := range []string{"PID", "NAME", "STATE", "CPU", "MEMORY"} {
		if !strings.Contains(view, header) {
			t.Fatalf("missing table header %q", header)
		}
	}

	// Verify state formatting
	if !strings.Contains(view, "running") {
		t.Fatal("missing running state in view")
	}
	if !strings.Contains(view, "failed") {
		t.Fatal("missing failed state in view")
	}
	if !strings.Contains(view, "stopping") {
		t.Fatal("missing stopping state in view")
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

	t.Run("refresh reconciles", testLogRefreshReconciles)
	t.Run("a tail per process", testLogTailsPerProcess)
	t.Run("follow", testLogFollow)
	t.Run("match across refresh", testLogMatchAcrossRefresh)
	t.Run("search belongs to the tail", testLogSearchLeavesWithTail)
	for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256} {
		t.Run(fmt.Sprintf("search/%d", mode), func(t *testing.T) { testLogSearch(t, mode) })
	}
}

// numberedLog is a log of lines prefix 001 to prefix last, from first.
func numberedLog(prefix string, first, last int) string {
	var b strings.Builder
	for i := first; i <= last; i++ {
		fmt.Fprintf(&b, "%s %03d\n", prefix, i)
	}
	return b.String()
}

// The log size the tests view the tail at: a bordered panel with 15 rows
// of log text.
const logW, logH = 60, 20

// logScreen is the open log tail drawn at logW\u00D7logH.
func logScreen(tm *Model, kit ui.Kit) string {
	return bloom.Render(logW, logH, tm.LogsNode(kit, func() bool { return true }))
}

func logFrame(tm *Model, kit ui.Kit) *bloom.Frame {
	return bloom.Solve(logW, logH, tm.LogsNode(kit, func() bool { return true }))
}

// logTop is the line at the top of the open log tail.
func logTop(t *testing.T, tm *Model, kit ui.Kit) int {
	t.Helper()
	v, ok := logFrame(tm, kit).Viewport(LogsViewport)
	if !ok {
		t.Fatal("no log viewport on screen")
	}
	return v.First
}

// logText is the open log tail's lines.
func logText(tm *Model) []string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.openLog().lines()
}

func scrollLogTo(tm *Model, kit ui.Kit, row int) {
	tm.ScrollLogs(logFrame(tm, kit), func(v bloom.ViewportView) bloom.ViewportPos { return v.AtRow(row) })
}

// checkLog fails unless the open tail holds exactly want, a line each.
func checkLog(t *testing.T, tm *Model, want string) {
	t.Helper()
	if got := strings.Join(logText(tm), "\n") + "\n"; got != want {
		t.Fatalf("log holds\n%s\nwant\n%s", got, want)
	}
}

// Snapshots that repeat, extend, overlap, shorten or replace the tail
// reconcile without duplicates, keeping the line read at the top while it
// is kept, and clamping to the lines kept when it is not.
func testLogRefreshReconciles(t *testing.T) {
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	tm := New()
	tm.ShowLogs(5, "daemon", numberedLog("a", 1, 40))
	scrollLogTo(tm, kit, 10)
	if top := logTop(t, tm, kit); top != 10 || !strings.Contains(logScreen(tm, kit), "a 011") {
		t.Fatalf("scrolled to line %d", top)
	}
	for _, step := range []struct {
		name, snapshot, want string
		top                  int
	}{
		{"identical", numberedLog("a", 1, 40), numberedLog("a", 1, 40), 10},
		{"overlapping", numberedLog("a", 21, 60), numberedLog("a", 1, 60), 10},
		{"shorter run of the end", numberedLog("a", 50, 60), numberedLog("a", 1, 60), 10},
		{"grown past the bound", numberedLog("a", 1, 130), numberedLog("a", 31, 130), 0},
	} {
		if !tm.UpdateLogs(5, step.snapshot) {
			t.Fatalf("%s: the opened process has no tail", step.name)
		}
		checkLog(t, tm, step.want)
		if top := logTop(t, tm, kit); top != step.top {
			t.Fatalf("%s: line %d is at the top, want %d", step.name, top, step.top)
		}
	}
	if n := len(logText(tm)); n != LogTailLines {
		t.Fatalf("kept %d lines, want the %d proc/logs is asked for", n, LogTailLines)
	}
	scrollLogTo(tm, kit, 50)
	tm.UpdateLogs(5, numberedLog("a", 41, 140))
	checkLog(t, tm, numberedLog("a", 41, 140))
	if top := logTop(t, tm, kit); logText(tm)[top] != "a 081" {
		t.Fatalf("eviction moved the top from a 081 to %s", logText(tm)[top])
	}

	tm.UpdateLogs(5, "rotated 1\nrotated 2\nrotated 3\n")
	checkLog(t, tm, "rotated 1\nrotated 2\nrotated 3\n")
	if screen := logScreen(tm, kit); !strings.Contains(screen, "rotated 1") || !strings.Contains(screen, "rotated 3") || strings.Contains(screen, "a 0") {
		t.Fatalf("a rotated log did not replace the tail:\n%s", screen)
	}
	if top := logTop(t, tm, kit); top != 0 {
		t.Fatalf("a replaced tail left line %d at the top", top)
	}

	// A last line without a newline is still being written.
	tm.ShowLogs(6, "partial", "one\ntwo\nthr")
	for _, step := range []struct{ snapshot, want string }{
		{"one\ntwo\nthr", "one\ntwo\nthr\n"},
		{"one\ntwo\nthree\nfour\n", "one\ntwo\nthree\nfour\n"},
		{"two\nthree\nfour\nfi", "one\ntwo\nthree\nfour\nfi\n"},
		{"four\nfive\n", "one\ntwo\nthree\nfour\nfive\n"},
		{"four\nfive\n", "one\ntwo\nthree\nfour\nfive\n"},
	} {
		tm.UpdateLogs(6, step.snapshot)
		checkLog(t, tm, step.want)
	}
}

// Each opened process keeps a tail of its own: another's lines and
// position never mix in, a refresh of one leaves the other's screen as it
// is, and reopening a process restores its tail where it was read until
// it leaves the table.
func testLogTailsPerProcess(t *testing.T) {
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	tm := New()
	tm.SetProcesses([]ProcessRow{{ID: 5, Name: "five"}, {ID: 9, Name: "nine"}})
	tm.ShowLogs(5, "five", numberedLog("five", 1, 50))
	scrollLogTo(tm, kit, 20)

	tm.ShowLogs(9, "nine", numberedLog("nine", 1, 30))
	screen := logScreen(tm, kit)
	if pid, open := tm.LogsPID(); pid != 9 || !open || strings.Contains(screen, "five") || !strings.Contains(screen, "nine 001") {
		t.Fatalf("opening PID 9 shows:\n%s", screen)
	}
	if top := logTop(t, tm, kit); top != 0 {
		t.Fatalf("PID 9 opened at PID 5's line %d", top)
	}
	if !tm.UpdateLogs(5, numberedLog("five", 11, 60)) {
		t.Fatal("PID 5's tail was not kept")
	}
	if got := logScreen(tm, kit); got != screen {
		t.Fatalf("refreshing PID 5 changed PID 9's screen:\n%s", got)
	}
	if tm.UpdateLogs(77, "never opened\n") {
		t.Fatal("a process never opened took a refresh")
	}

	tm.CloseModal()
	tm.ShowLogs(5, "five", numberedLog("five", 11, 60))
	checkLog(t, tm, numberedLog("five", 1, 60))
	if top := logTop(t, tm, kit); top != 20 {
		t.Fatalf("reopening PID 5 put line %d at the top, want 20", top)
	}

	// PID 9 leaves the table, and its tail goes with it.
	tm.SetProcesses([]ProcessRow{{ID: 5, Name: "five"}})
	if tm.UpdateLogs(9, numberedLog("nine", 1, 31)) {
		t.Fatal("a process gone from the table kept its tail")
	}
	if !tm.UpdateLogs(5, numberedLog("five", 11, 60)) {
		t.Fatal("the open tail was dropped")
	}
}

// Follow shows each refresh's new lines at the bottom until the tail is
// scrolled or f stops it; then refreshes leave the line read in place.
func testLogFollow(t *testing.T) {
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	tm := New()
	tm.ShowLogs(3, "svc", numberedLog("l", 1, 40))
	if tm.Following() || !strings.Contains(logScreen(tm, kit), "l 001") {
		t.Fatal("a tail opens following, or not at its first line")
	}
	tm.ToggleFollow(logFrame(tm, kit))
	screen := logScreen(tm, kit)
	if !tm.Following() || !strings.Contains(screen, "l 040") || !strings.Contains(screen, "FOLLOW") {
		t.Fatalf("f did not follow:\n%s", screen)
	}
	tm.UpdateLogs(3, numberedLog("l", 1, 45))
	if screen := logScreen(tm, kit); !strings.Contains(screen, "l 045") {
		t.Fatalf("a followed refresh is not on screen:\n%s", screen)
	}

	tm.ScrollLogs(logFrame(tm, kit), func(v bloom.ViewportView) bloom.ViewportPos { return v.Up(1) })
	top := logTop(t, tm, kit)
	if tm.Following() {
		t.Fatal("scrolling up kept following")
	}
	tm.UpdateLogs(3, numberedLog("l", 1, 50))
	if got := logTop(t, tm, kit); got != top || strings.Contains(logScreen(tm, kit), "l 050") {
		t.Fatalf("a refresh moved an unfollowed tail from line %d to %d", top, got)
	}
	// Scrolling back to the end does not follow on its own.
	tm.ScrollLogs(logFrame(tm, kit), bloom.ViewportView.Bottom)
	if tm.Following() {
		t.Fatal("scrolling to the end followed without f")
	}
	tm.ToggleFollow(logFrame(tm, kit))
	tm.ToggleFollow(logFrame(tm, kit))
	top = logTop(t, tm, kit)
	tm.UpdateLogs(3, numberedLog("l", 1, 55))
	if tm.Following() || logTop(t, tm, kit) != top {
		t.Fatal("f did not stop following where the tail was")
	}
}

// The current match stays on its line as eviction drops lines above it,
// and is let go when its line is dropped, or when it was on an unfinished
// last line that a refresh rewrites.
func testLogMatchAcrossRefresh(t *testing.T) {
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	marker := kit.Theme().Focus.Marker + " "
	tm := New()
	search := func(q string) {
		tm.StartSearch()
		tm.SearchInput(q)
		tm.CommitSearch(logFrame(tm, kit))
	}
	screen := func() string { return ansi.Strip(logScreen(tm, kit)) }

	tm.ShowLogs(8, "evict", numberedLog("a", 1, 100))
	search("a 050")
	if s := screen(); !strings.Contains(s, "Match 1 of 1") || !strings.Contains(s, marker+"a 050") {
		t.Fatalf("the search shows:\n%s", s)
	}
	tm.UpdateLogs(8, numberedLog("a", 21, 120))
	checkLog(t, tm, numberedLog("a", 21, 120))
	if s := screen(); !strings.Contains(s, "Match 1 of 1") || !strings.Contains(s, marker+"a 050") {
		t.Fatalf("evicting the lines above the match lost it:\n%s", s)
	}
	tm.SeekMatch(logFrame(tm, kit), true)
	if s := screen(); !strings.Contains(s, marker+"a 050") {
		t.Fatalf("n after eviction left the match:\n%s", s)
	}
	tm.UpdateLogs(8, numberedLog("a", 61, 160))
	if s := screen(); !strings.Contains(s, `No match for "a 050"`) || strings.Contains(s, marker+"a ") {
		t.Fatalf("evicting the matched line kept the match:\n%s", s)
	}
	tm.SeekMatch(logFrame(tm, kit), false)
	checkLog(t, tm, numberedLog("a", 61, 160))

	tm.ShowLogs(9, "partial", "one\ntwo\nthr")
	search("thr")
	if s := screen(); !strings.Contains(s, "Match 1 of 1") || !strings.Contains(s, marker+"thr") {
		t.Fatalf("the unfinished line's match shows:\n%s", s)
	}
	tm.UpdateLogs(9, "one\ntwo\nthree\n")
	checkLog(t, tm, "one\ntwo\nthree\n")
	if s := screen(); !strings.Contains(s, `1 match for "thr"`) || strings.Contains(s, marker+"t") || strings.Contains(s, marker+"o") {
		t.Fatalf("rewriting the unfinished line kept its old match:\n%s", s)
	}
	tm.SeekMatch(logFrame(tm, kit), true)
	if s := screen(); !strings.Contains(s, "Match 1 of 1") || !strings.Contains(s, marker+"three") {
		t.Fatalf("n did not find the rewritten line:\n%s", s)
	}
}

// A search being typed belongs to the open tail: leaving it, for the table
// or a crash receipt, drops the search.
func testLogSearchLeavesWithTail(t *testing.T) {
	tm := New()
	tm.ShowLogs(3, "svc", "one\n")
	tm.StartSearch()
	tm.SearchInput("on")
	tm.ShowReceipt(&receipts.CrashReceipt{ID: "r1", PID: 3})
	if tm.Searching() {
		t.Fatal("a crash receipt kept the log search")
	}
	tm.CloseModal()
	tm.ShowLogs(3, "svc", "one\n")
	if tm.Searching() {
		t.Fatal("reopening the tail resumed an abandoned search")
	}
}

// Search marks every match in the theme's selection style, n and N move to
// the next and previous one, wrapping, and bring it into view; a refresh
// while typing, Unicode, no matches and an empty search keep the log and
// its viewport whole.
func testLogSearch(t *testing.T, mode ui.ColorMode) {
	kit := ui.New(theme.Nostalgia(), mode)
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		switch {
		case i == 3:
			b.WriteString("Alpha \u754c three\n")
		case i%10 == 0:
			fmt.Fprintf(&b, "alpha line %03d\n", i)
		default:
			fmt.Fprintf(&b, "beta line %03d\n", i)
		}
	}
	tm := New()
	tm.ShowLogs(4, "search", b.String())
	plain := logScreen(tm, kit)

	tm.StartSearch()
	tm.SearchInput("alp")
	tm.UpdateLogs(4, b.String()+"beta line 041\n")
	tm.SearchInput("hx")
	tm.SearchBackspace()
	if screen := logScreen(tm, kit); !tm.Searching() || !strings.Contains(ansi.Strip(screen), "Search: alph_") {
		t.Fatalf("typing the search shows:\n%s", ansi.Strip(screen))
	}
	tm.CommitSearch(logFrame(tm, kit))
	screen := logScreen(tm, kit)
	if tm.Searching() || !strings.Contains(ansi.Strip(screen), "Match 1 of 5") {
		t.Fatalf("the committed search shows:\n%s", ansi.Strip(screen))
	}
	marker := kit.Theme().Focus.Marker + " Alpha"
	if !strings.Contains(ansi.Strip(screen), marker) {
		t.Fatalf("the current match is not marked with %q:\n%s", marker, ansi.Strip(screen))
	}
	switch mode {
	case ui.Plain:
		if strings.Contains(screen, "\x1b") {
			t.Fatal("Plain search drew an escape sequence")
		}
	default:
		if screen == plain || strings.Contains(screen, "38;2;") || strings.Contains(screen, "48;2;") {
			t.Fatal("ANSI256 matches are not highlighted, or in truecolor")
		}
	}

	wantLines := []int{10, 20, 30, 40, 3}
	for i, line := range wantLines {
		tm.SeekMatch(logFrame(tm, kit), true)
		screen := ansi.Strip(logScreen(tm, kit))
		if !strings.Contains(screen, fmt.Sprintf("Match %d of 5", (i+1)%5+1)) {
			t.Fatalf("n %d shows:\n%s", i+1, screen)
		}
		want := fmt.Sprintf("alpha line %03d", line)
		if line == 3 {
			want = "Alpha \u754c three"
		}
		if !strings.Contains(screen, kit.Theme().Focus.Marker+" "+want) {
			t.Fatalf("n %d did not bring %q into view:\n%s", i+1, want, screen)
		}
	}
	tm.SeekMatch(logFrame(tm, kit), false)
	if screen := ansi.Strip(logScreen(tm, kit)); !strings.Contains(screen, "Match 5 of 5") {
		t.Fatalf("N before the first match did not wrap to the last:\n%s", screen)
	}

	for _, c := range []struct{ query, status string }{
		{"\u754c", "Match 1 of 1"},
		{"zzz", "No match for \"zzz\""},
		{"", "/ search"},
	} {
		tm.StartSearch()
		tm.CancelSearch()
		tm.StartSearch()
		for tm.mu.RLock(); tm.searchDraft != ""; tm.mu.RLock() {
			tm.mu.RUnlock()
			tm.SearchBackspace()
		}
		tm.mu.RUnlock()
		tm.SearchInput(c.query)
		tm.CommitSearch(logFrame(tm, kit))
		tm.SeekMatch(logFrame(tm, kit), true)
		tm.SeekMatch(logFrame(tm, kit), false)
		if screen := ansi.Strip(logScreen(tm, kit)); !strings.Contains(screen, c.status) {
			t.Fatalf("search %q shows:\n%s", c.query, screen)
		}
	}
	tm.mu.RLock()
	cleared := tm.openLog().re == nil && !tm.openLog().matched
	tm.mu.RUnlock()
	rows := strings.Split(ansi.Strip(logScreen(tm, kit)), "\n")
	if !cleared || strings.Contains(strings.Join(rows[2:], "\n"), kit.Theme().Focus.Marker) {
		t.Fatalf("an empty search left a match:\n%s", strings.Join(rows, "\n"))
	}

	// A refresh that replaces the line of the current match drops it.
	tm.StartSearch()
	tm.SearchInput("alpha")
	tm.CommitSearch(logFrame(tm, kit))
	tm.UpdateLogs(4, "rotated\nalpha again\n")
	tm.SeekMatch(logFrame(tm, kit), true)
	if screen := ansi.Strip(logScreen(tm, kit)); !strings.Contains(screen, "Match 1 of 1") || !strings.Contains(screen, "alpha again") {
		t.Fatalf("search after a replaced tail shows:\n%s", screen)
	}
}

func TestReceiptInspection(t *testing.T) {
	tm := New()
	receipt := receipts.New(8, "crasher", "failed", 137, "out of memory", "fatal: killed")
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
	if strings.Join(prev, " ") != "PID NAME STATE CPU MEMORY" {
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
	if strings.Join(dropped, " ") != "MEMORY CPU STATE" {
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
	if strings.Join(restored, " ") != "STATE CPU MEMORY" {
		t.Fatalf("restore order %v", restored)
	}
}

// A population without supported resource metrics has no CPU or MEMORY
// values to show, so those columns are omitted rather than filled with
// dashes, and their width widens NAME. The decision follows the displayed
// samples, not the platform name.
func TestUnsupportedMetricsDropColumnsAndWidenName(t *testing.T) {
	tm := New()
	tm.SetProcesses([]ProcessRow{
		{ID: 41, Name: "very-long-process-identity-name", State: "running"},
		{ID: 42, Name: "another-long-process-identity", State: "failed"},
	})
	kit := ui.New(theme.Nostalgia(), ui.Plain)

	wide := tm.Render(kit, ui.Bounds{Width: 80, Height: 24})
	for _, title := range []string{"CPU", "MEMORY"} {
		if strings.Contains(wide, title) {
			t.Fatalf("80x24 kept %s with no supported metrics:\n%s", title, wide)
		}
	}
	if !strings.Contains(wide, "very-long-process-identity-name") {
		t.Fatalf("80x24 did not give the released width to NAME:\n%s", wide)
	}
	if !strings.Contains(wide, "STATE") || !strings.Contains(wide, "running") {
		t.Fatalf("80x24 lost state with no supported metrics:\n%s", wide)
	}
	marker := kit.Theme().Focus.Marker
	if !lineHasAll(wide, marker, "41", "very-long-process-identity-name") {
		t.Fatalf("80x24 selected identity not visible:\n%s", wide)
	}

	minimum := tm.Render(kit, ui.Bounds{Width: 30, Height: 10})
	for _, title := range []string{"CPU", "MEMORY"} {
		if strings.Contains(minimum, title) {
			t.Fatalf("30x10 kept %s with no supported metrics:\n%s", title, minimum)
		}
	}
	if !lineHasAll(minimum, marker, "41", "very-long-proc") {
		t.Fatalf("30x10 selected identity not visible:\n%s", minimum)
	}

	cols := solveColumns(tableColumnsFor(tm.Processes()), 74, tm.pidWidth())
	widths := make(map[ColumnID]int, len(cols))
	for _, c := range cols {
		widths[c.id] = c.width
	}
	if _, ok := widths["cpu"]; ok {
		t.Fatalf("CPU still laid out: %v", widths)
	}
	if _, ok := widths["memory"]; ok {
		t.Fatalf("MEMORY still laid out: %v", widths)
	}
	if widths["name"] <= 20 || widths["state"] == 0 {
		t.Fatalf("released width did not reach identity/state: %v", widths)
	}
}

// lineHasAll reports whether one rendered line contains every part.
func lineHasAll(view string, parts ...string) bool {
	for _, line := range strings.Split(view, "\n") {
		ok := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// Supported samples keep the CPU and MEMORY columns with their formatted
// values, and mixed samples keep the existing placeholder for unsupported
// rows. The width-based 30x10 collapse still applies independently.
func TestSupportedAndMixedSamplesKeepMetricColumns(t *testing.T) {
	usage := platform.ResourceUsage{Supported: true, CPUUserMs: 15, CPUSysMs: 5, MemoryBytes: 512 * 1024}
	tm := New()
	tm.SetProcesses([]ProcessRow{
		{ID: 51, Name: "worker-a", State: "running", Resources: usage},
		{ID: 52, Name: "worker-b", State: "running", Resources: platform.ResourceUsage{Supported: false}},
	})
	kit := ui.New(theme.Nostalgia(), ui.Plain)

	wide := tm.Render(kit, ui.Bounds{Width: 80, Height: 24})
	for _, title := range []string{"CPU", "MEMORY"} {
		if !strings.Contains(wide, title) {
			t.Fatalf("80x24 dropped %s for a supported sample:\n%s", title, wide)
		}
	}
	rows := strings.Split(wide, "\n")
	var supported, unsupported string
	for _, line := range rows {
		if strings.Contains(line, "worker-a") {
			supported = line
		}
		if strings.Contains(line, "worker-b") {
			unsupported = line
		}
	}
	if !strings.Contains(supported, "20ms") || !strings.Contains(supported, "512 KB") {
		t.Fatalf("supported row lost formatted metrics: %q", supported)
	}
	if !strings.Contains(unsupported, "-") {
		t.Fatalf("unsupported row lost its placeholder: %q", unsupported)
	}

	allSupported := []ProcessRow{
		{ID: 51, Name: "worker-a", State: "running", Resources: usage},
		{ID: 52, Name: "worker-b", State: "running", Resources: usage},
	}
	tm.SetProcesses(allSupported)
	if got := tm.Render(kit, ui.Bounds{Width: 80, Height: 24}); !strings.Contains(got, "20ms") || strings.Contains(got, "- ") {
		t.Fatalf("all-supported population did not keep metrics:\n%s", got)
	}
	for _, title := range []string{"CPU", "MEMORY"} {
		if got := tm.Render(kit, ui.Bounds{Width: 30, Height: 10}); strings.Contains(got, title) {
			t.Fatalf("30x10 kept %s even though width collapses it:\n%s", title, got)
		}
	}
}

// Updating the displayed population between unsupported, mixed, and
// supported keeps the selection and the identity/state data usable while the
// metric columns follow the current samples.
func TestMetricColumnsFollowPopulationUpdates(t *testing.T) {
	usage := platform.ResourceUsage{Supported: true, CPUUserMs: 1200, CPUSysMs: 300, MemoryBytes: 15 * 1024 * 1024}
	unsupported := []ProcessRow{
		{ID: 61, Name: "alpha", State: "running"},
		{ID: 62, Name: "beta", State: "running"},
	}
	tm := New()
	tm.SetProcesses(unsupported)
	tm.SelectNext()
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	wide := ui.Bounds{Width: 80, Height: 24}

	if got := tm.Render(kit, wide); strings.Contains(got, "MEMORY") {
		t.Fatalf("unsupported population kept MEMORY:\n%s", got)
	}
	tm.SetProcesses([]ProcessRow{unsupported[0], {ID: 62, Name: "beta", State: "running", Resources: usage}})
	if p := tm.SelectedProcess(); p == nil || p.ID != 62 {
		t.Fatalf("selection changed across population update: %+v", p)
	}
	mixed := tm.Render(kit, wide)
	if !strings.Contains(mixed, "MEMORY") || !strings.Contains(mixed, "1.5s") || !strings.Contains(mixed, "15.0 MB") {
		t.Fatalf("mixed population did not restore metrics:\n%s", mixed)
	}
	if !lineHasAll(mixed, kit.Theme().Focus.Marker, "62", "beta") {
		t.Fatalf("selected identity lost after update:\n%s", mixed)
	}
	tm.SetProcesses(unsupported)
	if got := tm.Render(kit, wide); strings.Contains(got, "MEMORY") || strings.Contains(got, "CPU") {
		t.Fatalf("return to unsupported population kept metrics:\n%s", got)
	}
}

// An empty table has no samples, so it must not claim metric support or
// invent a metric column.
func TestEmptyProcessesDoNotReportMetricSupport(t *testing.T) {
	if resourcesSupported(nil) {
		t.Fatal("empty population reported resource support")
	}
	out := New().Render(ui.New(theme.Nostalgia(), ui.Plain), ui.Bounds{Width: 80, Height: 24})
	if !strings.Contains(out, "No Processes Found") {
		t.Fatalf("empty table lost its notice:\n%s", out)
	}
	if strings.Contains(out, "CPU") || strings.Contains(out, "MEMORY") {
		t.Fatalf("empty table invented metric columns:\n%s", out)
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

// Below the bordered tail's height the log text stays on screen under a
// one-line heading and scrolls by the same offset; from that height the
// bordered tail returns.
func TestCompactLogsShowAndScrollText(t *testing.T) {
	tm := New()
	kit := ui.New(theme.Nostalgia(), ui.Plain)
	tm.ShowLogs(7, "echo", "")
	if out := tm.Render(kit, ui.Bounds{Width: 26, Height: 3}); !strings.Contains(out, "(no output captured") {
		t.Fatalf("empty compact logs:\n%s", out)
	}
	tm.ShowLogs(7, "echo", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight")
	for h := 1; h < compactLogsHeight; h++ {
		out := tm.Render(kit, ui.Bounds{Width: 26, Height: h})
		if lines := strings.Split(out, "\n"); len(lines) != h || lipgloss.Width(out) != 26 {
			t.Fatalf("height %d: rendered %dx%d", h, lipgloss.Width(out), len(lines))
		}
		if !strings.Contains(out, "one") || strings.Contains(out, "CHILD LOG TAIL") {
			t.Fatalf("height %d: log text not shown compactly:\n%s", h, out)
		}
		if h >= 2 && !strings.HasPrefix(out, "LOGS · PID 7 · echo") {
			t.Fatalf("height %d: heading missing:\n%s", h, out)
		}
	}
	tm.ScrollLogs(bloom.Solve(26, 3, tm.LogsNode(kit, func() bool { return true })), func(v bloom.ViewportView) bloom.ViewportPos { return v.Down(2) })
	if out := tm.Render(kit, ui.Bounds{Width: 26, Height: 3}); !strings.Contains(out, "three") || !strings.Contains(out, "four") || strings.Contains(out, "two") {
		t.Fatalf("scrolled compact logs:\n%s", out)
	}
	out := tm.Render(kit, ui.Bounds{Width: 40, Height: compactLogsHeight})
	if !strings.Contains(out, "LOGS · PID 7") || !strings.Contains(out, "CHILD LOG TAIL") || !strings.Contains(out, "three") {
		t.Fatalf("bordered tail did not return at height %d:\n%s", compactLogsHeight, out)
	}
}
