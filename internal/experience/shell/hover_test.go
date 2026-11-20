package shell

import (
	"image"
	"os"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/ui"
)

// hoverPoint is the stable pointer target a class offers at 80x24.
type hoverPoint struct {
	class string
	point image.Point
	// setup prepares the screen and returns the keyboard state that hover
	// must not change.
	setup func(t *testing.T, m *Model) func() any
	// cue is the stripped screen text a hover must add.
	cue string
}

// hoverClasses drives all six clickable populations named by the contract.
func hoverClasses() []struct {
	name string
	run  func(t *testing.T, m *Model) hoverPoint
} {
	return []struct {
		name string
		run  func(t *testing.T, m *Model) hoverPoint
	}{
		{"launcher card", func(t *testing.T, m *Model) hoverPoint {
			m.setMode(modeLauncher)
			r := rectOf(t, m, launcherCard("com.gostalgia.notes"))
			return hoverPoint{
				class: "launcher card",
				point: image.Pt(r.Min.X+r.Dx()/2, r.Min.Y+1),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any { return m.selected }
				},
				cue: "~ Notes",
			}
		}},
		{"header tab", func(t *testing.T, m *Model) hoverPoint {
			m.setMode(modeHome)
			tabs, active := m.tabs()
			r := rectOf(t, m, slotTabs)
			var span ui.TabSpan
			found := false
			for _, s := range m.kit.TabSpans(tabs, active, r.Dx()) {
				if tabs[s.Index].Label == "Tasks" {
					span, found = s, true
					break
				}
			}
			if !found {
				t.Fatal("the Tasks tab is not visible")
			}
			return hoverPoint{
				class: "header tab",
				point: image.Pt(r.Min.X+span.X+span.Width/2, r.Min.Y),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any { return m.currentMode() }
				},
				cue: "~Tasks~",
			}
		}},
		{"palette item", func(t *testing.T, m *Model) hoverPoint {
			m.openPalette()
			r := rectOf(t, m, layerPalette)
			p, ok := findText(m, r, "help")
			if !ok {
				t.Fatal("the help palette item is not visible")
			}
			return hoverPoint{
				class: "palette item",
				point: image.Pt(p.X+r.Dx()/2, p.Y),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any { return m.paletteSelected }
				},
				cue: "~ [CMD] help",
			}
		}},
		{"Home row", func(t *testing.T, m *Model) hoverPoint {
			m.setMode(modeHome)
			r := rectOf(t, m, slotBody)
			p, ok := findText(m, r, "Clock")
			if !ok {
				t.Fatal("the Clock Home row is not visible")
			}
			return hoverPoint{
				class: "Home row",
				point: image.Pt(p.X+1, p.Y),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any { return m.homeSelected }
				},
				cue: "~ Clock",
			}
		}},
		{"Task Manager row", func(t *testing.T, m *Model) hoverPoint {
			m.taskView = true
			r := rectOf(t, m, slotBody)
			p, ok := findText(m, r, "com.gostalgia.echo")
			if !ok {
				t.Fatal("the Echo process row is not visible")
			}
			return hoverPoint{
				class: "Task Manager row",
				point: image.Pt(p.X, p.Y),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any {
						if p := m.tasks.SelectedProcess(); p != nil {
							return p.ID
						}
						return int32(0)
					}
				},
				cue: "~ 7",
			}
		}},
		{"app grid cell", func(t *testing.T, m *Model) hoverPoint {
			m.presentation = &appView{data: padData(), instance: "launch", version: 2,
				values: map[string]string{"text": "draft"}, focus: 2}
			r := rectOf(t, m, slotBody)
			p, ok := findText(m, r, "NE")
			if !ok {
				t.Fatal("the NE grid cell is not visible")
			}
			return hoverPoint{
				class: "app grid cell",
				point: image.Pt(p.X, p.Y),
				setup: func(t *testing.T, m *Model) func() any {
					return func() any { return m.presentation.cell }
				},
				cue: "<NE>",
			}
		}},
	}
}

