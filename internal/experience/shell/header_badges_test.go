package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// headerRow is the tab row as it is drawn, stripped of styling.
func headerRow(t *testing.T, m *Model) string {
	t.Helper()
	r := rectOf(t, m, slotTabs)
	lines := screenText(m.View())
	if r.Min.Y < 0 || r.Min.Y >= len(lines) {
		t.Fatalf("tab row %v outside %d rendered rows", r, len(lines))
	}
	return lines[r.Min.Y]
}

// headerBadgeCases cover the five built-in themes and Plain separately.
// Monochrome renders Plain like the rest of the existing shell snapshots.
func headerBadgeCases() []struct {
	name string
	th   theme.Theme
	mode ui.ColorMode
} {
	return []struct {
		name string
		th   theme.Theme
		mode ui.ColorMode
	}{
		{"nostalgia", theme.Nostalgia(), ui.ANSI256},
		{"midnight", theme.Midnight(), ui.ANSI256},
		{"monochrome", theme.Monochrome(), ui.Plain},
		{"high-contrast", theme.HighContrast(), ui.ANSI256},
		{"high-contrast-light", theme.HighContrastLight(), ui.ANSI256},
		{"plain", theme.Nostalgia(), ui.Plain},
	}
}

// The fixture has two running apps, one stopped app, and unrelated services
// in its process table and status. The header counts apps only.
func TestHeaderCountsRunningAppsAndUnseenNotifications(t *testing.T) {
	for _, tc := range headerBadgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(fixtureModel(tc.th, tc.mode), 80, 24)
			if got := len(m.apps); got != 3 {
				t.Fatalf("fixture has %d apps, want 3", got)
			}
			header := headerRow(t, m)
			for _, want := range []string{"Apps (2)", "Notifications (2)"} {
				if !strings.Contains(header, want) {
					t.Fatalf("header %q lacks %q", header, want)
				}
			}
			for _, unwanted := range []string{"Apps (4)", "Apps (5)", "Apps (0)"} {
				if strings.Contains(header, unwanted) {
					t.Fatalf("header %q counted services or processes: %q", header, unwanted)
				}
			}
			if tc.mode == ui.Plain && strings.Contains(m.View().Content, "\x1b") {
				t.Fatal("Plain header contains escapes")
			}
			tabs, _ := m.tabs()
			for _, tab := range tabs {
				if !isASCIIText(tab.Label) {
					t.Fatalf("tab label %q does not carry an ASCII-readable count cue", tab.Label)
				}
			}
		})
	}
}

func isASCIIText(s string) bool {
	for _, r := range s {
		if r > 0x7e || r < 0x20 {
			return false
		}
	}
	return true
}

// An accepted app/list refresh changes the running-app count without asking
// the client for anything from the header or from rendering.
func TestHeaderRunningAppCountFollowsAcceptedRefresh(t *testing.T) {
	rec := &recordingCaller{}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.client = rec
	if header := headerRow(t, m); !strings.Contains(header, "Apps (2)") {
		t.Fatalf("header %q lacks the fixture's two running apps", header)
	}

	// Clock stopped out of band; only Echo is still running.
	refreshed := []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.notes", "Notes"),
		readyApp("com.gostalgia.clock", "Clock"),
	}
	m.Update(appListMsg{seq: m.appsSent, apps: refreshed})
	if header := headerRow(t, m); !strings.Contains(header, "Apps (1)") || strings.Contains(header, "Apps (2)") {
		t.Fatalf("header %q did not follow the accepted refresh", header)
	}

	m.Update(appListMsg{seq: m.appsSent, apps: []appStatus{}})
	if header := headerRow(t, m); strings.Contains(header, "Apps (") {
		t.Fatalf("empty registry kept a running count: %q", header)
	}

	// The header is read-only: rendering it sends no request and starts no
	// refresh sequence.
	for range 3 {
		m.View()
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("header rendering sent %v", calls)
	}
	if m.appsSent != 0 {
		t.Fatalf("header rendering sent %d app/list refreshes", m.appsSent)
	}
}

