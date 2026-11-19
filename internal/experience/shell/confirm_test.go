package shell

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

type ipcCall struct{ method, params string }

// recordingCaller records every request and answers each with an empty
// success.
type recordingCaller struct {
	mu    sync.Mutex
	calls []ipcCall
}

func (r *recordingCaller) Call(_ context.Context, method string, params, _ any) error {
	b, _ := json.Marshal(params)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, ipcCall{method, string(b)})
	return nil
}

var destructiveMethods = map[string]bool{"sys/shutdown": true, "app/stop": true, "proc/stop": true, "proc/reap": true}

// destructive returns the recorded requests that destroy something.
func (r *recordingCaller) destructive() []ipcCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []ipcCall
	for _, c := range r.calls {
		if destructiveMethods[c.method] {
			out = append(out, c)
		}
	}
	return out
}

func (r *recordingCaller) all() []ipcCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ipcCall(nil), r.calls...)
}

// recordingFixture is the populated fixture shell talking to a recording
// caller, at 80×24.
func recordingFixture(t *testing.T) (*Model, *recordingCaller) {
	t.Helper()
	rec := &recordingCaller{}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.client = rec
	return m, rec
}

func key(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

func typed(s string) tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyExtended, Text: s} }

var (
	tabKey      = key(tea.KeyTab)
	shiftTabKey = tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	enterKey    = key(tea.KeyEnter)
	repeatEnter = tea.KeyPressMsg{Code: tea.KeyEnter, IsRepeat: true}
	escKey      = key(tea.KeyEsc)
)

// act sends k through the shell's public Update and runs the command it
// returns, as Bubble Tea would, feeding the result back. Commands here are
// one-shot requests; nothing in these paths schedules a timer.
func act(t *testing.T, m *Model, k tea.Msg) {
	t.Helper()
	_, cmd := m.Update(k)
	runCmd(t, m, cmd)
}

func runCmd(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(t, m, c)
		}
	case tea.QuitMsg:
	default:
		_, next := m.Update(msg)
		// Results only schedule follow-up refreshes; run none of them.
		_ = next
	}
}

// dialogText is the screen with the open dialog on it.
func dialogText(m *Model) string {
	return ansi.Strip(m.View().Content)
}

// dialogWords is the text inside the dialog's border on screen, its
// wrapped lines joined with single spaces.
func dialogWords(m *Model) string {
	var words []string
	for _, line := range strings.Split(dialogText(m), "\n") {
		first, last := strings.Index(line, "║"), strings.LastIndex(line, "║")
		if first >= 0 && last > first {
			words = append(words, strings.Fields(line[first+len("║"):last])...)
		}
	}
	return strings.Join(words, " ")
}

// selectPID moves the Task Manager selection to pid.
func selectPID(t *testing.T, m *Model, pid int32) {
	t.Helper()
	for range m.tasks.Processes() {
		m.tasks.SelectPrev()
	}
	for range m.tasks.Processes() {
		if sel := m.tasks.SelectedProcess(); sel != nil && sel.ID == pid {
			return
		}
		m.tasks.SelectNext()
	}
	t.Fatalf("no process %d", pid)
}

// entryPath opens one destructive action's confirmation the way a user
// reaches it.
type entryPath struct {
	name   string
	open   func(t *testing.T, m *Model)
	method string
	params string
	shows  []string
}

func entryPaths() []entryPath {
	palette := func(query string) func(t *testing.T, m *Model) {
		return func(t *testing.T, m *Model) {
			m.setMode(modePrompt)
			act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
			act(t, m, typed(query))
			act(t, m, enterKey)
		}
	}
	prompt := func(line string) func(t *testing.T, m *Model) {
		return func(t *testing.T, m *Model) {
			m.setMode(modePrompt)
			m.input, m.cursor = nil, 0
			act(t, m, typed(line))
			act(t, m, enterKey)
		}
	}
	return []entryPath{
		{"typed shutdown", prompt("shutdown"), "sys/shutdown", `{"reason":"requested by Charm shell"}`,
			[]string{"Shut down Gostalgia?", "Stops every app and service", "every attached session"}},
		{"palette shutdown", palette("shut down gostalgia"), "sys/shutdown", `{"reason":"requested by Charm shell"}`,
			[]string{"Shut down Gostalgia?"}},
		{"typed stop", prompt("stop com.gostalgia.echo"), "app/stop", `{"id":"com.gostalgia.echo"}`,
			[]string{"Stop Echo?", "com.gostalgia.echo", "PID 7"}},
		{"palette stop", palette("stop echo"), "app/stop", `{"id":"com.gostalgia.echo"}`,
			[]string{"Stop Echo?", "com.gostalgia.echo", "PID 7"}},
		{"launcher F3", func(t *testing.T, m *Model) {
			m.setMode(modeLauncher)
			m.selected = 0
			act(t, m, key(tea.KeyF3))
		}, "app/stop", `{"id":"com.gostalgia.echo"}`, []string{"Stop Echo?", "com.gostalgia.echo", "PID 7"}},
		{"tasks x", func(t *testing.T, m *Model) {
			m.taskView = true
			selectPID(t, m, 7)
			act(t, m, typed("x"))
		}, "proc/stop", `{"id":7}`, []string{"Stop process 7?", "com.gostalgia.echo (PID 7)"}},
		{"tasks r", func(t *testing.T, m *Model) {
			m.taskView = true
			act(t, m, typed("r"))
		}, "proc/reap", `{}`, []string{"Reap ended processes?", "every terminated process in the environment", "not only this row"}},
		{"typed reap", prompt("reap"), "proc/reap", `{}`, []string{"Reap ended processes?", "in the environment"}},
	}
}

