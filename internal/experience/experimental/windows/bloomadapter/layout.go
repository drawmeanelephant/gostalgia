package bloomadapter

import (
	"fmt"
	"image"
	"strings"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/on"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/ui"
)

// View composes the base help and open windows. Rendering performs no service
// transition and therefore cannot manufacture lifecycle events.
func (m *Model) View() tea.View {
	base := bloom.Box(m.kit.TextStyle(), bloom.Column(
		bloom.Fill(bloom.Text(m.kit.Text(ui.Sanitize("EXPERIMENTAL WINDOW SPIKE\n"+m.status))).Style(m.kit.TextStyle())),
		bloom.Fixed(1, bloom.Text(m.kit.Text("F6 focus  Alt-arrows move  Ctrl-W close  Tab controls  Enter act  Ctrl-C exit"))),
	))
	layers := make([]bloom.LayerNode, 0, len(m.service.Order()))
	for _, w := range m.service.Windows() {
		layers = append(layers, m.windowLayer(w))
	}
	view := m.root.View(m.width, m.height, bloom.Overlay(base, layers...))
	view.AltScreen = true
	return view
}

func (m *Model) windowLayer(w windows.Window) bloom.LayerNode {
	id := w.ID
	b := w.Bounds
	titleControl := controlID{window: id, kind: "title"}
	closeControl := controlID{window: id, kind: "close"}
	focused, _ := m.service.Focused()

	titleText := w.Title
	if focused == id {
		titleText = m.kit.Theme().Focus.Marker + " " + titleText
	}
	titleText = m.kit.Truncate(ui.Sanitize(titleText), max(1, b.Width-10))
	title := bloom.Text(m.kit.Heading(titleText)).ID(titleControl).Truncate().On(
		on.Drag(func(e on.Event) tea.Cmd {
			m.drag(id, e)
			return nil
		}),
		on.Mouse(func(e on.Event) tea.Cmd {
			if click, ok := e.Msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
				m.activate(id, controlID{})
			}
			return nil
		}),
	)
	closeButton := m.controlText("[X]", m.focus.Is(closeControl)).ID(closeControl).Focusable().On(
		controlMouse(id, closeControl, m),
		on.Press(func() tea.Cmd {
			m.close(id)
			return tea.ClearScreen
		}),
	)
	body := m.presentationBody(id)
	style := m.kit.TextStyle().Border(m.kit.Theme().Borders.Panel)
	box := bloom.Box(style, body).
		Edge(bloom.Top, bloom.Start, title).
		Edge(bloom.Top, bloom.End, closeButton)
	scoped := bloom.Auto(box).ID(scopeID{window: id}).Scope()
	return bloom.Layer(scoped).
		ID(layerID{window: id}).
		Anchor(bloom.Start, bloom.Start).
		Offset(b.X, b.Y).
		Size(b.Width, b.Height).
		On(on.Mouse(func(e on.Event) tea.Cmd {
			if click, ok := e.Msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
				m.activate(id, controlID{})
			}
			return nil
		}))
}

func controlMouse(id windows.ID, control controlID, m *Model) on.Handler {
	return on.Mouse(func(e on.Event) tea.Cmd {
		if click, ok := e.Msg.(tea.MouseClickMsg); ok && click.Button == tea.MouseLeft {
			m.activate(id, control)
		}
		return nil
	})
}

func (m *Model) drag(id windows.ID, e on.Event) {
	w, ok := m.service.Get(id)
	if !ok {
		return
	}
	click, ok := e.Msg.(tea.MouseClickMsg)
	if ok {
		m.drags[id] = dragState{start: image.Pt(click.X, click.Y), origin: w.Bounds}
		m.activate(id, controlID{})
		return
	}
	drag, ok := m.drags[id]
	if !ok {
		return
	}
	if motion, ok := e.Msg.(tea.MouseMotionMsg); ok {
		b := drag.origin
		b.X += motion.X - drag.start.X
		b.Y += motion.Y - drag.start.Y
		m.service.Move(id, m.clampBounds(b))
	}
	if e.Done {
		delete(m.drags, id)
	}
}

