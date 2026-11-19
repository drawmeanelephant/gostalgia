package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func numberedLog(prefix string, first, last int) string {
	var b strings.Builder
	for i := first; i <= last; i++ {
		fmt.Fprintf(&b, "%s %03d\n", prefix, i)
	}
	return b.String()
}

// logsFixture is an 80x24 Task Manager over two processes with logs.
func logsFixture(t *testing.T) (*Model, *experienceCaller) {
	t.Helper()
	client := &experienceCaller{
		procs: []taskmanager.ProcessRow{{ID: 1, Name: "init", State: "running"}, {ID: 2, Name: "worker", State: "running"}},
		logs:  map[int32]string{1: numberedLog("init", 1, 5), 2: numberedLog("worker", 1, 40)},
	}
	m := sized(NewWithTheme(context.Background(), client, nil, theme.Nostalgia(), ui.Plain), 80, 24)
	press(m, tea.KeyPressMsg{Code: tea.KeyF5})
	m.Update(procUpdateMsg{procs: client.procs})
	return m, client
}

// requestLogs selects pid in the table and presses l, returning the
// request without answering it.
func requestLogs(t *testing.T, m *Model, pid int32) tea.Cmd {
	t.Helper()
	for range 4 {
		if sel := m.tasks.SelectedProcess(); sel != nil && sel.ID == pid {
			break
		}
		press(m, keyDown)
		if sel := m.tasks.SelectedProcess(); sel != nil && sel.ID != pid {
			press(m, keyUp, keyUp)
		}
	}
	if sel := m.tasks.SelectedProcess(); sel == nil || sel.ID != pid {
		t.Fatalf("cannot select PID %d", pid)
	}
	_, cmd := m.Update(typed("l"))
	if cmd == nil {
		t.Fatal("l asked for no logs")
	}
	return cmd
}

// tickLogs runs a tick and returns the log refresh it asks for, answered.
func tickLogs(m *Model) (procLogsMsg, bool) {
	_, cmd := m.Update(tickMsg(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)))
	msgs := make(chan tea.Msg, 16)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					run(c)
				}
				return
			}
			msgs <- msg
		}()
	}
	run(cmd)
	timeout := time.After(250 * time.Millisecond)
	for {
		select {
		case msg := <-msgs:
			if lm, ok := msg.(procLogsMsg); ok {
				return lm, true
			}
		case <-timeout:
			return procLogsMsg{}, false
		}
	}
}