// Every destructive entry path opens a confirmation without a request;
// Esc and Cancel close it without one; confirming sends exactly one request
// for the target the dialog showed.
func TestDestructiveActionsConfirmTheirTarget(t *testing.T) {
	for _, p := range entryPaths() {
		t.Run(p.name, func(t *testing.T) {
			// Open, then Esc.
			m, rec := recordingFixture(t)
			p.open(t, m)
			if m.Focus() != FocusDialog {
				t.Fatalf("focus = %v, want the dialog", m.Focus())
			}
			for _, want := range p.shows {
				if !strings.Contains(dialogWords(m), want) {
					t.Fatalf("dialog lacks %q:\n%s", want, dialogText(m))
				}
			}
			if got := rec.destructive(); len(got) != 0 {
				t.Fatalf("opening sent %v", got)
			}
			act(t, m, escKey)
			if m.confirm != nil || len(rec.destructive()) != 0 {
				t.Fatalf("Esc left the dialog open or sent %v", rec.destructive())
			}

			// Open, Tab to the action and back, then Enter on Cancel.
			p.open(t, m)
			act(t, m, tabKey)
			act(t, m, shiftTabKey)
			if m.confirm.active != confirmCancel {
				t.Fatalf("Shift-Tab did not return to Cancel: %d", m.confirm.active)
			}
			act(t, m, enterKey)
			if m.confirm != nil || len(rec.destructive()) != 0 {
				t.Fatalf("Cancel left the dialog open or sent %v", rec.destructive())
			}

			// Open, Tab, held Enter repeats, then one Enter.
			p.open(t, m)
			act(t, m, tabKey)
			if m.confirm.active != confirmProceed {
				t.Fatalf("Tab did not move to the action: %d", m.confirm.active)
			}
			act(t, m, repeatEnter)
			if m.confirm == nil || len(rec.destructive()) != 0 {
				t.Fatal("a repeat-marked Enter confirmed")
			}
			act(t, m, enterKey)
			got := rec.destructive()
			if len(got) != 1 || got[0].method != p.method || got[0].params != p.params {
				t.Fatalf("confirming sent %v, want one %s %s", got, p.method, p.params)
			}
			if m.confirm != nil {
				t.Fatal("dialog stayed open after confirming")
			}
			// Further Enters reach the screen beneath, never the action again.
			act(t, m, enterKey)
			act(t, m, repeatEnter)
			if n := len(rec.destructive()); n != 1 {
				t.Fatalf("%d destructive requests after repeated Enter", n)
			}
		})
	}
}

// Raw IPC, unrelated commands and app actions are outside the audit and
// run as before.
func TestUnrelatedCommandsDoNotConfirm(t *testing.T) {
	for _, line := range []string{"call sys/status", "status", "stop", "stop a b", "shutdown now", "reap all"} {
		m, _ := recordingFixture(t)
		m.setMode(modePrompt)
		act(t, m, typed(line))
		if _, cmd := m.Update(enterKey); m.confirm != nil || cmd == nil {
			t.Fatalf("%q opened a confirmation instead of running", line)
		}
	}
}

