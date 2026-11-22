package shell

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	"bloomfx.dev/bloom/on"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
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
	if err := m.windows.Open(windows.Window{ID: id, Title: viewText(a.Manifest.Name), Bounds: m.placeWindow()}); err != nil {
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
	}, viewTick(id, epoch), m.ensureViewSub(), m.startWindowReveal(id))
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
	m.stopWindowReveal(id)
	m.windowMoving = false
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
	m.windowsShown, m.windowMoving = false, false
	return cancels
}

// Window geometry is in body cells. A window is never smaller than
// minWindowWidth × minWindowHeight, which leaves room for [X], a short title,
// and ten content rows inside the border. A body below that floor shows only
// the focused window, filling it without chrome.
const (
	minWindowWidth  = 32
	minWindowHeight = 12
	// Cascaded windows align flush-left with the body so no clipped text or
	// border slivers peek out along the left edge, and step down one row per
	// window so each lower window's title bar stays visible. windowMarginX
	// leaves two columns on the right for the static shadow and horizontal
	// movement; windowShadowY leaves one row below when height permits.
	windowMarginX, windowShadowY = 2, 1
	windowCascadeY               = 1
)

// Window chrome, ported from the experimental bloom adapter.
const (
	closeLabel = "[X]"
	// closeHoverLabel is the close control under the pointer: the shell's
	// ASCII pointer cue in the same three cells, as a hovered grid cell.
	closeHoverLabel = "<X>"
	// topBorderSpan is the border space both corners and the margins inside
	// them take from a window's top edge before the title and close controls
	// can share the rest.
	topBorderSpan = 4
	// shadowTint is how far the shadow band's background moves toward the
	// theme's muted tone.
	shadowTint = 0.35
)

// windowLayerID names a window's layer for Frame.Rect.
type windowLayerID windows.ID

// windowPart names a window's title or close control for Frame.Rect. A
// dragged title is followed from frame to frame by it.
type windowPart struct {
	id   windows.ID
	kind windowPartKind
}

type windowPartKind uint8

const (
	windowTitle windowPartKind = iota + 1
	windowClose
)

// windowDrag is a title drag in progress: the window and its bounds when the
// button went down.
type windowDrag struct {
	id     windows.ID
	origin windows.Bounds
}

// fitsWindows reports whether body is large enough for floating windows.
func fitsWindows(body image.Point) bool {
	return body.X >= minWindowWidth && body.Y >= minWindowHeight
}

// floatingWindows reports whether the Windows screen draws floating windows
// at the current size, rather than the focused one filling the body.
func (m *Model) floatingWindows() bool { return fitsWindows(m.bodySize()) }

// clampWindow keeps b inside body: no larger than it and no smaller than the
// floor, and moved in from any edge it crosses.
func clampWindow(b windows.Bounds, body image.Point) windows.Bounds {
	b.Width = max(minWindowWidth, min(b.Width, body.X))
	b.Height = max(minWindowHeight, min(b.Height, body.Y))
	b.X = max(0, min(b.X, body.X-b.Width))
	b.Y = max(0, min(b.Y, body.Y-b.Height))
	return b
}

// placeWindow is where a new window opens. The first fills the body, so one
// window has the room the presentation had before windows, which an 80×24
// focal-block split needs. Each later one aligns flush-left one row below the
// window opened before it, inset by windowMarginX on the right and
// windowShadowY on the bottom when height permits, clamped inside the body.
func (m *Model) placeWindow() windows.Bounds {
	body := m.bodySize()
	if m.windows.Len() == 0 {
		return clampWindow(windows.Bounds{Width: body.X, Height: body.Y}, body)
	}
	y := windowCascadeY
	var last *appView
	for _, v := range m.views {
		if last == nil || v.epoch > last.epoch {
			last = v
		}
	}
	if last != nil {
		if w, ok := m.windows.Get(windows.ID(last.id)); ok {
			y = w.Bounds.Y + windowCascadeY
		}
	}
	b := windows.Bounds{
		X:      0,
		Y:      y,
		Width:  body.X - windowMarginX,
		Height: body.Y - y - windowShadowY,
	}
	return clampWindow(b, body)
}