// The log tail scrolls, follows and searches from the keyboard; while a
// search is typed no key acts on the process, and Esc leaves the search,
// then the tail, then the Task Manager.
func TestLogTailKeyboardFollowAndSearch(t *testing.T) {
	m, client := logsFixture(t)
	m.Update(requestLogs(t, m, 2)())
	if pid, open := m.tasks.LogsPID(); !open || pid != 2 || !strings.Contains(bodyText(t, m), "worker 001") {
		t.Fatalf("l did not open PID 2:\n%s", bodyText(t, m))
	}
	help := screenText(m.View())
	for _, want := range []string{"f FOLLOW", "/ SEARCH"} {
		if !strings.Contains(help[len(help)-1], want) {
			t.Fatalf("help bar lacks %q: %s", want, help[len(help)-1])
		}
	}

	press(m, keyDown, keyDown, keyDown)
	if line := m.tasks.LogsPos().Line; line != 3 {
		t.Fatalf("Down three times reached line %d", line)
	}
	press(m, keyUp)
	if line := m.tasks.LogsPos().Line; line != 2 {
		t.Fatalf("Up reached line %d", line)
	}
	press(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	end := m.tasks.LogsPos()
	if end.AtEnd() || !strings.Contains(bodyText(t, m), "worker 040") {
		t.Fatalf("End left %+v", end)
	}
	for range 3 {
		press(m, keyPgDown)
	}
	if m.tasks.LogsPos() != end {
		t.Fatalf("PgDown past the end moved to %+v from %+v", m.tasks.LogsPos(), end)
	}
	press(m, tea.KeyPressMsg{Code: tea.KeyHome})
	if line := m.tasks.LogsPos().Line; line != 0 {
		t.Fatalf("Home reached line %d", line)
	}

	press(m, typed("f"))
	client.logs[2] = numberedLog("worker", 1, 45)
	refresh, ok := tickLogs(m)
	if !ok || refresh.pid != 2 || refresh.open {
		t.Fatalf("a tick asked for %+v (%v), want a refresh of PID 2", refresh, ok)
	}
	if _, again := tickLogs(m); again {
		t.Fatal("a second refresh was sent while one was in flight")
	}
	m.Update(refresh)
	if body := bodyText(t, m); !m.tasks.Following() || !strings.Contains(body, "worker 045") || !strings.Contains(body, "FOLLOW") {
		t.Fatalf("a followed refresh is not on screen:\n%s", body)
	}

	press(m, typed("f"))
	top := m.tasks.LogsPos()
	client.logs[2] = numberedLog("worker", 1, 50)
	refresh, _ = tickLogs(m)
	m.Update(refresh)
	if body := bodyText(t, m); m.tasks.LogsPos() != top || strings.Contains(body, "worker 050") {
		t.Fatalf("an unfollowed refresh moved the tail from %+v to %+v", top, m.tasks.LogsPos())
	}

	press(m, typed("/"))
	if !m.tasks.Searching() {
		t.Fatal("/ did not start a search")
	}
	press(m, typed("x"), typed("r"), typed("c"), typed("l"), typed("f"), typed("n"), typed("N"), typed("/"), keyUp, keyPgUp)
	if m.confirm != nil || m.tasks.Mode() != taskmanager.ModeLogs || m.tasks.Following() || m.tasks.LogsPos() != top {
		t.Fatalf("search input acted on the process: confirm %v, mode %v", m.confirm, m.tasks.Mode())
	}
	if help := screenText(m.View()); !strings.Contains(help[len(help)-1], "Enter FIND") || !strings.Contains(bodyText(t, m), "Search: xrclfnN/_") {
		t.Fatalf("the search being typed is not shown:\n%s", strings.Join(help, "\n"))
	}
	for range 8 {
		press(m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	m.Update(tea.PasteMsg{Content: "WORKER 04"})
	press(m, keyEnter)
	if body := bodyText(t, m); m.tasks.Searching() || !strings.Contains(body, "Match 1 of 10") {
		t.Fatalf("Enter did not find the matches:\n%s", body)
	}
	marker := m.kit.Theme().Focus.Marker
	if body := bodyText(t, m); !strings.Contains(body, marker+" worker 040") {
		t.Fatalf("the first match is not marked:\n%s", body)
	}
	press(m, typed("n"))
	if body := bodyText(t, m); !strings.Contains(body, "Match 2 of 10") || !strings.Contains(body, marker+" worker 041") {
		t.Fatalf("n did not move to the next match:\n%s", body)
	}
	press(m, typed("N"), typed("N"))
	if body := bodyText(t, m); !strings.Contains(body, "Match 10 of 10") || !strings.Contains(body, marker+" worker 049") {
		t.Fatalf("N did not wrap to the last match:\n%s", body)
	}
	if strings.Contains(m.View().Content, "\x1b") {
		t.Fatal("Plain search drew an escape sequence")
	}

	press(m, typed("/"), typed("q"), keyEsc)
	if m.tasks.Searching() || m.tasks.Mode() != taskmanager.ModeLogs || !strings.Contains(bodyText(t, m), "Match 10 of 10") {
		t.Fatal("Esc did not cancel only the search")
	}
	press(m, keyEsc)
	if m.tasks.Mode() != taskmanager.ModeTable || !m.taskView {
		t.Fatal("Esc did not return from the tail to the table")
	}
	press(m, keyEsc)
	if m.taskView {
		t.Fatal("Esc did not leave the Task Manager")
	}
}

// While a command runs, the open log tail still scrolls, follows and
// searches, and Enter still returns to the table; only its refresh waits
// for the command, as the process table's does.
func TestLogTailKeysWhileBusy(t *testing.T) {
	m, _ := logsFixture(t)
	m.Update(requestLogs(t, m, 2)())
	m.busy = true
	press(m, keyDown, keyDown)
	if line := m.tasks.LogsPos().Line; line != 2 {
		t.Fatalf("Down twice while busy reached line %d", line)
	}
	press(m, typed("f"))
	if !m.tasks.Following() {
		t.Fatal("f while busy did not follow")
	}
	press(m, typed("/"), typed("0"), typed("3"), typed("8"), keyEnter)
	if body := bodyText(t, m); !strings.Contains(body, "Match 1 of 1") || !strings.Contains(body, m.kit.Theme().Focus.Marker+" worker 038") {
		t.Fatalf("searching while busy shows:\n%s", body)
	}
	if _, ok := tickLogs(m); ok {
		t.Fatal("the tail refreshed while a command ran")
	}
	press(m, keyEnter)
	if m.tasks.Mode() != taskmanager.ModeTable {
		t.Fatalf("Enter while busy left mode %v", m.tasks.Mode())
	}
}

// A late answer for another process, or for a request since superseded,
// never shows its lines; a late refresh still brings that process's own
// kept tail up to date for when it is opened again.
func TestLogResponsesForOtherProcesses(t *testing.T) {
	m, client := logsFixture(t)
	forWorker := requestLogs(t, m, 2)
	forInit := requestLogs(t, m, 1)
	m.Update(forInit())
	m.Update(forWorker())
	if pid, _ := m.tasks.LogsPID(); pid != 1 || strings.Contains(bodyText(t, m), "worker") {
		t.Fatalf("a superseded request opened PID %d:\n%s", pid, bodyText(t, m))
	}

	press(m, keyEsc)
	m.Update(requestLogs(t, m, 2)())
	for range 5 {
		press(m, keyDown)
	}
	client.logs[2] = numberedLog("worker", 1, 60)
	refresh, ok := tickLogs(m)
	if !ok || refresh.pid != 2 {
		t.Fatalf("no refresh of PID 2: %+v", refresh)
	}
	press(m, keyEsc)
	m.Update(requestLogs(t, m, 1)())
	initScreen := bodyText(t, m)
	m.Update(refresh)
	if got := bodyText(t, m); got != initScreen {
		t.Fatalf("a late refresh of PID 2 changed PID 1's tail:\n%s", got)
	}

	press(m, keyEsc)
	m.Update(requestLogs(t, m, 2)())
	if pos := m.tasks.LogsPos(); pos.Line != 5 {
		t.Fatalf("reopening PID 2 put line %d at the top, want 5", pos.Line)
	}
	press(m, tea.KeyPressMsg{Code: tea.KeyEnd})
	if body := bodyText(t, m); !strings.Contains(body, "worker 060") || strings.Count(body, "worker 052") != 1 {
		t.Fatalf("PID 2's tail missed its late refresh, or doubled it:\n%s", body)
	}

	// An answer arriving after the table was left opens nothing.
	press(m, keyEsc)
	late := requestLogs(t, m, 1)
	press(m, keyEsc)
	m.Update(late())
	if m.taskView || m.tasks.Mode() != taskmanager.ModeTable {
		t.Fatalf("a late answer opened the tail: task view %v, mode %v", m.taskView, m.tasks.Mode())
	}
}