// The dialog keeps the target it showed: a refresh that relaunches the app,
// stops the process, or reorders the rows can disable confirmation, never
// retarget it.
func TestConfirmationNeverRetargets(t *testing.T) {
	t.Run("app relaunched under a new PID", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.setMode(modeLauncher)
		m.selected = 0
		act(t, m, key(tea.KeyF3))
		act(t, m, tabKey)
		relaunched := append([]appStatus(nil), m.apps...)
		relaunched[0].PID = 44
		relaunched[0], relaunched[2] = relaunched[2], relaunched[0]
		m.Update(resultMsg{apps: relaunched})
		if !strings.Contains(dialogWords(m), "no longer running as PID 7") {
			t.Fatalf("dialog does not say its target went away:\n%s", dialogText(m))
		}
		act(t, m, enterKey)
		act(t, m, tabKey)
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("stale confirmation sent %v", got)
		}
	})
	// The app list refreshes only after a command, but the process table
	// is polled; a relaunch the poll sees first must still disable the
	// stop, because app/stop would reach the new instance.
	t.Run("app relaunched, seen only in the process table", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.setMode(modeLauncher)
		m.selected = 0
		act(t, m, key(tea.KeyF3))
		act(t, m, tabKey)
		if m.confirm.active != confirmProceed {
			t.Fatal("stop was not confirmable before the relaunch")
		}
		rows := m.tasks.Processes()
		rows[1] = taskmanager.ProcessRow{ID: 45, Name: "com.gostalgia.echo", Kind: "app", State: "running"}
		m.Update(procUpdateMsg{procs: rows})
		if !strings.Contains(dialogWords(m), "no longer running as PID 7") {
			t.Fatalf("dialog does not say its target went away:\n%s", dialogText(m))
		}
		act(t, m, enterKey)
		act(t, m, tabKey)
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("stale confirmation sent %v", got)
		}
	})
	t.Run("app listed as running but its process has failed", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.setMode(modeLauncher)
		m.selected = 2 // Clock: the list says PID 31, the table says it failed
		act(t, m, key(tea.KeyF3))
		if m.confirm == nil || !strings.Contains(dialogWords(m), "Clock is no longer running as PID 31.") {
			t.Fatalf("dialog does not explain:\n%s", dialogText(m))
		}
		m.confirm.active = confirmProceed
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("stopped a failed app: %v", got)
		}
	})
	t.Run("process stopped", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.taskView = true
		selectPID(t, m, 7)
		act(t, m, typed("x"))
		act(t, m, tabKey)
		rows := m.tasks.Processes()
		rows[1].State = "stopped"
		m.Update(procUpdateMsg{procs: rows})
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("confirmed a stopped process: %v", got)
		}
		if m.confirm == nil {
			t.Fatal("dialog closed though nothing ran")
		}
	})
	t.Run("rows reordered and selection moved", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.taskView = true
		selectPID(t, m, 7)
		act(t, m, typed("x"))
		rows := m.tasks.Processes()
		rows = append([]taskmanager.ProcessRow{{ID: 3, Name: "newcomer", State: "running"}}, rows...)
		m.Update(procUpdateMsg{procs: rows})
		if sel := m.tasks.SelectedProcess(); sel == nil || sel.ID == 7 {
			t.Fatalf("fixture did not move the selection off PID 7: %+v", sel)
		}
		act(t, m, tabKey)
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 1 || got[0].params != `{"id":7}` {
			t.Fatalf("confirmation retargeted: %v", got)
		}
	})
}