// A registered app's process row carries its app ID as the process name. The
// header follows the newest accepted process poll even while the user stays
// on Home, so an external stop or launch becomes visible without Apps.
func TestHeaderRunningAppCountFollowsAcceptedProcessPolls(t *testing.T) {
	rec := &recordingCaller{}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.client = rec
	if m.currentMode() != modeHome {
		t.Fatalf("start mode = %v, want Home", m.currentMode())
	}
	// Start stopped so every visible count change is the poll's, not the
	// fixture's stale app/list running flags.
	for i := range m.apps {
		m.apps[i].Running, m.apps[i].PID = false, 0
	}
	env := taskmanager.ProcessRow{ID: 1, Name: "environment", Kind: "inproc", State: "running"}
	indexer := taskmanager.ProcessRow{ID: 12, Name: "indexer-with-a-very-long-name", Kind: "child", State: "running"}
	echoRunning := taskmanager.ProcessRow{ID: 7, Name: "com.gostalgia.echo", Kind: "inproc", State: "running"}
	echoStopped := taskmanager.ProcessRow{ID: 7, Name: "com.gostalgia.echo", Kind: "inproc", State: "stopped"}
	notesStopped := taskmanager.ProcessRow{ID: 21, Name: "com.gostalgia.notes", Kind: "inproc", State: "stopped"}
	notesRunning := taskmanager.ProcessRow{ID: 42, Name: "com.gostalgia.notes", Kind: "inproc", State: "running"}
	clockRunning := taskmanager.ProcessRow{ID: 31, Name: "com.gostalgia.clock", Kind: "child", State: "running"}
	expectCount := func(want string, why string) {
		t.Helper()
		header := headerRow(t, m)
		if !strings.Contains(header, want) {
			t.Fatalf("%s: header %q lacks %q", why, header, want)
		}
	}

	// Echo runs next to a stopped registered app, a running service and an
	// unrelated running process. Only the app is counted.
	m.Update(procUpdateMsg{seq: 1, procs: []taskmanager.ProcessRow{env, echoRunning, indexer, notesStopped}})
	expectCount("Apps (1)", "accepted poll with one running app")

	// Another gctl client stops Echo. The next accepted poll clears the
	// count while the user remains on Home; Home's own app rows follow too.
	m.Update(procUpdateMsg{seq: 2, procs: []taskmanager.ProcessRow{env, echoStopped, indexer, notesStopped}})
	if header := headerRow(t, m); strings.Contains(header, "Apps (") {
		t.Fatalf("stopped app kept a running count: %q", header)
	}
	if view := screenText(m.View()); !strings.Contains(strings.Join(view, "\n"), "No applications running") {
		t.Fatalf("Home still lists a stopped app:\n%s", strings.Join(view, "\n"))
	}

	// A later external launch appears on the next accepted poll, including a
	// live PID in Home's row, without opening Apps or running a command.
	m.Update(procUpdateMsg{seq: 3, procs: []taskmanager.ProcessRow{env, echoStopped, indexer, notesStopped, notesRunning}})
	expectCount("Apps (1)", "external launch")
	view := strings.Join(screenText(m.View()), "\n")
	if !strings.Contains(view, "Notes") || !strings.Contains(view, "LIVE / PID 42") {
		t.Fatalf("Home lacks the externally launched app:\n%s", view)
	}

	// Two live rows for one registered app count once, and a second running
	// app adds one. Unrelated rows stay uncounted.
	m.Update(procUpdateMsg{seq: 4, procs: []taskmanager.ProcessRow{
		env, echoStopped, indexer, notesRunning, clockRunning,
		{ID: 43, Name: "com.gostalgia.notes", Kind: "inproc", State: "restarting"},
	}})
	expectCount("Apps (2)", "deduplicated process rows")

	// Failed and older samples keep the last known app state.
	before := headerRow(t, m)
	m.Update(procUpdateMsg{seq: 5, err: fmt.Errorf("proc/list: socket closed")})
	if got := headerRow(t, m); got != before {
		t.Fatalf("failed sample changed header %q to %q", before, got)
	}
	m.Update(procUpdateMsg{seq: 1, procs: []taskmanager.ProcessRow{env}})
	if got := headerRow(t, m); got != before {
		t.Fatalf("older sample changed header %q to %q", before, got)
	}

	// A fresh app/list answer outranks the older process-derived state until
	// the next accepted poll reconciles it.
	m.Update(appListMsg{seq: m.appsSent, apps: []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 55),
		readyApp("com.gostalgia.notes", "Notes"),
		readyApp("com.gostalgia.clock", "Clock"),
	}})
	expectCount("Apps (1)", "fresh app list after an older poll")
	m.Update(procUpdateMsg{seq: 6, procs: []taskmanager.ProcessRow{env, echoStopped}})
	if header := headerRow(t, m); strings.Contains(header, "Apps (") {
		t.Fatalf("newer poll kept the app list's old count: %q", header)
	}

	// The poll path is the only trigger: no app/list refresh and no request
	// came from the process updates or from rendering.
	for range 3 {
		m.View()
	}
	if calls := rec.all(); len(calls) != 0 {
		t.Fatalf("process-poll badge path sent %v", calls)
	}
	if m.appsSent != 0 {
		t.Fatalf("process-poll badge path sent %d app/list refreshes", m.appsSent)
	}
}

// enterNotificationCenter drives one existing route into the center.
func enterNotificationCenter(t *testing.T, m *Model, route string) {
	t.Helper()
	switch route {
	case "F6":
		act(t, m, key(tea.KeyF6))
	case "header tab":
		p, ok := findText(m, rectOf(t, m, slotTabs), "Notifications")
		if !ok {
			t.Fatalf("Notifications tab is not on screen:\n%s", strings.Join(screenText(m.View()), "\n"))
		}
		clickAt(t, m, p)
	case "notifications", "alerts":
		m.Update(execute(context.Background(), m.client, "/users/guest", route))
	default:
		t.Fatalf("unknown route %q", route)
	}
	if !m.notifView {
		t.Fatalf("%s did not open the Notification Center", route)
	}
}