func (m *Model) presentationBody(id windows.ID) bloom.Child {
	p := m.presentations[id]
	if p == nil {
		w, ok := m.service.Get(id)
		if !ok {
			return bloom.Text("")
		}
		p = newPresentationState(w.Presentation)
		m.presentations[id] = p
	}
	children := make([]bloom.Child, 0, 20)
	for i, item := range p.view.Items {
		marker := "  "
		if i == p.item {
			marker = "> "
		}
		children = append(children, fixedText(marker+item.Label+" — "+item.Detail, m))
	}
	for _, field := range p.view.Fields {
		control := controlID{window: id, kind: "field", id: field.ID}
		label := field.Label + ": [" + p.values[field.ID] + "]"
		node := m.controlText(label, m.focus.Is(control)).
			ID(control).Focusable().On(controlMouse(id, control, m))
		children = append(children, bloom.Fixed(1, node))
	}
	for _, action := range p.view.Actions {
		control := controlID{window: id, kind: "action", id: action.ID}
		label := "[" + action.Label + "]"
		if action.Disabled {
			label = "(" + action.Label + ")"
			children = append(children, fixedText(label, m))
			continue
		}
		node := m.controlText(label, m.focus.Is(control)).ID(control).Focusable().On(
			controlMouse(id, control, m),
			on.Press(func() tea.Cmd {
				m.dispatch(id, action.ID, "")
				return nil
			}),
		)
		children = append(children, bloom.Fixed(1, node))
	}
	for _, block := range p.view.Blocks {
		text := ""
		if block.Label != "" {
			text = block.Label + "\n"
		}
		text += block.Text
		lines := strings.Split(ui.Sanitize(text), "\n")
		children = append(children, bloom.Fixed(len(lines), bloom.Text(m.kit.Text(strings.Join(lines, "\n")))))
	}
	for _, meter := range p.view.Meters {
		children = append(children, fixedText(fmt.Sprintf("%s %.0f%%", meter.Label, min(max(meter.Value, 0), 1)*100), m))
	}
	if grid := p.view.Grid; grid != nil && len(grid.Cells) > 0 {
		children = append(children, fixedText(grid.Label, m))
		columns := max(1, grid.Columns)
		for start := 0; start < len(grid.Cells); start += columns {
			end := min(len(grid.Cells), start+columns)
			row := make([]bloom.Child, 0, end-start)
			for _, cell := range grid.Cells[start:end] {
				control := controlID{window: id, kind: "cell", id: cell.ID}
				label := "[" + cell.Label + "]"
				if cell.Disabled {
					label = "(" + cell.Label + ")"
					row = append(row, bloom.Fixed(6, bloom.Text(m.kit.Muted(ui.Sanitize(label)))))
					continue
				}
				node := m.controlText(label, m.focus.Is(control)).ID(control).Focusable().On(
					controlMouse(id, control, m),
					on.Press(func() tea.Cmd {
						m.activateCell(id, cell)
						return nil
					}),
				)
				row = append(row, bloom.Fixed(6, node))
			}
			children = append(children, bloom.Fixed(1, bloom.Row(row...)))
		}
	}
	if p.view.Status != "" {
		children = append(children, fixedText("Status: "+p.view.Status, m))
	}
	return bloom.Column(children...)
}

func (m *Model) controlText(text string, focused bool) bloom.TextNode {
	text = ui.Sanitize(text)
	if focused {
		return bloom.Text(m.kit.FocusText(text))
	}
	return bloom.Text(m.kit.Text(text))
}

func fixedText(text string, m *Model) bloom.Child {
	return bloom.Fixed(1, bloom.Text(m.kit.Text(ui.Sanitize(text))).Truncate())
}

// Frame exposes adapter-local solved geometry for tests and demonstrations.
func (m *Model) Frame() *bloom.Frame { return m.root.Frame() }

// LayerRect returns the solved on-screen rectangle for an open window.
func (m *Model) LayerRect(id windows.ID) (image.Rectangle, bool) {
	return m.root.Frame().Rect(layerID{window: id})
}