// windowRect is b clamped inside body as a rectangle in body cells.
func windowRect(b windows.Bounds, body image.Point) image.Rectangle {
	b = clampWindow(b, body)
	return image.Rect(b.X, b.Y, b.X+b.Width, b.Y+b.Height)
}

// windowObscured reports whether any higher window in above overlaps w's
// interior content area. Obscured unfocused windows render a quiet blank
// surface inside their chrome so clipped text fragments never compete with
// the active window.
func windowObscured(b windows.Bounds, body image.Point, above []image.Rectangle) bool {
	content := windowRect(b, body).Inset(1)
	if content.Empty() {
		return false
	}
	for _, r := range above {
		if !content.Intersect(r).Empty() {
			return true
		}
	}
	return false
}

// windowsPromptHint is the prompt-row annotation on the Windows screen.
// With one window or below the floating floor it keeps the compact default
// hint; with multiple floating windows it lists the other open windows in
// F8 cycle order so every open window stays discoverable even when the
// focused window covers the rest of the body.
func (m *Model) windowsPromptHint() string {
	if m.windows == nil || m.windows.Len() <= 1 || !m.floatingWindows() {
		return "Window owns focus. Esc goes back, Ctrl-W closes."
	}
	order := m.windows.Order()
	sep := m.kit.Separator()
	others := make([]string, 0, len(order)-1)
	for _, id := range order[:len(order)-1] {
		if w, ok := m.windows.Get(id); ok {
			title := viewText(w.Title)
			if title == "" {
				title = string(id)
			}
			others = append(others, title)
		}
	}
	return "F8 next: " + strings.Join(others, sep)
}

// clampWindows keeps every window inside a resized body. A body below the
// floor draws no window frames, so the windows keep their places for when
// it grows again.
func (m *Model) clampWindows() {
	body := m.bodySize()
	if m.windows == nil || !fitsWindows(body) {
		return
	}
	for _, w := range m.windows.Windows() {
		m.windows.Move(w.ID, clampWindow(w.Bounds, body))
	}
}

// windowContentSize is the size v's presentation is drawn at: inside its
// window's border, or in a body below the floor, the whole body.
func (m *Model) windowContentSize(v *appView) (width, height int) {
	body := m.bodySize()
	if !fitsWindows(body) {
		return body.X, body.Y
	}
	w, ok := m.windows.Get(windows.ID(v.id))
	if !ok {
		return 0, 0
	}
	b := clampWindow(w.Bounds, body)
	return b.Width - 2, b.Height - 2
}

// moveWindow moves the focused window by dx, dy cells inside the body, as
// Alt-arrows and move mode do. Below the floor no window frame shows, so
// nothing moves.
func (m *Model) moveWindow(dx, dy int) {
	body := m.bodySize()
	id, ok := m.windows.Focused()
	w, found := m.windows.Get(id)
	if !ok || !found || !fitsWindows(body) {
		return
	}
	b := clampWindow(w.Bounds, body)
	b.X, b.Y = b.X+dx, b.Y+dy
	if m.windows.Move(id, clampWindow(b, body)) {
		// The window moved under a still pointer.
		m.clearHoverAll()
	}
}

// arrowDelta is the cell step of an arrow key name.
func arrowDelta(key string) (dx, dy int, ok bool) {
	switch key {
	case "left":
		return -1, 0, true
	case "right":
		return 1, 0, true
	case "up":
		return 0, -1, true
	case "down":
		return 0, 1, true
	}
	return 0, 0, false
}

// movingWindow reports whether move mode is on: the palette's Move window
// started it, and the arrow keys move the focused window until Enter or Esc.
func (m *Model) movingWindow() bool {
	return m.windowMoving && m.onWindows() && m.floatingWindows()
}

