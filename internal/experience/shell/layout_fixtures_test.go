package shell

import (
	"context"
	"fmt"
	"time"

	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/receipts"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/platform"
)

// geometryScreen is one keyboard-reachable screen with real fixtures behind
// it: populated apps, documents, processes, logs, a receipt, notifications
// and an app presentation, so no screen is a mode label over empty data.
type geometryScreen struct {
	name  string
	setup func(*Model)
}

var fixtureTime = time.Date(2025, 3, 14, 15, 9, 26, 0, time.UTC)

func fixtureApp(id, name string, running bool, pid int32) appStatus {
	var a appStatus
	a.Manifest.ID, a.Manifest.Name, a.Manifest.Version = id, name, "1.2.0"
	a.Manifest.Description = "Fixture app " + name + " with a description long enough to clip."
	a.Manifest.Permissions = []string{"ipc", "files"}
	a.Running, a.PID = running, pid
	return a
}

func fixtureReceipt() receipts.CrashReceipt {
	r := receipts.New(31, "clock", "failed", 2, "exit status 2", 3, "tick\ntock\npanic: wound too tight")
	r.ID, r.Timestamp = "CR-31-fixture", fixtureTime
	return r
}

// fixtureModel is a shell populated through its public state the way a
// running session populates it.
func fixtureModel(th theme.Theme, mode ui.ColorMode) *Model {
	notifs := notifications.NewManager(100, 3, 8)
	m := NewWithThemeAndNotifs(context.Background(), noopCaller{}, nil, th, mode, notifs)
	m.apps = []appStatus{
		fixtureApp("com.gostalgia.echo", "Echo", true, 7),
		fixtureApp("com.gostalgia.notes", "Notes", false, 0),
		fixtureApp("com.gostalgia.clock", "Clock", true, 31),
	}
	m.homeData = homeData{
		UptimeSeconds: 3725, User: "guest", ProcessCount: 4, ServicesCount: 2,
		Documents: []docShortcut{{Name: "readme.txt", Path: "/users/guest/documents/readme.txt", Size: 120},
			{Name: "todo.txt", Path: "/users/guest/documents/todo.txt", Size: 48}},
	}
	usage := platform.ResourceUsage{Supported: true, MemoryBytes: 12 << 20, CPUUserMs: 840, CPUSysMs: 410}
	m.tasks.SetProcesses([]taskmanager.ProcessRow{
		{ID: 1, Name: "environment", Kind: "inproc", State: "running", Resources: usage},
		{ID: 7, Name: "com.gostalgia.echo", Kind: "child", State: "running", RestartCount: 1, Resources: usage},
		{ID: 12, Name: "indexer-with-a-very-long-name", Kind: "inproc", State: "restarting", RestartCount: 4},
		{ID: 31, Name: "com.gostalgia.clock", Kind: "child", State: "failed", ExitCode: 2, RestartCount: 3, CrashLoop: true},
	})
	receipt := fixtureReceipt()
	notifs.Record(notifications.Notification{ID: "n1", Timestamp: fixtureTime, Title: "clock Crashed",
		Message: "PID 31 exit 2 (failed)", Level: notifications.LevelError, Source: "process", PID: 31, Receipt: &receipt})
	notifs.Record(notifications.Notification{ID: "n2", Timestamp: fixtureTime.Add(time.Minute), Title: "indexer Backoff",
		Message: "PID 12 backoff restart 4", Level: notifications.LevelWarning, Source: "process", PID: 12})
	// A settled session: the alerts stay unread history, but their toasts
	// have expired, so screens show their own content. Toasts overlay the
	// body without changing its layout; withToasts and the toast tests
	// cover them.
	for range 8 {
		notifs.Tick()
	}
	for _, e := range []entry{{"> apps", "command"}, {"Echo, Notes and Clock are installed.", ""},
		{"Unicode e\u0301 界 👩‍💻 survives wrapping.", "muted"}} {
		m.append(e)
	}
	return m
}

// withToasts records n fresh process alerts, each with an active toast.
func withToasts(m *Model, n int) *Model {
	for i := range n {
		m.notifs.Record(notifications.Notification{ID: fmt.Sprintf("toast-%d", i), Timestamp: fixtureTime,
			Title: fmt.Sprintf("worker-%d Crashed", i), Message: "exit 1", Level: notifications.LevelError, Source: "process"})
	}
	return m
}

func geometryScreens() []geometryScreen {
	base := []geometryScreen{
		{"home", func(m *Model) { m.setMode(modeHome) }},
		{"prompt", func(m *Model) {
			m.setMode(modePrompt)
			m.input = []rune("echo he\u0301llo 界 👩‍💻 world")
			m.cursor = 8
		}},
		{"launcher", func(m *Model) { m.setMode(modeLauncher); m.selected = 2 }},
		{"app", func(m *Model) {
			m.presentation = &appView{data: padData(), instance: "launch", version: 2,
				values: map[string]string{"text": "draft"}, focus: 2, cell: 1}
		}},
		{"tasks-table", func(m *Model) { m.taskView = true; m.tasks.SelectNext() }},
		{"tasks-logs", func(m *Model) {
			m.taskView = true
			m.tasks.ShowLogs(7, "com.gostalgia.echo", "boot\nlistening on ipc\nhello 界\nbye")
		}},
		{"tasks-receipt", func(m *Model) {
			m.taskView = true
			r := fixtureReceipt()
			m.tasks.ShowReceipt(&r)
		}},
		{"notifications", func(m *Model) { m.notifView = true }},
	}
	screens := append([]geometryScreen(nil), base...)
	for _, s := range base {
		setup := s.setup
		screens = append(screens, geometryScreen{"palette-from-" + s.name, func(m *Model) {
			setup(m)
			m.openPalette()
		}})
	}
	return screens
}
