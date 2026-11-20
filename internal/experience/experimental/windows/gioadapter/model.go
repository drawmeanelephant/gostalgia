// Package gioadapter is the experimental Gio implementation of the draft
// window service. Toolkit types stay in this adapter and out of package
// windows. It uses only Gio's pure-Go core (layout, op, widget, gesture, the
// io/input router, and the embedded Go fonts); gioui.org/app and the GPU
// backends are cgo on macOS and require display headers on Linux, so the demo
// window lives behind the giodemo build tag.
package gioadapter

import (
	"fmt"
	"image"
	"strings"

	"gioui.org/font"
	"gioui.org/font/gofont"
	"gioui.org/gesture"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/layout"
	"gioui.org/text"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
	"gostalgia/sdk"
)

// controlID mirrors the bloom adapter's control identity: one window's title,
// close control, field, action, or grid cell.
type controlID struct {
	window windows.ID
	kind   string
	id     string
}

// windowTag is the event tag that reports presses landing anywhere inside a
// window's frame, for click-to-raise.
type windowTag struct{ window windows.ID }

// control is one clickable, key-focusable region. Its gesture is the pointer
// tag; the control itself is the key tag, so FocusCmd, FocusFilter, and
// focus-limited key filters all name the same value.
type control struct {
	id    controlID
	click gesture.Click
}

type windowState struct {
	title    gesture.Drag
	bg       windowTag
	close    *control
	fields   map[string]*control
	actions  map[string]*control
	cells    map[string]*control
	tabOrder []event.Tag
}

func newWindowState(id windows.ID, view sdk.View) *windowState {
	s := &windowState{
		bg:      windowTag{window: id},
		fields:  make(map[string]*control),
		actions: make(map[string]*control),
		cells:   make(map[string]*control),
	}
	s.close = &control{id: controlID{window: id, kind: "close"}}
	s.tabOrder = append(s.tabOrder, s.close)
	for _, f := range view.Fields {
		c := &control{id: controlID{window: id, kind: "field", id: f.ID}}
		s.fields[f.ID] = c
		s.tabOrder = append(s.tabOrder, c)
	}
	for _, a := range view.Actions {
		if a.Disabled {
			continue
		}
		c := &control{id: controlID{window: id, kind: "action", id: a.ID}}
		s.actions[a.ID] = c
		s.tabOrder = append(s.tabOrder, c)
	}
	if g := view.Grid; g != nil {
		for _, cell := range g.Cells {
			if cell.Disabled {
				continue
			}
			c := &control{id: controlID{window: id, kind: "cell", id: cell.ID}}
			s.cells[cell.ID] = c
			s.tabOrder = append(s.tabOrder, c)
		}
	}
	return s
}

func (s *windowState) find(id controlID) *control {
	var m map[string]*control
	switch id.kind {
	case "field":
		m = s.fields
	case "action":
		m = s.actions
	case "cell":
		m = s.cells
	case "close":
		return s.close
	default:
		return nil
	}
	return m[id.id]
}

type dragState struct {
	start  image.Point
	origin windows.Bounds
}

// keyMoveStep is the keyboard move step in the adapter's pixel units. Bloom
// moves one cell per Alt-arrow; one pixel would be imperceptible, so the
// equivalent here is a small fixed step through the same clamped Move
// transition.
const keyMoveStep = 8

// statusBand is the reserved bottom strip for status and key help. Windows
// clamp above it the way the bloom adapter reserves its help row.
const statusBand = 32

// Model hosts every experimental window in one Gio frame, mirroring the bloom
// adapter's single terminal program. Input and focus bookkeeping live here;
// layout.Context supplies the input.Source each frame.
type Model struct {
	service *windows.Service
	th      theme.Theme
	colors  gioColors
	shaper  *text.Shaper
	font    font.Font
	size    image.Point

	presentations map[windows.ID]*presentationState
	states        map[windows.ID]*windowState
	remembered    map[windows.ID]controlID
	drags         map[windows.ID]dragState
	scope         windows.ID
	current       *control
	status        string

	// Quit reports Ctrl-C/Ctrl-D, the bloom model's tea.Quit equivalent. The
	// caller owns the event loop and exits on it.
	Quit bool

	// OnAction observes the unchanged semantic SDK request. It runs during
	// layout and must not block.
	OnAction func(windows.ID, sdk.ActionRequest)
}