// startMovingWindow shows the Windows screen in move mode, for the palette's
// Move window, the keyboard's way to Alt-arrows on terminals that do not
// report them.
func (m *Model) startMovingWindow() tea.Cmd {
	if m.focusedView() == nil {
		return nil
	}
	m.showWindows()
	m.windowMoving = true
	return nil
}

// raiseWindow focuses and raises id, as a click on it does.
func (m *Model) raiseWindow(id windows.ID) {
	if current, ok := m.windows.Focused(); ok && current == id {
		return
	}
	if m.windows.Focus(id) {
		m.clearHoverAll()
	}
}

// windowAct runs act for a click on a window, unless a modal layer, the
// mouse setting, or another screen has taken the Windows screen's input
// since the frame was drawn.
func (m *Model) windowAct(act func() tea.Cmd) tea.Cmd {
	return m.baseAct(func() tea.Cmd {
		if !m.onWindows() {
			return nil
		}
		return act()
	})
}

// dragWindow follows a drag on id's title: the press focuses and raises the
// window, and each motion moves it by how far the pointer has moved since,
// clamped inside the body.
func (m *Model) dragWindow(id windows.ID, e on.Event) {
	if e.Done {
		defer func() { m.drag = windowDrag{} }()
	}
	body := m.bodySize()
	if !m.baseMouseAllowed() || !m.onWindows() || !fitsWindows(body) {
		m.drag = windowDrag{}
		return
	}
	if _, ok := e.Msg.(tea.MouseClickMsg); ok {
		w, ok := m.windows.Get(id)
		if !ok {
			return
		}
		m.raiseWindow(id)
		m.drag = windowDrag{id: id, origin: clampWindow(w.Bounds, body)}
		return
	}
	if e.Msg == nil || m.drag.id != id {
		return
	}
	b := m.drag.origin
	b.X += e.Screen.X - e.Start.X
	b.Y += e.Screen.Y - e.Start.Y
	m.windows.Move(id, clampWindow(b, body))
}

// windowHover keeps kind's pointer cue on id while the pointer is over the
// node it is attached to.
func (m *Model) windowHover(kind hoverKind, id windows.ID) on.Handler {
	epoch := m.hoverEpoch
	return on.Hover(func(over bool) tea.Cmd {
		if epoch != m.hoverEpoch {
			return nil
		}
		if over && m.baseMouseAllowed() && m.onWindows() {
			m.setHover(kind, string(id))
		} else {
			m.clearHover(kind, string(id))
		}
		return nil
	})
}

// windowShadow is the static depth beneath a window: the background of the
// cells in the band moves toward the theme's muted tone while their text
// stays put, so the window reads as raised without hiding what it overlaps.
// Plain, ANSI16 and Monochrome carry meaning without color and get none.
func (m *Model) windowShadow() []fx.Effect {
	switch mode := m.kit.Mode(); {
	case mode != ui.ANSI256 && mode != ui.TrueColor, m.kit.Theme().Name == theme.Monochrome().Name:
		return nil
	}
	return []fx.Effect{fx.Recolor{Bg: m.kit.Theme().Palette.Muted, Amount: shadowTint}}
}

// windowsNode is the Windows screen: every window in z-order over an empty
// desktop, or in a body below the floor, the focused window alone filling it
// without chrome.
func (m *Model) windowsNode() bloom.Child {
	return ui.SizedNode(func(ctx bloom.Context) bloom.Child {
		body := image.Pt(ctx.Width, ctx.Height)
		if !fitsWindows(body) {
			return m.windowContent(m.focusedView(), true)
		}
		ws := m.windows.Windows()
		rects := make([]image.Rectangle, len(ws))
		for i, w := range ws {
			rects[i] = windowRect(w.Bounds, body)
		}
		layers := make([]bloom.LayerNode, 0, len(ws))
		for i, w := range ws {
			layers = append(layers, m.windowLayer(w, body, windowObscured(w.Bounds, body, rects[i+1:])))
		}
		return bloom.Overlay(bloom.Text("").Style(m.kit.TextStyle()), layers...)
	})
}

