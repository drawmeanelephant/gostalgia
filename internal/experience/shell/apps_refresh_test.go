package shell

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// appListCaller answers app/list from a queue as the shell asks, so a test
// can hold a reply and choose the order its answers come back in.
type appListCaller struct {
	replies []appListReply
	asked   int
}

type appListReply struct {
	apps []appStatus
	err  error
}

func (c *appListCaller) Call(_ context.Context, method string, _, out any) error {
	if method != "app/list" {
		return nil
	}
	if c.asked >= len(c.replies) {
		return errors.New("unexpected app/list call")
	}
	r := c.replies[c.asked]
	c.asked++
	if r.err != nil {
		return r.err
	}
	data, err := json.Marshal(r.apps)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func runningApp(id, name string, pid int32) appStatus {
	return fixtureApp(id, name, true, pid)
}

func readyApp(id, name string) appStatus {
	return fixtureApp(id, name, false, 0)
}

func selectedAppID(m *Model) string {
	filtered := m.filteredApps()
	if m.selected < 0 || m.selected >= len(filtered) {
		return ""
	}
	return filtered[m.selected].Manifest.ID
}

// Each way into Apps reads the registry again. The tab and F2 share
// switchScreen, the palette item and typed launcher go through it too, and
// the apps command switches through the same path.
func TestAppsEntryRefreshesEveryRoute(t *testing.T) {
	t.Run("F2", func(t *testing.T) {
		m := New(context.Background(), noopCaller{}, nil)
		if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF2}); cmd == nil {
			t.Fatal("F2 returned no command")
		}
		if m.Focus() != FocusLauncher || m.appsSent != 1 {
			t.Fatalf("focus %v, app refreshes %d", m.Focus(), m.appsSent)
		}
	})
	t.Run("header tab", func(t *testing.T) {
		m := New(context.Background(), noopCaller{}, nil)
		if cmd := m.switchScreen(screenApps, false); cmd == nil {
			t.Fatal("tab screen switch returned no command")
		}
		if m.Focus() != FocusLauncher || m.appsSent != 1 {
			t.Fatalf("focus %v, app refreshes %d", m.Focus(), m.appsSent)
		}
	})
	t.Run("palette", func(t *testing.T) {
		m := New(context.Background(), noopCaller{}, nil)
		m.setMode(modePrompt)
		m.openPalette()
		m.paletteQuery = "apps"
		if cmd := m.executePaletteItem(); cmd == nil {
			t.Fatal("palette apps item returned no command")
		}
		if m.Focus() != FocusLauncher || m.appsSent != 1 {
			t.Fatalf("focus %v, app refreshes %d", m.Focus(), m.appsSent)
		}
	})
	t.Run("typed launcher", func(t *testing.T) {
		m := New(context.Background(), noopCaller{}, nil)
		m.setMode(modePrompt)
		m.input = []rune("launcher")
		m.cursor = len(m.input)
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatal("typed launcher returned no command")
		}
		if m.Focus() != FocusLauncher || m.appsSent != 1 {
			t.Fatalf("focus %v, app refreshes %d", m.Focus(), m.appsSent)
		}
	})
	t.Run("apps command", func(t *testing.T) {
		m := New(context.Background(), noopCaller{}, nil)
		m.setMode(modePrompt)
		res := execute(context.Background(), noopCaller{}, m.cwd, "apps")
		if res.switchView != "shelf" {
			t.Fatalf("apps command switchView = %q", res.switchView)
		}
		if _, cmd := m.Update(res); cmd == nil {
			t.Fatal("apps command returned no command")
		}
		if m.Focus() != FocusLauncher || m.appsSent != 1 {
			t.Fatalf("focus %v, app refreshes %d", m.Focus(), m.appsSent)
		}
	})
}

// A pending refresh holds no command lifecycle state: typing filters, moving
// the selection and leaving still take effect before its answer arrives.
func TestAppsRefreshLeavesInputResponsive(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.clock", "Clock"),
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF2}); cmd == nil {
		t.Fatal("F2 returned no command")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "Echo"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.launcherQuery != "Echo" || m.Focus() != FocusLauncher {
		t.Fatalf("filter %q focus %v before the refresh answered", m.launcherQuery, m.Focus())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusPrompt {
		t.Fatalf("leaving Apps with a refresh pending left focus %v", m.Focus())
	}
}

// An accepted refresh selects the same app by ID, even when the runtime
// returns the shelf in another order or with other PIDs.
func TestAppListRefreshKeepsSelectionByIdentity(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.notes", "Notes"),
		runningApp("com.gostalgia.clock", "Clock", 31),
	}
	m.setMode(modeLauncher)
	m.selected = 1
	if got := selectedAppID(m); got != "com.gostalgia.notes" {
		t.Fatalf("selected %q before refresh", got)
	}

	reordered := []appStatus{
		runningApp("com.gostalgia.clock", "Clock", 31),
		runningApp("com.gostalgia.echo", "Echo", 9),
		readyApp("com.gostalgia.notes", "Notes"),
	}
	m.Update(appListMsg{seq: m.appsSent, apps: reordered})
	if got := selectedAppID(m); got != "com.gostalgia.notes" {
		t.Fatalf("selected %q after refresh, want Notes by ID", got)
	}
	if m.apps[1].PID != 9 {
		t.Fatalf("Echo PID = %d, want refreshed 9", m.apps[1].PID)
	}
}

