package shell

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/windows"
	"gostalgia/sdk"
)

// Every app presentation opens in a window on the Windows screen. The model's
// window service owns each window's identity, title, bounds, focus, and
// stacking; views holds its presentation state under the same ID, which is
// the app's ID, as apps are single-instance. The windows stay open while
// other screens show, with their drafts, selection, and scroll.

// maxWindows bounds the open windows, and with them the view fetches each
// 500 ms tick sends.
const maxWindows = 8

// settingsAppID is the Settings app, which F7 and the settings command open.
const settingsAppID = "com.gostalgia.settings"

// focusedView is the focused window's presentation state, or nil while no
// window is open.
func (m *Model) focusedView() *appView {
	if m.windows == nil {
		return nil
	}
	id, ok := m.windows.Focused()
	if !ok {
		return nil
	}
	return m.views[id]
}

// onWindows reports whether the Windows screen is the body. It shows only
// while a window is open.
func (m *Model) onWindows() bool { return m.windowsShown && m.focusedView() != nil }

// FocusedWindow is the ID of the window that takes presentation input, which
// is its app's ID, or "" while no window is open. Focus reports whether that
// window has the keyboard now, which it has only on the Windows screen.
func (m *Model) FocusedWindow() string {
	if v := m.focusedView(); v != nil {
		return v.id
	}
	return ""
}

// openWindow opens a's window and shows the Windows screen. A window already
// open for the same instance of a is focused and raised instead. One bound
// to an earlier instance closes first, so a restarted app never inherits the
// drafts of the instance before it.
func (m *Model) openWindow(a appStatus) tea.Cmd {
	id := windows.ID(a.Manifest.ID)
	var closed tea.Cmd
	if v, ok := m.views[id]; ok {
		if v.pid == a.PID {
			m.windows.Focus(id)
			m.showWindows()
			m.clearHoverAll()
			return nil
		}
		closed = m.dropWindow(id)
	}
	if m.windows.Len() >= maxWindows {
		m.initExperience()
		m.flash(notifications.Notification{
			Level: notifications.LevelWarning, Source: "windows", Badge: "WINDOWS", Wrap: true,
			Title:   "Too many windows",
			Message: fmt.Sprintf("The shell keeps at most %d windows open. Close one with Ctrl-W, then open %s again.", maxWindows, viewText(a.Manifest.Name)),
		})
		return closed
	}
	m.viewEpoch++
	v := &appView{
		id: a.Manifest.ID, pid: a.PID, epoch: m.viewEpoch, version: sdk.PresentationVersion,
		data:   sdk.View{Title: a.Manifest.Name, State: sdk.ViewLoading},
		values: make(map[string]string), regionPinned: true,
	}
	if err := m.windows.Open(windows.Window{ID: id, Title: viewText(a.Manifest.Name), Bounds: m.windowBounds()}); err != nil {
		m.append(entry{"Cannot open a window: " + err.Error(), "error"})
		return closed
	}
	m.views[id] = v
	m.showWindows()
	m.clearHoverAll()
	epoch, ctx, client := v.epoch, m.ctx, m.client
	return tea.Batch(closed, func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		data, version, err := fetchView(ctx, client, string(id), sdk.PresentationVersion)
		return viewMsg{id: id, epoch: epoch, version: version, data: data, err: err}
	}, viewTick(id, epoch), m.ensureViewSub())
}

// windowBounds is where a new window opens: over the whole body, which the
// focused window fills.
func (m *Model) windowBounds() windows.Bounds {
	return windows.Bounds{Width: max(1, m.bodyWidth()), Height: max(1, m.height)}
}

// showWindows makes the Windows screen the body and remembers the screen it
// replaces, which Esc returns to.
func (m *Model) showWindows() {
	if m.windowsShown {
		return
	}
	m.windowsBack = m.shownScreen()
	m.taskView, m.notifView = false, false
	m.setMode(modePrompt)
	m.windowsShown = true
}

// leaveWindows returns from the Windows screen to the screen it replaced.
// Every window stays open.
func (m *Model) leaveWindows() tea.Cmd {
	if !m.windowsShown {
		return nil
	}
	return m.switchScreen(m.windowsBack, false)
}

// closeWindow closes id's window, as Ctrl-W does. Closing the last window
// leaves the Windows screen.
func (m *Model) closeWindow(id windows.ID) tea.Cmd {
	cancel := m.dropWindow(id)
	if m.windowsShown && m.windows.Len() == 0 {
		return tea.Batch(cancel, m.leaveWindows())
	}
	return cancel
}

// dropWindow retracts id's window, its focus, and its data at once, and
// cancels its in-flight action. The app keeps running.
func (m *Model) dropWindow(id windows.ID) tea.Cmd {
	v, ok := m.views[id]
	if !ok {
		return nil
	}
	delete(m.views, id)
	m.windows.Close(id)
	m.clearHoverAll()
	if v.cancel == nil {
		return nil
	}
	return m.cancelViewAction(v)
}

// nextWindow focuses and raises the next window in z-order, as F8 does: the
// bottom one, so repeated presses visit every window.
func (m *Model) nextWindow() {
	if order := m.windows.Order(); len(order) > 1 {
		m.windows.Focus(order[0])
		m.clearHoverAll()
	}
}

// closeAllWindows closes every window, for teardown, and returns the cancel
// requests of their in-flight actions for the caller to run.
func (m *Model) closeAllWindows() []tea.Cmd {
	var cancels []tea.Cmd
	if m.windows == nil {
		return nil
	}
	for _, id := range m.windows.Order() {
		if cancel := m.dropWindow(id); cancel != nil {
			cancels = append(cancels, cancel)
		}
	}
	m.windowsShown = false
	return cancels
}