// Disabled actions never run: not as the active index, not when they
// become disabled after the dialog opens, and not while the shell is busy
// or another process action is in flight.
func TestDisabledConfirmationNeverRuns(t *testing.T) {
	t.Run("disabled active index", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.openConfirm(confirmation{kind: confirmShutdown})
		m.busy = true
		m.confirm.active = confirmProceed
		if d := m.confirmDialog(); !d.Actions[confirmProceed].Disabled {
			t.Fatal("busy shell left the action enabled")
		}
		if !strings.Contains(dialogWords(m), "(Shut down)") {
			t.Fatalf("disabled action lacks the kit's disabled presentation:\n%s", dialogText(m))
		}
		act(t, m, enterKey)
		act(t, m, tabKey)
		if m.confirm.active != confirmCancel {
			t.Fatalf("Tab landed on a disabled action: %d", m.confirm.active)
		}
		act(t, m, tabKey)
		if m.confirm.active != confirmCancel {
			t.Fatalf("Tab landed on a disabled action: %d", m.confirm.active)
		}
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("disabled action ran: %v", got)
		}
		m.busy = false
		act(t, m, tabKey)
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 1 {
			t.Fatalf("re-enabled action sent %v", got)
		}
	})
	t.Run("pending process action", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.taskView = true
		selectPID(t, m, 7)
		act(t, m, typed("x"))
		act(t, m, tabKey)
		_, stop := m.Update(enterKey)
		if stop == nil || !m.procPending {
			t.Fatal("confirming did not start the stop")
		}
		act(t, m, typed("r"))
		act(t, m, tabKey)
		if m.confirm == nil || m.confirm.active != confirmCancel || !strings.Contains(dialogWords(m), "still in progress") {
			t.Fatalf("reap confirmable while a stop is in flight:\n%s", dialogText(m))
		}
		runCmd(t, m, stop) // the stop's result arrives
		if m.procPending {
			t.Fatal("result did not clear the pending action")
		}
		act(t, m, tabKey)
		act(t, m, enterKey)
		got := rec.destructive()
		if len(got) != 2 || got[0].method != "proc/stop" || got[1].method != "proc/reap" {
			t.Fatalf("requests = %v", got)
		}
	})
	t.Run("unknown or stopped app", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.setMode(modePrompt)
		act(t, m, typed("stop com.gostalgia.notes"))
		act(t, m, enterKey)
		if !strings.Contains(dialogWords(m), "Notes is not running.") {
			t.Fatalf("dialog does not explain:\n%s", dialogText(m))
		}
		act(t, m, tabKey)
		act(t, m, enterKey)
		if m.confirm != nil {
			t.Fatal("Enter on Cancel left the dialog open")
		}
		m.input, m.cursor = nil, 0
		act(t, m, typed("stop no.such.app"))
		act(t, m, enterKey)
		m.confirm.active = confirmProceed
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("stopped a stopped or unknown app: %v", got)
		}
	})
	t.Run("F3 on a stopped app", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.setMode(modeLauncher)
		m.selected = 1 // Notes, not running
		act(t, m, key(tea.KeyF3))
		if m.confirm != nil || len(rec.all()) != 0 {
			t.Fatalf("F3 on a stopped app opened %v or sent %v", m.confirm, rec.all())
		}
	})
	t.Run("busy shell from the palette", func(t *testing.T) {
		m, rec := recordingFixture(t)
		m.busy = true
		m.setMode(modePrompt)
		act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		act(t, m, typed("shut down gostalgia"))
		act(t, m, enterKey)
		m.confirm.active = confirmProceed
		act(t, m, enterKey)
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("busy palette shutdown sent %v", got)
		}
	})
}

// A typed destructive command keeps its draft until it is confirmed.
func TestTypedConfirmationKeepsDraftUntilConfirmed(t *testing.T) {
	m, _ := recordingFixture(t)
	m.setMode(modePrompt)
	act(t, m, typed("shutdown"))
	act(t, m, enterKey)
	act(t, m, escKey)
	if string(m.input) != "shutdown" || m.cursor != len("shutdown") {
		t.Fatalf("cancel lost the draft: %q at %d", string(m.input), m.cursor)
	}
}

// At the minimum screen every dialog shows its whole body, blocked reason
// included, and both actions, so the scope of what is confirmed is never
// cut off.
func TestConfirmationsFitTheMinimumScreen(t *testing.T) {
	check := func(t *testing.T, m *Model) {
		t.Helper()
		m.Update(tea.WindowSizeMsg{Width: minFrameWidth, Height: minFrameHeight})
		d := m.confirmDialog()
		words := dialogWords(m)
		for _, want := range []string{d.Title, strings.Join(strings.Fields(d.Body), " "), d.Actions[0].Label, d.Actions[1].Label} {
			if !strings.Contains(words, want) {
				t.Fatalf("%d×%d dialog lacks %q:\n%s", minFrameWidth, minFrameHeight, want, dialogText(m))
			}
		}
	}
	for _, p := range entryPaths() {
		t.Run(p.name, func(t *testing.T) {
			m, _ := recordingFixture(t)
			p.open(t, m)
			check(t, m)
		})
	}
	t.Run("blocked", func(t *testing.T) {
		m, _ := recordingFixture(t)
		m.taskView = true
		selectPID(t, m, 7)
		act(t, m, typed("x"))
		m.procPending = true
		check(t, m)
	})
}

// Below the minimum size the dialog is not drawn, so Enter there confirms
// nothing; the dialog is still open once the screen is large enough again.
func TestEnterBelowMinimumSizeDoesNotConfirm(t *testing.T) {
	m, rec := recordingFixture(t)
	m.openConfirm(confirmation{kind: confirmShutdown})
	act(t, m, tabKey)
	m.Update(tea.WindowSizeMsg{Width: minFrameWidth - 1, Height: minFrameHeight})
	act(t, m, enterKey)
	m.Update(tea.WindowSizeMsg{Width: minFrameWidth, Height: minFrameHeight - 1})
	act(t, m, enterKey)
	if got := rec.destructive(); len(got) != 0 || m.confirm == nil {
		t.Fatalf("hidden dialog confirmed: %v, open %v", got, m.confirm != nil)
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	act(t, m, enterKey)
	if got := rec.destructive(); len(got) != 1 || got[0].method != "sys/shutdown" {
		t.Fatalf("visible dialog sent %v", got)
	}
}