// New builds an adapter over an existing neutral service and the SDK snapshot
// each open window shows; the service itself carries no presentation. The
// shaper uses only the embedded Go fonts so frames are identical on every host.
func New(service *windows.Service, th theme.Theme, views map[windows.ID]sdk.View) *Model {
	m := &Model{
		service:       service,
		th:            th,
		colors:        resolveColors(th),
		shaper:        text.NewShaper(text.NoSystemFonts(), text.WithCollection(gofont.Collection())),
		font:          font.Font{Typeface: "Go Mono"},
		presentations: make(map[windows.ID]*presentationState),
		states:        make(map[windows.ID]*windowState),
		remembered:    make(map[windows.ID]controlID),
		drags:         make(map[windows.ID]dragState),
		status:        "Two SDK fixtures; no production window wiring.",
	}
	for _, w := range service.Windows() {
		m.presentations[w.ID] = newPresentationState(views[w.ID])
		m.states[w.ID] = newWindowState(w.ID, views[w.ID])
	}
	if id, ok := service.Focused(); ok {
		m.scope = id
		m.current = m.resolveFocus(id, controlID{})
	}
	return m
}

// Layout draws one frame: global window keys first so no control can consume
// them, then the base canvas, then every open window in bottom-to-top service
// order. Rendering performs no service transition, so a frame cannot
// manufacture lifecycle events.
func (m *Model) Layout(gtx layout.Context) layout.Dimensions {
	m.globalKeys(gtx)
	if m.size != gtx.Constraints.Max {
		m.size = gtx.Constraints.Max
		m.clampWindows()
	}
	m.base(gtx)
	for _, w := range m.service.Windows() {
		m.window(gtx, w)
	}
	m.ensureFocus(gtx)
	return layout.Dimensions{Size: gtx.Constraints.Max}
}

func (m *Model) globalKeys(gtx layout.Context) {
	for {
		ev, ok := gtx.Event(
			key.Filter{Name: key.NameF6},
			key.Filter{Name: "W", Required: key.ModCtrl},
			key.Filter{Name: "C", Required: key.ModCtrl},
			key.Filter{Name: "D", Required: key.ModCtrl},
			key.Filter{Name: key.NameTab},
			key.Filter{Name: key.NameTab, Required: key.ModShift},
			key.Filter{Name: key.NameLeftArrow, Required: key.ModAlt},
			key.Filter{Name: key.NameRightArrow, Required: key.ModAlt},
			key.Filter{Name: key.NameUpArrow, Required: key.ModAlt},
			key.Filter{Name: key.NameDownArrow, Required: key.ModAlt},
		)
		if !ok {
			return
		}
		e, ok := ev.(key.Event)
		if !ok || e.State != key.Press {
			continue
		}
		switch {
		case e.Name == key.NameF6:
			m.cycleFocus(gtx)
		case e.Name == "W":
			if id, ok := m.service.Focused(); ok {
				m.close(gtx, id)
			}
		case e.Name == "C" || e.Name == "D":
			m.Quit = true
		case e.Name == key.NameTab:
			m.tabFocus(gtx, e.Modifiers&key.ModShift != 0)
		case e.Name == key.NameLeftArrow:
			m.keyboardMove(-keyMoveStep, 0)
		case e.Name == key.NameRightArrow:
			m.keyboardMove(keyMoveStep, 0)
		case e.Name == key.NameUpArrow:
			m.keyboardMove(0, -keyMoveStep)
		case e.Name == key.NameDownArrow:
			m.keyboardMove(0, keyMoveStep)
		}
	}
}