// TestHoverCuesOnAllSixTargetClasses checks each named population in every
// built-in theme and Plain, with hover distinct from the normal and selected
// states, a cleared cue on leave, and unchanged keyboard selection and
// geometry.
func TestHoverCuesOnAllSixTargetClasses(t *testing.T) {
	for _, tc := range headerBadgeCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, class := range hoverClasses() {
				t.Run(class.name, func(t *testing.T) {
					m := sized(fixtureModel(tc.th, tc.mode), 80, 24)
					hp := class.run(t, m)
					before := hp.setup(t, m)
					state := before()
					normal := strings.Join(screenText(m.View()), "\n")
					if strings.Contains(normal, hp.cue) {
						t.Fatalf("%s cue is already shown before hover:\n%s", hp.class, normal)
					}

					mouse(t, m, tea.MouseMotionMsg{X: hp.point.X, Y: hp.point.Y})
					hovered := strings.Join(screenText(m.View()), "\n")
					if !strings.Contains(hovered, hp.cue) {
						t.Fatalf("%s hover cue %q missing:\n%s", hp.class, hp.cue, hovered)
					}
					if got := before(); got != state {
						t.Fatalf("%s hover changed keyboard state from %v to %v", hp.class, state, got)
					}
					if tc.mode == ui.Plain && strings.Contains(m.View().Content, "\x1b") {
						t.Fatalf("%s Plain hover emitted escapes", hp.class)
					}
					// The cue is noncolor vocabulary: the stripped text
					// carries it in every theme and mode.
					if !strings.Contains(ansi.Strip(m.View().Content), hp.cue) {
						t.Fatalf("%s cue is not readable without color", hp.class)
					}

					// Leaving ends the cue without moving selection.
					mouse(t, m, tea.MouseMotionMsg{X: 0, Y: 0})
					left := strings.Join(screenText(m.View()), "\n")
					if strings.Contains(left, hp.cue) {
						t.Fatalf("%s cue outlived the pointer:\n%s", hp.class, left)
					}
					if got := before(); got != state {
						t.Fatalf("%s leave changed keyboard state from %v to %v", hp.class, state, got)
					}
				})
			}
		})
	}
}

// TestHoverDoesNotChangeMeasurements pins the layout the pointer cue is
// drawn into: cards, tab spans and grid targets keep their rectangles.
func TestHoverDoesNotChangeMeasurements(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	m.setMode(modeLauncher)
	echo := rectOf(t, m, launcherCard("com.gostalgia.echo"))
	notes := rectOf(t, m, launcherCard("com.gostalgia.notes"))
	tabs, active := m.tabs()
	tabsBefore := m.kit.TabSpans(tabs, active, rectOf(t, m, slotTabs).Dx())
	mouse(t, m, tea.MouseMotionMsg{X: notes.Min.X + 2, Y: notes.Min.Y + 1})
	if got := rectOf(t, m, launcherCard("com.gostalgia.echo")); got != echo {
		t.Fatalf("hover moved the Echo card from %v to %v", echo, got)
	}
	if got := rectOf(t, m, launcherCard("com.gostalgia.notes")); got != notes {
		t.Fatalf("hover moved the Notes card from %v to %v", notes, got)
	}
	tabs, active = m.tabs()
	if got := m.kit.TabSpans(tabs, active, rectOf(t, m, slotTabs).Dx()); !sameSpans(got, tabsBefore) {
		t.Fatalf("hover changed tab spans from %v to %v", tabsBefore, got)
	}

	// A grid cell keeps its target rectangle and every cell's x.
	m.presentation = &appView{data: padData(), instance: "launch", version: 2,
		values: map[string]string{"text": "draft"}, focus: 2}
	body := rectOf(t, m, slotBody)
	p, ok := findText(m, body, "NE")
	if !ok {
		t.Fatal("the NE grid cell is not visible")
	}
	_, beforeTargets := m.viewLayout(body.Dy())
	mouse(t, m, tea.MouseMotionMsg{X: p.X, Y: p.Y})
	_, afterTargets := m.viewLayout(body.Dy())
	if len(beforeTargets) != len(afterTargets) {
		t.Fatalf("hover changed the grid target count from %d to %d", len(beforeTargets), len(afterTargets))
	}
	for i := range beforeTargets {
		if beforeTargets[i] != afterTargets[i] {
			t.Fatalf("hover changed grid target %d from %+v to %+v", i, beforeTargets[i], afterTargets[i])
		}
	}
}

