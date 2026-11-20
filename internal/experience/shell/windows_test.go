package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/windows"
)

// showWindow opens v as the focused window on the Windows screen, the state
// openWindow reaches once v's snapshot has arrived. A window already open
// for v's app is replaced.
func (m *Model) showWindow(v *appView) *appView {
	if v.id == "" {
		v.id = "com.test.window"
	}
	if v.values == nil {
		v.values = map[string]string{}
	}
	id := windows.ID(v.id)
	m.dropWindow(id)
	if err := m.windows.Open(windows.Window{ID: id, Title: viewText(v.data.Title), Bounds: m.windowBounds()}); err != nil {
		panic(err)
	}
	m.views[id] = v
	m.showWindows()
	return v
}

// windowApp is a running app whose window id is its app ID.
func windowApp(id string, pid int32) appStatus {
	var a appStatus
	a.Manifest.ID, a.Manifest.Name, a.Running, a.PID = id, strings.TrimPrefix(id, "com.test."), true, pid
	return a
}

// openLoaded opens a's window and delivers its first snapshot.
func openLoaded(m *Model, a appStatus) *appView {
	m.openWindow(a)
	v := m.views[windows.ID(a.Manifest.ID)]
	m.Update(viewMsg{id: windows.ID(a.Manifest.ID), epoch: v.epoch, data: screenData()})
	return v
}

func typeText(m *Model, s string) {
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: s})
}

func newWindowsModel() *Model {
	return New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		return writeJSON(out, screenData())
	}), nil)
}

func TestWindowsKeepIndependentState(t *testing.T) {
	m := newWindowsModel()
	a, b := windowApp("com.test.alpha", 1), windowApp("com.test.beta", 2)
	m.apps = []appStatus{a, b}
	va := openLoaded(m, a)
	typeText(m, "alpha")
	vb := openLoaded(m, b)
	typeText(m, "beta")
	if got := m.windows.Order(); len(got) != 2 || got[1] != "com.test.beta" || m.FocusedWindow() != "com.test.beta" {
		t.Fatalf("second window not open on top: order=%v focused=%q", got, m.FocusedWindow())
	}
	if !strings.Contains(m.View().Content, "Windows (2)") {
		t.Fatal("the Windows tab does not count both windows")
	}
	if va.values["text"] != "alpha" || vb.values["text"] != "beta" {
		t.Fatalf("drafts crossed windows: alpha=%q beta=%q", va.values["text"], vb.values["text"])
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyF8})
	if m.FocusedWindow() != "com.test.alpha" || m.focusedView().values["text"] != "alpha" {
		t.Fatal("F8 did not focus the other window with its draft")
	}
	typeText(m, "!")
	m.Update(tea.KeyPressMsg{Code: tea.KeyF8})
	if m.FocusedWindow() != "com.test.beta" || va.values["text"] != "alpha!" || vb.values["text"] != "beta" {
		t.Fatalf("F8 cycling lost input: alpha=%q beta=%q", va.values["text"], vb.values["text"])
	}

	// Each window answers only its own epoch, so a reply addressed to one
	// window but stamped with the other's epoch changes neither.
	fresh := screenData()
	fresh.Status = "alpha refreshed"
	m.Update(viewMsg{id: "com.test.alpha", epoch: vb.epoch, data: fresh})
	if va.data.Status == fresh.Status || vb.data.Status == fresh.Status {
		t.Fatal("a reply with another window's epoch was applied")
	}
	m.Update(viewMsg{id: "com.test.alpha", epoch: va.epoch, data: fresh})
	if va.data.Status != fresh.Status || vb.data.Status == fresh.Status {
		t.Fatal("a window's reply did not reach exactly that window")
	}
	if strings.Contains(m.View().Content, fresh.Status) {
		t.Fatal("a background window's snapshot drew over the focused one")
	}

	// Esc goes back to Home and keeps every window; F8 returns to them.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.onWindows() || m.Focus() != FocusHome || m.windows.Len() != 2 {
		t.Fatalf("Esc did not go back with the windows open: windows=%v focus=%v open=%d", m.onWindows(), m.Focus(), m.windows.Len())
	}
	typeText(m, "z")
	if vb.values["text"] != "beta" || m.shownScreen() != screenPrompt {
		t.Fatal("typing away from the windows did not go to the prompt")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF8})
	if !m.onWindows() || m.FocusedWindow() != "com.test.beta" || m.focusedView().values["text"] != "beta" {
		t.Fatal("F8 did not return to the Windows screen as it was left")
	}

	// Ctrl-W closes the focused window and focuses the next; closing the
	// last one goes back to the screen F8 left.
	m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if !m.onWindows() || m.FocusedWindow() != "com.test.alpha" || m.views["com.test.beta"] != nil {
		t.Fatalf("Ctrl-W did not hand focus to the remaining window: focused=%q", m.FocusedWindow())
	}
	m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if m.onWindows() || m.windows.Len() != 0 || len(m.views) != 0 || m.shownScreen() != screenPrompt {
		t.Fatalf("closing the last window did not go back: open=%d views=%d screen=%v", m.windows.Len(), len(m.views), m.shownScreen())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF8})
	if m.onWindows() || m.shownScreen() != screenPrompt {
		t.Fatal("F8 with no windows left the current screen")
	}
}

func TestWindowsLimitWarnsAndKeepsOpenWindows(t *testing.T) {
	m := newWindowsModel()
	for i := range maxWindows {
		openLoaded(m, windowApp(fmt.Sprintf("com.test.w%d", i), int32(10+i)))
	}
	if m.windows.Len() != maxWindows {
		t.Fatalf("%d windows open, want %d", m.windows.Len(), maxWindows)
	}
	focused := m.FocusedWindow()
	if cmd := m.openWindow(windowApp("com.test.extra", 99)); cmd != nil {
		t.Fatal("the refused window still fetched a view")
	}
	if m.windows.Len() != maxWindows || m.views["com.test.extra"] != nil || m.FocusedWindow() != focused {
		t.Fatal("a window past the limit opened or moved focus")
	}
	toasts := m.notifs.ActiveToasts()
	if len(toasts) != 1 || toasts[0].Title != "Too many windows" || toasts[0].Level != notifications.LevelWarning {
		t.Fatalf("limit toast = %+v", toasts)
	}
	// An app already open still raises its window at the limit.
	m.openWindow(windowApp("com.test.w0", 10))
	if m.FocusedWindow() != "com.test.w0" || len(m.notifs.ActiveToasts()) != 1 {
		t.Fatal("raising an open window at the limit was refused")
	}
	m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if cmd := m.openWindow(windowApp("com.test.extra", 99)); cmd == nil || m.FocusedWindow() != "com.test.extra" {
		t.Fatal("a closed slot did not admit a new window")
	}
}

func TestWindowsReopenFocusesOrReplaces(t *testing.T) {
	m := newWindowsModel()
	a := windowApp("com.test.alpha", 1)
	v := openLoaded(m, a)
	typeText(m, "draft")
	openLoaded(m, windowApp("com.test.beta", 2))
	if cmd := m.openWindow(a); cmd != nil {
		t.Fatal("opening an open window fetched its view again")
	}
	if m.FocusedWindow() != "com.test.alpha" || m.views["com.test.alpha"] != v || v.values["text"] != "draft" {
		t.Fatal("opening an open app did not raise its window with the draft")
	}
	a.PID = 3
	m.openWindow(a)
	if nv := m.views["com.test.alpha"]; nv == v || nv.pid != 3 || nv.values["text"] != "" || m.windows.Len() != 2 {
		t.Fatal("a restarted app's window kept the old instance's state")
	}
}