func (m *Model) cycleFocus(gtx layout.Context) {
	order := m.service.Order()
	if len(order) == 0 {
		return
	}
	current, _ := m.service.Focused()
	next := order[0]
	for i, id := range order {
		if id == current {
			next = order[(i+1)%len(order)]
			break
		}
	}
	m.activate(gtx, next, controlID{})
}

func (m *Model) activate(gtx layout.Context, id windows.ID, c controlID) {
	if _, ok := m.service.Get(id); !ok {
		return
	}
	m.service.Focus(id)
	m.focusControl(gtx, id, c)
	m.syncGridFocus(id)
}

// focusControl moves adapter key focus inside window id: the given control,
// else the window's remembered control, else its first tab stop. Gio key focus
// is global to the router; the per-window scope and remembered control are
// adapter policy, like bloom's scope push/pop.
func (m *Model) focusControl(gtx layout.Context, id windows.ID, c controlID) {
	if m.scope != id && m.scope != "" && m.current != nil {
		m.remembered[m.scope] = m.current.id
	}
	m.scope = id
	if target := m.resolveFocus(id, c); target != nil {
		m.current = target
		m.remembered[id] = target.id
		gtx.Execute(key.FocusCmd{Tag: target})
	}
}

func (m *Model) resolveFocus(id windows.ID, c controlID) *control {
	ws := m.states[id]
	if ws == nil {
		return nil
	}
	if c.window == id {
		if t := ws.find(c); t != nil {
			return t
		}
	}
	if r, ok := m.remembered[id]; ok {
		if t := ws.find(r); t != nil {
			return t
		}
	}
	if len(ws.tabOrder) > 0 {
		if c, ok := ws.tabOrder[0].(*control); ok {
			return c
		}
	}
	return nil
}

// tabFocus cycles key focus within the focused window only. The router's own
// MoveFocus walks a global focus order and would escape the window's scope, so
// the adapter cycles its per-window tab order itself, matching the bloom
// scope.
func (m *Model) tabFocus(gtx layout.Context, backward bool) {
	id, ok := m.service.Focused()
	if !ok {
		return
	}
	ws := m.states[id]
	if ws == nil || len(ws.tabOrder) == 0 {
		return
	}
	delta := 1
	if backward {
		delta = -1
	}
	idx := -delta
	for i, t := range ws.tabOrder {
		if t == event.Tag(m.current) {
			idx = i
			break
		}
	}
	var next *control
	for range len(ws.tabOrder) {
		idx = (idx + delta + len(ws.tabOrder)) % len(ws.tabOrder)
		if c, ok := ws.tabOrder[idx].(*control); ok {
			next = c
			break
		}
	}
	if next != nil {
		m.current = next
		m.remembered[id] = next.id
		gtx.Execute(key.FocusCmd{Tag: next})
	}
}

// ensureFocus restores adapter key focus when the router has none: the initial
// focus for a service that was already focused at New, or after an external
// clear.
func (m *Model) ensureFocus(gtx layout.Context) {
	if m.scope == "" || m.current == nil {
		return
	}
	if gtx.Focused(m.current) {
		return
	}
	gtx.Execute(key.FocusCmd{Tag: m.current})
}

func (m *Model) close(gtx layout.Context, id windows.ID) {
	if !m.service.Close(id) {
		return
	}
	delete(m.presentations, id)
	delete(m.states, id)
	delete(m.remembered, id)
	delete(m.drags, id)
	if m.current != nil && m.current.id.window == id {
		m.current = nil
	}
	if next, ok := m.service.Focused(); ok {
		m.focusControl(gtx, next, controlID{})
	} else {
		m.scope = ""
		gtx.Execute(key.FocusCmd{Tag: nil})
	}
}

func (m *Model) clampWindows() {
	if m.size.X <= 0 || m.size.Y <= statusBand {
		return
	}
	for _, w := range m.service.Windows() {
		m.service.Move(w.ID, m.clampBounds(w.Bounds))
	}
}