func sameSpans(a, b []ui.TabSpan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHoverClearsOnLeaveNavigationResizeReplacementAndMouseOff covers the
// lifecycle conditions in the contract, including a stale leave callback.
func TestHoverClearsOnLeaveNavigationResizeReplacementAndMouseOff(t *testing.T) {
	t.Run("leave", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeLauncher)
		r := rectOf(t, m, launcherCard("com.gostalgia.notes"))
		mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y + 1})
		if m.hover.kind != hoverLauncherCard || m.hover.id != "com.gostalgia.notes" {
			t.Fatalf("hover = %+v, want the Notes card", m.hover)
		}
		mouse(t, m, tea.MouseMotionMsg{X: 0, Y: 0})
		if m.hover.valid() {
			t.Fatalf("hover survived leave: %+v", m.hover)
		}
	})

	t.Run("navigation", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeLauncher)
		r := rectOf(t, m, launcherCard("com.gostalgia.notes"))
		mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y + 1})
		act(t, m, key(tea.KeyF1))
		if m.currentMode() != modeHome {
			t.Fatalf("F1 left mode %v", m.currentMode())
		}
		if m.hover.valid() {
			t.Fatalf("hover survived navigation: %+v", m.hover)
		}
	})

	t.Run("resize", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeHome)
		r := rectOf(t, m, slotBody)
		p, ok := findText(m, r, "Clock")
		if !ok {
			t.Fatal("Clock row is not visible")
		}
		mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
		if m.hover.kind != hoverHomeRow {
			t.Fatalf("hover = %+v, want a Home row", m.hover)
		}
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 23})
		if m.hover.valid() {
			t.Fatalf("hover survived resize: %+v", m.hover)
		}
	})

	t.Run("replacement", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeLauncher)
		r := rectOf(t, m, launcherCard("com.gostalgia.notes"))
		mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y + 1})
		if m.hover.kind != hoverLauncherCard {
			t.Fatalf("hover = %+v, want the Notes card", m.hover)
		}
		m.Update(appListMsg{seq: m.appsSent, apps: []appStatus{
			runningApp("com.gostalgia.echo", "Echo", 7),
			readyApp("com.gostalgia.notes", "Notes"),
		}})
		if m.hover.valid() {
			t.Fatalf("hover survived app list replacement: %+v", m.hover)
		}
	})

	t.Run("task row replacement", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.taskView = true
		r := rectOf(t, m, slotBody)
		p, ok := findText(m, r, "com.gostalgia.echo")
		if !ok {
			t.Fatal("Echo row is not visible")
		}
		mouse(t, m, tea.MouseMotionMsg{X: p.X, Y: p.Y})
		if m.hover.kind != hoverTaskRow || m.hover.id != "7" {
			t.Fatalf("hover = %+v, want the Echo process row", m.hover)
		}
		// The new process list no longer has PID 7; its cue must not move
		// to whatever row now sits under the pointer.
		m.handleProcessUpdates([]taskmanager.ProcessRow{{ID: 1, Name: "environment", State: "running"}})
		if m.hover.valid() {
			t.Fatalf("hover survived task row replacement: %+v", m.hover)
		}
	})

	t.Run("task row shift", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.taskView = true
		r := rectOf(t, m, slotBody)
		p, ok := findText(m, r, "com.gostalgia.echo")
		if !ok {
			t.Fatal("Echo row is not visible")
		}
		mouse(t, m, tea.MouseMotionMsg{X: p.X, Y: p.Y})
		if m.hover.kind != hoverTaskRow || m.hover.id != "7" {
			t.Fatalf("hover = %+v, want the Echo process row", m.hover)
		}
		// Echo survives, but losing the earlier PID moves it up a row.
		m.handleProcessUpdates([]taskmanager.ProcessRow{{ID: 7, Name: "com.gostalgia.echo", State: "running"}})
		if m.hover.valid() {
			t.Fatalf("hover survived a task row shift: %+v", m.hover)
		}
	})

	t.Run("mouse off", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeHome)
		r := rectOf(t, m, slotBody)
		p, ok := findText(m, r, "Clock")
		if !ok {
			t.Fatal("Clock row is not visible")
		}
		mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
		if !m.hover.valid() {
			t.Fatal("no hover was set before mouse off")
		}
		m.setMouse(false)
		if m.hover.valid() {
			t.Fatalf("mouse off kept hover %+v", m.hover)
		}
		if v := m.View(); v.MouseMode != tea.MouseModeNone {
			t.Fatalf("mouse off asks for %v", v.MouseMode)
		}
		// A report from a frame drawn before the setting changed does
		// nothing, and cannot restore the cue.
		m.Update(tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
		if m.hover.valid() {
			t.Fatalf("stale mouse report restored hover %+v", m.hover)
		}
	})

	t.Run("stale leave identity", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.setMode(modeLauncher)
		m.setHover(hoverLauncherCard, "com.gostalgia.notes")
		m.clearHover(hoverLauncherCard, "com.gostalgia.echo")
		if m.hover.id != "com.gostalgia.notes" {
			t.Fatalf("an old leave cleared the current target: %+v", m.hover)
		}
		m.clearHover(hoverTab, "com.gostalgia.notes")
		if m.hover.id != "com.gostalgia.notes" {
			t.Fatalf("a leave from another class cleared the target: %+v", m.hover)
		}
		m.clearHover(hoverLauncherCard, "com.gostalgia.notes")
		if m.hover.valid() {
			t.Fatalf("the matching leave did not clear: %+v", m.hover)
		}
	})
}

