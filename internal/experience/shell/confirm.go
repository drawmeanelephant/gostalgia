package shell

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// confirmKind is a destructive action that waits for confirmation.
type confirmKind int

const (
	confirmShutdown confirmKind = iota + 1
	confirmStopApp
	confirmStopProcess
	confirmReap
)

// The dialog's actions, in order. Cancel comes first and is focused when
// the dialog opens, so Enter alone never destroys anything.
const (
	confirmCancel = iota
	confirmProceed
)

// confirmation is an open confirmation dialog. It holds the target as it was
// shown, never a row index or selection, so a refresh or reorder between
// opening and confirming cannot retarget it.
type confirmation struct {
	kind confirmKind
	// appID and pid identify a Launcher app and the process it ran as when
	// the dialog opened (pid is 0 for an app that was not running). For a
	// process stop, pid is the process and name its process name.
	appID string
	name  string
	pid   int32
	// typed reaps through the shell command, so its result lands in the
	// transcript as a typed reap always has.
	typed  bool
	active int
}

// destructiveLine reports the confirmation a typed shell line asks for.
// Lines with the wrong arguments fall through to the command, which reports
// its usage without making any request.
func (m *Model) destructiveLine(line string) (confirmation, bool) {
	args, err := words(line)
	if err != nil || len(args) == 0 {
		return confirmation{}, false
	}
	switch cmd, args := strings.ToLower(args[0]), args[1:]; {
	case cmd == "shutdown" && len(args) == 0:
		return confirmation{kind: confirmShutdown}, true
	case cmd == "reap" && len(args) == 0:
		return confirmation{kind: confirmReap, typed: true}, true
	case cmd == "stop" && len(args) == 1:
		for _, a := range m.apps {
			if a.Manifest.ID == args[0] {
				return stopAppConfirmation(a), true
			}
		}
		return confirmation{kind: confirmStopApp, appID: args[0], name: args[0]}, true
	}
	return confirmation{}, false
}

func stopAppConfirmation(a appStatus) confirmation {
	c := confirmation{kind: confirmStopApp, appID: a.Manifest.ID, name: a.Manifest.Name}
	if a.Running {
		c.pid = a.PID
	}
	return c
}

// run executes a shell command line, opening a confirmation first when the
// line is destructive.
func (m *Model) run(line string) tea.Cmd {
	if c, ok := m.destructiveLine(line); ok {
		m.openConfirm(c)
		return nil
	}
	return m.submit(line)
}

func (m *Model) openConfirm(c confirmation) {
	c.active = confirmCancel
	m.confirm = &c
}

// stoppable reports whether a process in state can still be stopped: it
// has not ended and is not already stopping.
func stoppable(state string) bool {
	switch state {
	case "stopping", "stopped", "failed", "crashloop":
		return false
	}
	return true
}

// confirmBlocked returns why c cannot run right now, or "" when it can. It
// is checked when the dialog is drawn and again at the moment of
// confirmation, against the shell's latest app and process state.
func (m *Model) confirmBlocked(c confirmation) string {
	if m.busy {
		return "Another command is still running."
	}
	if m.procPending {
		return "A process action is still in progress."
	}
	switch c.kind {
	case confirmStopApp:
		name := viewText(c.name)
		if c.pid == 0 {
			return name + " is not running."
		}
		running := false
		for _, a := range m.apps {
			if a.Manifest.ID == c.appID {
				running = a.Running && a.PID == c.pid
			}
		}
		if !running {
			return fmt.Sprintf("%s is no longer running as PID %d.", name, c.pid)
		}
	case confirmStopProcess:
		alive := false
		if m.tasks != nil {
			for _, p := range m.tasks.Processes() {
				if p.ID == c.pid {
					alive = stoppable(p.State)
				}
			}
		}
		if !alive {
			return fmt.Sprintf("PID %d is no longer running.", c.pid)
		}
	}
	return ""
}

// confirmDialog is the open confirmation as a kit dialog.
func (m *Model) confirmDialog() ui.Dialog {
	c := *m.confirm
	var title, body, verb string
	switch c.kind {
	case confirmShutdown:
		title, verb = "Shut down Gostalgia?", "Shut down"
		body = "Stops every app and service in this environment and ends every attached session."
	case confirmStopApp:
		title, verb = "Stop "+viewText(c.name)+"?", "Stop app"
		body = fmt.Sprintf("Stops %s (%s), running as PID %d. Work it has not saved may be lost.",
			viewText(c.name), viewText(c.appID), c.pid)
		if c.pid == 0 {
			body = fmt.Sprintf("App: %s (%s).", viewText(c.name), viewText(c.appID))
		}
	case confirmStopProcess:
		title, verb = fmt.Sprintf("Stop process %d?", c.pid), "Stop process"
		body = fmt.Sprintf("Stops %s (PID %d).", viewText(c.name), c.pid)
	case confirmReap:
		title, verb = "Reap terminated processes?", "Reap all"
		body = "Removes every terminated process from the process table across the whole environment, not only the selected row."
	}
	blocked := m.confirmBlocked(c)
	if blocked != "" {
		body += " " + blocked
	}
	return ui.Dialog{
		Title: title, Body: body, State: theme.Error, Active: c.active,
		Actions: []ui.Action{{Label: "Cancel"}, {Label: verb, Disabled: blocked != ""}},
	}
}

// handleConfirmKey is every keystroke while a confirmation is open: the
// dialog owns input until it closes, so nothing reaches the screen beneath.
func (m *Model) handleConfirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	actions := m.confirmDialog().Actions
	switch msg.String() {
	case "esc":
		m.confirm = nil
	case "tab", "right":
		m.moveConfirm(actions, 1)
	case "shift+tab", "left":
		m.moveConfirm(actions, -1)
	case "enter":
		if msg.IsRepeat {
			// A held Enter acts once; its repeats never confirm.
			return m, nil
		}
		active := m.confirm.active
		if active < 0 || active >= len(actions) || actions[active].Disabled {
			return m, nil
		}
		c := *m.confirm
		if active == confirmCancel {
			m.confirm = nil
			return m, nil
		}
		if m.confirmBlocked(c) != "" {
			return m, nil
		}
		m.confirm = nil
		return m, m.confirmed(c)
	}
	return m, nil
}

// moveConfirm moves the dialog's focus by step to the next enabled action.
func (m *Model) moveConfirm(actions []ui.Action, step int) {
	n := len(actions)
	for i, at := 1, m.confirm.active; i <= n; i++ {
		next := ((at+step*i)%n + n) % n
		if !actions[next].Disabled {
			m.confirm.active = next
			return
		}
	}
}

// confirmed performs c, which the user has just confirmed: exactly one
// request for the target the dialog showed.
func (m *Model) confirmed(c confirmation) tea.Cmd {
	switch c.kind {
	case confirmShutdown:
		return m.submit("shutdown")
	case confirmStopApp:
		return m.submit("stop " + c.appID)
	case confirmStopProcess:
		m.procPending = true
		return m.stopProcessCmd(c.pid)
	case confirmReap:
		if c.typed {
			return m.submit("reap")
		}
		m.procPending = true
		return m.reapProcessesCmd()
	}
	return nil
}
