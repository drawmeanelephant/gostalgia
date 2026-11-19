// Package bloomadapter is the experimental bloom implementation of the draft
// window service. Toolkit types stay in this adapter and out of package windows.
package bloomadapter

import (
	"fmt"
	"image"
	"strings"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

type controlID struct {
	window windows.ID
	kind   string
	id     string
}

type scopeID struct{ window windows.ID }
type layerID struct{ window windows.ID }
type dragState struct {
	start  image.Point
	origin windows.Bounds
}

// Model is one Bubble Tea model hosting every experimental window. It owns the
// only terminal program used by the demo; window presentations are not programs.
type Model struct {
	service *windows.Service
	kit     ui.Kit
	root    *bloom.Root
	focus   bloom.Focus[controlID]
	scope   windows.ID

	width  int
	height int

	presentations map[windows.ID]*presentationState
	remembered    map[windows.ID]controlID
	drags         map[windows.ID]dragState
	status        string

	// OnAction observes the unchanged semantic SDK request. It runs on the
	// model event loop and must not block.
	OnAction func(windows.ID, sdk.ActionRequest)
}

// New builds an adapter over an existing neutral service. It never calls or
// returns Root.Init; the kit supplies explicit colors.
func New(service *windows.Service, kit ui.Kit) *Model {
	m := &Model{
		service:       service,
		kit:           kit,
		presentations: make(map[windows.ID]*presentationState),
		remembered:    make(map[windows.ID]controlID),
		drags:         make(map[windows.ID]dragState),
		status:        "Two SDK fixtures; no production window wiring.",
	}
	fg, bg := kit.Colors()
	m.root = bloom.New(bloom.WithColors(fg, bg), bloom.WithCopyFlash(false), bloom.WithFocus(&m.focus))
	for _, w := range service.Windows() {
		m.presentations[w.ID] = newPresentationState(w.Presentation)
	}
	if id, ok := service.Focused(); ok {
		m.scope = id
		m.focus.Push(scopeID{window: id})
	}
	return m
}

// Init deliberately schedules no host-color queries or animation work.
func (m *Model) Init() tea.Cmd { return nil }

// Update routes global window keys, scoped controls, and presentation input.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampWindows()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "f6":
			m.cycleFocus()
			return m, nil
		case "ctrl+w":
			if id, ok := m.service.Focused(); ok {
				m.close(id)
				return m, tea.ClearScreen
			}
			return m, nil
		case "alt+left", "alt+right", "alt+up", "alt+down":
			m.keyboardMove(msg.String())
			return m, nil
		}
	case tea.BackgroundColorMsg, tea.ForegroundColorMsg, tea.ColorProfileMsg:
		// Explicit theme policy wins even if a host sends an unsolicited reply.
		return m, nil
	}

	rootCmd := m.root.Update(msg)
	focusCmd, handled := m.focus.Update(msg, m.root.Frame())
	m.syncGridFocus()
	if handled {
		return m, tea.Batch(rootCmd, focusCmd)
	}
	m.presentationKey(msg)
	return m, rootCmd
}

func (m *Model) cycleFocus() {
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
	m.service.Focus(next)
	m.syncFocus(next, controlID{})
}

func (m *Model) activate(id windows.ID, control controlID) {
	if _, ok := m.service.Get(id); !ok {
		return
	}
	m.service.Focus(id)
	m.syncFocus(id, control)
	m.syncGridFocus()
}

func (m *Model) syncFocus(id windows.ID, control controlID) {
	if m.scope != id {
		if current, ok := m.focus.Current(); ok && m.scope != "" {
			m.remembered[m.scope] = current
		}
		if m.scope != "" {
			m.focus.Pop()
		}
		m.focus.Push(scopeID{window: id})
		m.scope = id
	}
	if control.window == id {
		m.focus.Set(control)
		return
	}
	if remembered, ok := m.remembered[id]; ok {
		m.focus.Set(remembered)
		if m.focus.Within(m.root.Frame(), scopeID{window: id}) {
			return
		}
	}
	m.focus.Next(m.root.Frame())
}

func (m *Model) close(id windows.ID) {
	if !m.service.Close(id) {
		return
	}
	delete(m.presentations, id)
	delete(m.remembered, id)
	delete(m.drags, id)
	if next, ok := m.service.Focused(); ok {
		m.syncFocus(next, controlID{})
	} else {
		m.focus.Clear()
		m.scope = ""
	}
}

func (m *Model) clampWindows() {
	if m.width <= 0 || m.height <= 1 {
		return
	}
	for _, w := range m.service.Windows() {
		m.service.Move(w.ID, m.clampBounds(w.Bounds))
	}
}

func (m *Model) clampBounds(b windows.Bounds) windows.Bounds {
	if m.width <= 0 || m.height <= 1 {
		return b
	}
	b.Width = min(b.Width, m.width)
	b.Height = min(b.Height, m.height-1)
	b.X = max(0, min(b.X, m.width-b.Width))
	b.Y = max(0, min(b.Y, m.height-1-b.Height))
	return b
}

func (m *Model) keyboardMove(key string) {
	id, ok := m.service.Focused()
	if !ok {
		return
	}
	w, ok := m.service.Get(id)
	if !ok {
		return
	}
	b := w.Bounds
	switch key {
	case "alt+left":
		b.X--
	case "alt+right":
		b.X++
	case "alt+up":
		b.Y--
	case "alt+down":
		b.Y++
	}
	m.service.Move(id, m.clampBounds(b))
}

func (m *Model) syncGridFocus() {
	id, ok := m.service.Focused()
	if !ok {
		return
	}
	p := m.presentations[id]
	control, ok := m.focus.Current()
	if p == nil || p.view.Grid == nil || !ok || control.window != id || control.kind != "cell" {
		return
	}
	for i, cell := range p.view.Grid.Cells {
		if cell.ID == control.id {
			p.cell = i
			return
		}
	}
}

func (m *Model) dispatch(id windows.ID, action, cellID string) {
	p, ok := m.presentations[id]
	if !ok || action == "" {
		return
	}
	w, ok := m.service.Get(id)
	if !ok {
		return
	}
	for _, a := range w.Presentation.Actions {
		if a.ID == action && a.Disabled {
			return
		}
	}
	if cellID == "" && p.view.Grid != nil && p.cell >= 0 && p.cell < len(p.view.Grid.Cells) {
		if cell := p.view.Grid.Cells[p.cell]; !cell.Disabled {
			cellID = cell.ID
		}
	}
	request := p.request(w.Presentation, action, cellID)
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

func keyText(msg tea.Msg) string {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return msg.Text
	case tea.PasteMsg:
		return msg.Content
	default:
		return ""
	}
}

func isKey(msg tea.Msg, names ...string) bool {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return false
	}
	for _, name := range names {
		if key.String() == name {
			return true
		}
	}
	return false
}

func trimLastCluster(s string) string {
	clusters := ui.GraphemeClusters(s)
	return strings.Join(clusters[:max(0, len(clusters)-1)], "")
}