// TestHoverRespectsPaletteAndDialogBoundaries keeps protected pointer and
// keyboard behavior: a modal layer blocks the cue and the click under it.
func TestHoverRespectsPaletteAndDialogBoundaries(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	m.setMode(modeHome)
	body := rectOf(t, m, slotBody)
	p, ok := findText(m, body, "Echo")
	if !ok {
		t.Fatal("Echo row is not visible")
	}

	m.openPalette()
	mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
	if m.hover.valid() {
		t.Fatalf("hover showed under the palette: %+v", m.hover)
	}
	if strings.Contains(strings.Join(screenText(m.View()), "\n"), "~ Echo") {
		t.Fatal("a Home hover cue was drawn under the palette")
	}
	clickAt(t, m, p)
	if m.presentation != nil || m.currentMode() != modeHome {
		t.Fatal("a click under the palette reached the Home row")
	}
	m.closePalette()

	m.openConfirm(confirmation{kind: confirmShutdown})
	mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
	if m.hover.valid() {
		t.Fatalf("hover showed under the dialog: %+v", m.hover)
	}
	if strings.Contains(strings.Join(screenText(m.View()), "\n"), "~ Echo") {
		t.Fatal("a Home hover cue was drawn under the dialog")
	}
	m.confirm = nil
}

// TestHeaderTabHoverMatchesHitGeometryAndEntry acknowledges the pointer cue
// and the tab's rendered span through the same coordinates, and checks that
// entering the center clears the unseen badge without touching severity.
func TestHeaderTabHoverMatchesHitGeometryAndEntry(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	if got := m.notifs.UnseenCount(); got != 2 {
		t.Fatalf("fixture has %d unseen, want 2", got)
	}
	homeTabs, homeActive := m.tabs()
	r := rectOf(t, m, slotTabs)
	var span ui.TabSpan
	for _, s := range m.kit.TabSpans(homeTabs, homeActive, r.Dx()) {
		if strings.HasPrefix(homeTabs[s.Index].Label, "Notifications") {
			span = s
			break
		}
	}
	p := image.Pt(r.Min.X+span.X+span.Width/2, r.Min.Y)
	labelAt, ok := findText(m, r, "Notifications")
	if !ok || labelAt.X < r.Min.X+span.X || labelAt.X >= r.Min.X+span.X+span.Width {
		t.Fatalf("tab span %v does not contain slot %v and label at %v", span, r, labelAt)
	}

	mouse(t, m, tea.MouseMotionMsg{X: p.X, Y: p.Y})
	if m.hover.kind != hoverTab || m.hover.id != "notifications" {
		t.Fatalf("hover tab = %+v, want notifications", m.hover)
	}
	if m.currentMode() != modeHome {
		t.Fatalf("hover moved the screen to %v", m.currentMode())
	}
	if screen := strings.Join(screenText(m.View()), "\n"); !strings.Contains(screen, "~Notifications (2)~") {
		t.Fatalf("hovered tab cue missing:\n%s", screen)
	}

	clickAt(t, m, p)
	if !m.notifView {
		t.Fatal("the header tab did not open the Notification Center")
	}
	if got := m.notifs.UnseenCount(); got != 0 {
		t.Fatalf("entry left %d unseen", got)
	}
	if m.hover.valid() {
		t.Fatalf("entry kept the tab cue: %+v", m.hover)
	}
}