// Overlapping refreshes: only the newest request's answer may apply. An
// older snapshot cannot restore a stopped app's old PID, and it cannot
// change the screen the user is on.
func TestAppListRefreshRejectsStaleReplies(t *testing.T) {
	live := []appStatus{runningApp("com.gostalgia.echo", "Echo", 1)}
	stopped := []appStatus{readyApp("com.gostalgia.echo", "Echo")}

	m := New(context.Background(), noopCaller{}, nil)
	m.apps = live
	m.setMode(modeLauncher)
	m.appsSent = 2 // two refreshes went out

	m.Update(appListMsg{seq: 2, apps: stopped})
	if m.apps[0].Running || m.apps[0].PID != 0 {
		t.Fatalf("newest answer did not show Echo stopped: %+v", m.apps[0])
	}

	m.setMode(modePrompt)
	m.Update(appListMsg{seq: 1, apps: live})
	if m.apps[0].Running || m.apps[0].PID != 0 {
		t.Fatalf("older answer restored stale LIVE state: %+v", m.apps[0])
	}
	if m.Focus() != FocusPrompt {
		t.Fatalf("stale answer changed the screen to %v", m.Focus())
	}

	// An older answer that arrives while the newer request is still pending
	// does not replace the last accepted state either.
	m.setMode(modeLauncher)
	m.apps = stopped
	m.Update(appListMsg{seq: 2, apps: stopped})
	m.Update(appListMsg{seq: 1, apps: live})
	if m.apps[0].Running {
		t.Fatal("pending-refresh stale answer restored LIVE")
	}
}

// A successful empty list is data: the shelf shows its empty state. A failed
// refresh keeps the last accepted cards and reports the failure readably.
func TestAppListRefreshEmptyAndFailure(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = []appStatus{runningApp("com.gostalgia.echo", "Echo", 1)}
	m.setMode(modeLauncher)

	m.Update(appListMsg{seq: m.appsSent, apps: []appStatus{}})
	if len(m.apps) != 0 || len(m.filteredApps()) != 0 {
		t.Fatalf("empty answer left %d apps", len(m.apps))
	}
	if body := strings.Join(m.renderLauncher(80, 10), "\n"); !strings.Contains(body, "No apps installed") {
		t.Fatalf("empty shelf missing empty state:\n%s", body)
	}

	m.apps = []appStatus{runningApp("com.gostalgia.echo", "Echo", 1)}
	m.Update(appListMsg{seq: m.appsSent, err: errors.New("app/list: socket closed")})
	if len(m.apps) != 1 || !m.apps[0].Running || m.apps[0].PID != 1 {
		t.Fatalf("failed refresh changed the shelf: %+v", m.apps)
	}
	found := false
	for _, e := range m.transcript {
		found = found || strings.Contains(e.text, "Could not refresh apps: app/list: socket closed")
	}
	if !found {
		t.Fatal("failed refresh left no readable transcript feedback")
	}
	if toasts := m.notifs.ActiveToasts(); len(toasts) != 1 || toasts[0].Title != "Shelf refresh failed" {
		t.Fatalf("failed refresh toasts = %+v", toasts)
	}
}

// The refresh command asks app/list with a new sequence number and returns a
// dedicated message, not a command result.
func TestAppsRefreshCommandCarriesItsOwnMessage(t *testing.T) {
	caller := &appListCaller{replies: []appListReply{{apps: []appStatus{readyApp("com.gostalgia.echo", "Echo")}}}}
	m := New(context.Background(), caller, nil)

	cmd := m.refreshAppsCmd()
	msg, ok := cmd().(appListMsg)
	if !ok {
		t.Fatalf("refresh returned %T", msg)
	}
	if msg.seq != 1 || m.appsSent != 1 {
		t.Fatalf("sequence = %d, sent = %d", msg.seq, m.appsSent)
	}
	m.Update(msg)
	if len(m.apps) != 1 || m.apps[0].Manifest.ID != "com.gostalgia.echo" || m.apps[0].Running {
		t.Fatalf("applied shelf = %+v", m.apps)
	}
	if caller.asked != 1 {
		t.Fatalf("app/list asked %d times", caller.asked)
	}
}