// clampBounds keeps a window inside the canvas above the status band. Neutral
// bounds are pixels in this adapter; the bloom adapter's cells have no meaning
// here.
func (m *Model) clampBounds(b windows.Bounds) windows.Bounds {
	if m.size.X <= 0 || m.size.Y <= statusBand {
		return b
	}
	b.Width = min(b.Width, m.size.X)
	b.Height = min(b.Height, m.size.Y-statusBand)
	b.X = max(0, min(b.X, m.size.X-b.Width))
	b.Y = max(0, min(b.Y, m.size.Y-statusBand-b.Height))
	return b
}

func (m *Model) keyboardMove(dx, dy int) {
	id, ok := m.service.Focused()
	if !ok {
		return
	}
	w, ok := m.service.Get(id)
	if !ok {
		return
	}
	b := w.Bounds
	b.X += dx
	b.Y += dy
	m.service.Move(id, m.clampBounds(b))
}

func (m *Model) syncGridFocus(id windows.ID) {
	p := m.presentations[id]
	if p == nil || p.view.Grid == nil || m.current == nil {
		return
	}
	c := m.current
	if c.id.window != id || c.id.kind != "cell" {
		return
	}
	for i, cell := range p.view.Grid.Cells {
		if cell.ID == c.id.id {
			p.cell = i
			return
		}
	}
}

// press activates a control as Enter/Space or a completed click would: fields
// only take focus, everything else performs its role.
func (m *Model) press(gtx layout.Context, id controlID) {
	switch id.kind {
	case "close":
		m.close(gtx, id.window)
	case "action":
		m.dispatch(id.window, id.id, "")
	case "cell":
		p := m.presentations[id.window]
		if p == nil || p.view.Grid == nil {
			return
		}
		for _, cell := range p.view.Grid.Cells {
			if cell.ID == id.id {
				m.activateCell(id.window, cell)
				return
			}
		}
	}
}

func (m *Model) activateCell(id windows.ID, cell sdk.Cell) {
	if cell.Disabled {
		return
	}
	p := m.presentations[id]
	if p == nil || p.view.Grid == nil {
		return
	}
	for i, candidate := range p.view.Grid.Cells {
		if candidate.ID == cell.ID {
			p.cell = i
			break
		}
	}
	if cell.Action != "" {
		m.dispatch(id, cell.Action, cell.ID)
	}
}

func (m *Model) dispatch(id windows.ID, action, cellID string) {
	p, ok := m.presentations[id]
	if !ok || action == "" {
		return
	}
	if _, ok := m.service.Get(id); !ok {
		return
	}
	for _, a := range p.view.Actions {
		if a.ID == action && a.Disabled {
			return
		}
	}
	if cellID == "" && p.view.Grid != nil && p.cell >= 0 && p.cell < len(p.view.Grid.Cells) {
		if cell := p.view.Grid.Cells[p.cell]; !cell.Disabled {
			cellID = cell.ID
		}
	}
	request := p.request(p.view, action, cellID)
	m.status = fmt.Sprintf("%s: %s", id, action)
	if m.OnAction != nil {
		m.OnAction(id, request)
	}
}

// Actions returns a copy of the semantic requests emitted by one window.
func (m *Model) Actions(id windows.ID) []sdk.ActionRequest {
	p := m.presentations[id]
	if p == nil {
		return nil
	}
	out := make([]sdk.ActionRequest, len(p.actions))
	for i, request := range p.actions {
		out[i] = cloneRequest(request)
	}
	return out
}

func cloneRequest(request sdk.ActionRequest) sdk.ActionRequest {
	values := make(map[string]string, len(request.Values))
	for k, v := range request.Values {
		values[k] = v
	}
	request.Values = values
	return request
}

func trimLastCluster(s string) string {
	clusters := ui.GraphemeClusters(s)
	return strings.Join(clusters[:max(0, len(clusters)-1)], "")
}

// oneLine flattens sanitized text for single-line labels, the same treatment
// bloom labels get through the kit's private helper.
func oneLine(s string) string {
	return strings.ReplaceAll(ui.Sanitize(s), "\n", " ")
}