// TestBadgeSeverityHoverCrossflow is VAL-BADGE-003's ordered flow: inspect
// the existing population, receive a new ERROR, hover Home, switch theme,
// render, then enter the center through the header tab.
func TestBadgeSeverityHoverCrossflow(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	act(t, m, key(tea.KeyF6))
	if got := m.notifs.UnseenCount(); got != 0 {
		t.Fatalf("initial inspection left %d unseen", got)
	}
	act(t, m, key(tea.KeyF1)) // back to Home
	if m.currentMode() != modeHome {
		t.Fatalf("F1 left mode %v", m.currentMode())
	}

	arrival := notifications.Notification{ID: "err-1", Timestamp: fixtureTime, Title: "Disk full", Message: "write failed",
		Level: notifications.LevelError, Source: "process", Badge: "IO"}
	m.notifs.Record(arrival)
	if got := m.notifs.UnseenCount(); got != 1 {
		t.Fatalf("new ERROR left %d unseen, want 1", got)
	}
	if header := headerRow(t, m); !strings.Contains(header, "Notifications (1)") {
		t.Fatalf("header %q lacks the new unseen count", header)
	}
	if screen := strings.Join(screenText(m.View()), "\n"); !strings.Contains(screen, "ERROR") {
		t.Fatalf("the new toast does not carry ERROR severity:\n%s", screen)
	}

	body := rectOf(t, m, slotBody)
	p, ok := findText(m, body, "Clock")
	if !ok {
		t.Fatal("Clock row is not visible")
	}
	mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
	if m.hover.kind != hoverHomeRow || !strings.Contains(strings.Join(screenText(m.View()), "\n"), "~ Clock") {
		t.Fatalf("Home hover is not coherent: %+v", m.hover)
	}

	if !m.SetThemeByName("midnight") {
		t.Fatal("midnight is not a built-in theme")
	}
	for range 2 {
		m.View()
	}
	if got := m.notifs.UnseenCount(); got != 1 {
		t.Fatalf("theme switch or rendering acknowledged the arrival: %d unseen", got)
	}
	var level notifications.Level
	seen := false
	for _, n := range m.notifs.History() {
		if n.ID == "err-1" {
			level, seen = n.Level, n.Seen
		}
	}
	if level != notifications.LevelError || seen {
		t.Fatalf("arrival changed to level %v seen=%v", level, seen)
	}
	if got := m.runningAppCount(); got != 2 {
		t.Fatalf("running app count changed to %d", got)
	}

	// Enter through the header at the rendered tab span.
	tabs, active := m.tabs()
	r := rectOf(t, m, slotTabs)
	var span ui.TabSpan
	for _, s := range m.kit.TabSpans(tabs, active, r.Dx()) {
		if strings.HasPrefix(tabs[s.Index].Label, "Notifications") {
			span = s
			break
		}
	}
	clickAt(t, m, image.Pt(r.Min.X+span.X+span.Width/2, r.Min.Y))
	if !m.notifView {
		t.Fatal("the header tab did not enter the center")
	}
	if got := m.notifs.UnseenCount(); got != 0 {
		t.Fatalf("entry left %d unseen", got)
	}
	for _, n := range m.notifs.History() {
		if n.ID == "err-1" && (!n.Seen || n.Level != notifications.LevelError) {
			t.Fatalf("entry changed the arrival: level %v seen %v", n.Level, n.Seen)
		}
	}
	if got := m.runningAppCount(); got != 2 {
		t.Fatalf("entry changed the running app count to %d", got)
	}
	if m.hover.valid() {
		t.Fatalf("entry kept a pointer cue: %+v", m.hover)
	}
}

// TestHoverCrossflowSnapshot records the VAL-BADGE-003 stages in order for
// the theme-switched and entry states.
func TestHoverCrossflowSnapshot(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	var b strings.Builder
	record := func(label string) {
		b.WriteString(label + "\n")
		b.WriteString(headerRow(t, m) + "\n")
		b.WriteString(strings.Join(screenText(m.View()), "\n") + "\n\n")
	}
	act(t, m, key(tea.KeyF6))
	act(t, m, key(tea.KeyF1))
	record("AFTER INSPECTION")

	m.notifs.Record(notifications.Notification{ID: "err-1", Timestamp: fixtureTime, Title: "Disk full",
		Message: "write failed", Level: notifications.LevelError, Source: "process"})
	record("NEW ERROR ARRIVAL")

	body := rectOf(t, m, slotBody)
	p, ok := findText(m, body, "Clock")
	if !ok {
		t.Fatal("Clock row is not visible")
	}
	mouse(t, m, tea.MouseMotionMsg{X: p.X + 1, Y: p.Y})
	record("HOME ROW HOVER")

	m.SetThemeByName("midnight")
	record("THEME SWITCH")

	tabs, active := m.tabs()
	r := rectOf(t, m, slotTabs)
	for _, s := range m.kit.TabSpans(tabs, active, r.Dx()) {
		if strings.HasPrefix(tabs[s.Index].Label, "Notifications") {
			clickAt(t, m, image.Pt(r.Min.X+s.X+s.Width/2, r.Min.Y))
			break
		}
	}
	record("CENTER ENTRY")

	path := "testdata/hover-badge-crossflow.snapshot"
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
		t.Fatalf("hover and badge crossflow snapshot changed:\n%s", b.String())
	}
}