// windowContent draws v's presentation at the size it is given. Only the
// focused window's controls take clicks; a click elsewhere in another
// window raises it first.
func (m *Model) windowContent(v *appView, focused bool) bloom.Child {
	leaf := m.kit.Leaf(func(b ui.Bounds) string {
		lines := m.viewLines(v, b.Width, b.Height)
		for i, line := range lines {
			lines[i] = m.kit.Truncate(line, b.Width)
		}
		return strings.Join(lines, "\n")
	})
	if !focused {
		return leaf
	}
	return m.withMouse(leaf, m.viewHits, nil)
}

// windowLayer is one window over the desktop: an opaque box with its title
// and close control on the top border, at its bounds clamped inside body.
func (m *Model) windowLayer(w windows.Window, body image.Point, obscured bool) bloom.LayerNode {
	id := w.ID
	b := clampWindow(w.Bounds, body)
	current, _ := m.windows.Focused()
	focused := current == id

	// The close control is placed first, from the right edge, because it
	// must survive every valid window width; the title takes what is left.
	closeText := closeLabel
	if m.hoverAt(hoverWindowClose, string(id)) {
		closeText = closeHoverLabel
	}
	titleWidth := max(0, b.Width-topBorderSpan-lipgloss.Width(closeText)-1)
	// A two-cell lead keeps the title in place as focus and the pointer
	// come and go: the focus marker, then the pointer cue.
	lead, cue := " ", " "
	if focused {
		lead = m.kit.Theme().Focus.Marker
	}
	if m.hoverAt(hoverWindowTitle, string(id)) {
		cue = ui.HoverMarker
	}
	titleText := m.kit.Truncate(lead+cue+ui.Sanitize(w.Title), titleWidth)
	var closeStyled string
	if focused {
		titleText = m.kit.Selection(titleText)
		closeStyled = m.kit.Text(closeText)
	} else {
		titleText = m.kit.WindowMuted(titleText)
		closeStyled = m.kit.WindowMuted(closeText)
	}
	title := bloom.Text(titleText).ID(windowPart{id, windowTitle}).Truncate()
	closeButton := bloom.Text(closeStyled).ID(windowPart{id, windowClose}).Truncate()
	if !m.mouseOff {
		title = title.On(
			on.Drag(func(e on.Event) tea.Cmd {
				m.dragWindow(id, e)
				return nil
			}),
			m.windowHover(hoverWindowTitle, id),
		)
		closeButton = closeButton.On(
			on.Click(func() tea.Cmd {
				return m.windowAct(func() tea.Cmd { return m.closeWindow(id) })
			}),
			m.windowHover(hoverWindowClose, id),
		)
	}
	var content bloom.Child
	switch {
	case !focused && obscured:
		content = m.kit.WindowMutedLeaf(func(ui.Bounds) string { return "" })
	case m.views[id] != nil:
		content = m.windowContent(m.views[id], focused)
	case !focused:
		content = m.kit.WindowMutedLeaf(func(ui.Bounds) string { return "" })
	default:
		content = m.kit.Leaf(func(ui.Bounds) string { return "" })
	}
	box := bloom.Box(m.kit.WindowStyle(focused), content).
		Edge(bloom.Top, bloom.End, closeButton).
		Edge(bloom.Top, bloom.Start, title)
	layer := bloom.Layer(box).ID(windowLayerID(id)).
		Anchor(bloom.Start, bloom.Start).
		Offset(b.X, b.Y).
		Size(b.Width, b.Height)
	if focused {
		layer = layer.Shadow(2, 1, m.windowShadow()...)
	}
	if progress, ok := m.windowProgress(id); ok {
		layer = layer.Transition(progress, fx.Iris{})
	}
	if !m.mouseOff {
		layer = layer.On(on.Mouse(func(e on.Event) tea.Cmd {
			if click, ok := e.Msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
				return m.windowAct(func() tea.Cmd {
					m.raiseWindow(id)
					return nil
				})
			}
			return nil
		}))
	}
	return layer
}