// Entering the center marks the population already there seen, through every
// existing route. A later arrival stays unseen across repeated rendering
// until it is inspected.
func TestNotificationCenterEntryAcknowledgesOnlyExistingPopulation(t *testing.T) {
	for _, route := range []string{"F6", "header tab", "notifications", "alerts"} {
		t.Run(route, func(t *testing.T) {
			m, _ := mouseModel(t, 80, 24)
			if got := m.notifs.UnseenCount(); got != 2 {
				t.Fatalf("fixture has %d unseen, want 2", got)
			}
			enterNotificationCenter(t, m, route)
			if got := m.notifs.UnseenCount(); got != 0 {
				t.Fatalf("%s left %d existing notifications unseen", route, got)
			}
			for _, n := range m.notifs.History() {
				if !n.Seen {
					t.Fatalf("%s left %s unseen", route, n.ID)
				}
			}

			m.notifs.Record(notifications.Notification{ID: "C", Title: "later arrival",
				Message: "arrived while the center was open", Level: notifications.LevelInfo})
			if got := m.notifs.UnseenCount(); got != 1 {
				t.Fatalf("later arrival left %d unseen, want 1", got)
			}
			for range 3 {
				m.View()
			}
			if got := m.notifs.UnseenCount(); got != 1 {
				t.Fatalf("repeated rendering acknowledged the later arrival: %d unseen", got)
			}
			if view := m.View().Content; !strings.Contains(ansi.Strip(view), "Notifications (1)") {
				t.Fatalf("header does not show the later arrival:\n%s", ansi.Strip(view))
			}

			// Enter inspects the selected, newest arrival.
			act(t, m, enterKey)
			if got := m.notifs.UnseenCount(); got != 0 {
				t.Fatalf("inspecting the arrival left %d unseen", got)
			}
			for _, n := range m.notifs.History() {
				if n.ID == "C" && !n.Seen {
					t.Fatal("the inspected arrival is still unseen")
				}
			}
		})
	}
}

// A delayed command result arriving while the center is already open is not a
// second entry: it must not acknowledge an arrival that came after entry.
func TestNotificationCenterDelayedEntryReplyKeepsLaterArrivalUnseen(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	enterNotificationCenter(t, m, "F6")
	if got := m.notifs.UnseenCount(); got != 0 {
		t.Fatalf("entry left %d unseen", got)
	}
	m.notifs.Record(notifications.Notification{ID: "C", Title: "later arrival",
		Message: "still waiting for its command reply", Level: notifications.LevelInfo})
	m.Update(resultMsg{switchView: "notifications"})
	if got := m.notifs.UnseenCount(); got != 1 {
		t.Fatalf("a delayed entry reply acknowledged the arrival: %d unseen", got)
	}
}

// Header badge renderings stay pinned per theme, with Plain checked
// separately.
func TestHeaderBadgeSnapshots(t *testing.T) {
	for _, tc := range headerBadgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(fixtureModel(tc.th, tc.mode), 80, 24)
			row := strings.Split(m.View().Content, "\n")[rectOf(t, m, slotTabs).Min.Y]
			got := strings.TrimRight(row, " ") + "\n"
			path := filepath.Join("testdata", "header-badges", tc.name+".golden")
			if *updateVisual {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("header badge snapshot changed:\n%s", ansi.Strip(got))
			}
			if tc.mode == ui.Plain && strings.Contains(got, "\x1b") {
				t.Fatal("Plain header snapshot has escapes")
			}
			if !strings.Contains(ansi.Strip(got), "Apps (2)") || !strings.Contains(ansi.Strip(got), "Notifications (2)") {
				t.Fatalf("snapshot %q lacks both counts", ansi.Strip(got))
			}
		})
	}
}

// notifications-entry-and-arrival.snapshot records the header counts before
// entry, after entry, after a later arrival, and after it is inspected.
func TestNotificationCenterEntryLifecycleSnapshot(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	unseenIDs := func() string {
		var ids []string
		for _, n := range m.notifs.History() {
			if !n.Seen {
				ids = append(ids, n.ID)
			}
		}
		if len(ids) == 0 {
			return "(none)"
		}
		return strings.Join(ids, ",")
	}
	var b strings.Builder
	record := func(label string) {
		fmt.Fprintf(&b, "%s\nheader: %s\nunseen: %s\n", label, headerRow(t, m), unseenIDs())
	}
	record("BEFORE ENTRY")
	enterNotificationCenter(t, m, "F6")
	record("AFTER ENTRY")
	m.notifs.Record(notifications.Notification{ID: "C", Title: "later arrival",
		Message: "arrived while the center was open", Level: notifications.LevelInfo})
	record("AFTER C ARRIVES")
	act(t, m, enterKey)
	record("AFTER C INSPECTED")

	path := filepath.Join("testdata", "notifications-entry-and-arrival.snapshot")
	if *updateVisual {
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if b.String() != string(want) {
		t.Fatalf("entry/arrival snapshot changed:\n%s", b.String())
	}
}
