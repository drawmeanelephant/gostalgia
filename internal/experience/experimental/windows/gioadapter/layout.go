package gioadapter

import (
	"fmt"
	"image"
	"image/color"
	"strings"

	"gioui.org/gesture"
	"gioui.org/io/event"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/io/semantic"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/op/clip"
	"gioui.org/op/paint"
	"gioui.org/unit"
	"gioui.org/widget"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// gioColors is the theme palette resolved to paint colors. The kit's lipgloss
// output cannot draw into a Gio frame; only its color tokens carry over.
type gioColors struct {
	canvas, surface, text, muted, accent, border color.NRGBA
	focusText, focusFill                         color.NRGBA
}

func resolveColors(th theme.Theme) gioColors {
	p := th.Palette
	f := th.Focus
	nrgba := func(c color.Color) color.NRGBA { return color.NRGBAModel.Convert(c).(color.NRGBA) }
	return gioColors{
		canvas:    nrgba(p.Canvas),
		surface:   nrgba(p.Surface),
		text:      nrgba(p.Text),
		muted:     nrgba(p.Muted),
		accent:    nrgba(p.Accent),
		border:    nrgba(p.Border),
		focusText: nrgba(f.Text),
		focusFill: nrgba(f.Fill),
	}
}

const (
	titleH   = 20
	bodyPad  = 6
	textSize = unit.Sp(12)
)

// base fills the canvas and draws the status band at the bottom. It declares
// no event tags, so presses on it hit nothing.
func (m *Model) base(gtx layout.Context) {
	paint.FillShape(gtx.Ops, m.colors.canvas, clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Op())
	if m.size.Y <= statusBand {
		return
	}
	defer op.Offset(image.Pt(2, m.size.Y-statusBand)).Push(gtx.Ops).Pop()
	status := oneLine("EXPERIMENTAL WINDOW SPIKE (GIO)  " + m.status)
	help := "F6 focus  Alt-arrows move  Ctrl-W close  Tab controls  Enter act  Ctrl-C exit"
	layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.label(gtx, status, m.colors.accent)
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.label(gtx, help, m.colors.muted)
		}),
	)
}

// window lays out one open window at its bounds. The whole frame is a clip
// area carrying the bg tag so presses anywhere inside raise the window; child
// controls record their own areas later in the op stream and therefore win
// the hit test.
func (m *Model) window(gtx layout.Context, w windows.Window) {
	ws := m.states[w.ID]
	if ws == nil {
		return
	}
	defer op.Offset(image.Pt(w.Bounds.X, w.Bounds.Y)).Push(gtx.Ops).Pop()
	gtx.Constraints = layout.Exact(image.Pt(w.Bounds.Width, w.Bounds.Height))
	area := clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Push(gtx.Ops)
	semantic.LabelOp(oneLine(w.Title)).Add(gtx.Ops)
	semantic.DescriptionOp("window").Add(gtx.Ops)
	event.Op(gtx.Ops, ws.bg)
	for {
		ev, ok := gtx.Event(pointer.Filter{Target: ws.bg, Kinds: pointer.Press})
		if !ok {
			break
		}
		if _, ok := ev.(pointer.Event); ok {
			m.activate(gtx, w.ID, controlID{})
		}
	}
	m.chrome(gtx, w, ws)
	area.Pop()
}

func (m *Model) chrome(gtx layout.Context, w windows.Window, ws *windowState) {
	// The border is a one-pixel theme-colored frame drawn behind inset content;
	// widget.Border is not used so the fill order stays explicit.
	full := gtx.Constraints.Max
	paint.FillShape(gtx.Ops, m.colors.border, clip.Rect(image.Rectangle{Max: full}).Op())
	defer op.Offset(image.Pt(1, 1)).Push(gtx.Ops).Pop()
	gtx.Constraints = layout.Exact(full.Sub(image.Pt(2, 2)))
	paint.FillShape(gtx.Ops, m.colors.surface, clip.Rect(image.Rectangle{Max: gtx.Constraints.Max}).Op())
	layout.Flex{Axis: layout.Vertical}.Layout(gtx,
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.titleBar(gtx, w, ws)
		}),
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			return layout.UniformInset(unit.Dp(bodyPad)).Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return m.body(gtx, w, ws)
			})
		}),
	)
}

// titleBar draws the drag zone and the close control. Only the title owns the
// drag handler, like the bloom adapter: the close control must not move the
// window.
func (m *Model) titleBar(gtx layout.Context, w windows.Window, ws *windowState) layout.Dimensions {
	gtx.Constraints.Min.Y = titleH
	gtx.Constraints.Max.Y = titleH
	focused, _ := m.service.Focused()
	return layout.Flex{Axis: layout.Horizontal, Alignment: layout.Middle}.Layout(gtx,
		layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
			m.titleEvents(gtx, w, ws)
			title := oneLine(w.Title)
			if focused == w.ID {
				title = m.th.Focus.Marker + " " + title
			}
			dims := layout.Inset{Left: unit.Dp(4), Top: unit.Dp(2)}.Layout(gtx, func(gtx layout.Context) layout.Dimensions {
				return m.label(gtx, title, m.colors.accent)
			})
			dims.Size.Y = titleH
			defer clip.Rect(image.Rectangle{Max: dims.Size}).Push(gtx.Ops).Pop()
			semantic.LabelOp(oneLine(w.Title)).Add(gtx.Ops)
			semantic.DescriptionOp("title").Add(gtx.Ops)
			ws.title.Add(gtx.Ops)
			return dims
		}),
		layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.control(gtx, ws.close, "Close "+oneLine(w.Title), func(gtx layout.Context) layout.Dimensions {
				return m.label(gtx, "[X]", m.colors.text)
			})
		}),
	)
}

// titleEvents drains the title's drag gesture. A press activates the window;
// motion applies the delta through the same clamped Move transition the
// keyboard equivalent uses. The gesture tag is stable across frames, so
// raising a window cannot lose capture.
func (m *Model) titleEvents(gtx layout.Context, w windows.Window, ws *windowState) {
	for {
		ev, ok := ws.title.Update(gtx.Metric, gtx.Source, gesture.Both)
		if !ok {
			break
		}
		switch ev.Kind {
		case pointer.Press:
			m.drags[w.ID] = dragState{start: ev.Position.Round(), origin: w.Bounds}
			m.activate(gtx, w.ID, controlID{})
		case pointer.Drag:
			d, ok := m.drags[w.ID]
			if !ok {
				continue
			}
			b := d.origin
			b.X += ev.Position.Round().X - d.start.X
			b.Y += ev.Position.Round().Y - d.start.Y
			m.service.Move(w.ID, m.clampBounds(b))
		case pointer.Release, pointer.Cancel:
			delete(m.drags, w.ID)
		}
	}
}

// control draws one clickable, focusable region and routes its pointer and
// key events. A press activates the window and this control; a completed
// click acts like Enter, matching the bloom adapter's Press activation.
func (m *Model) control(gtx layout.Context, c *control, label string, w layout.Widget) layout.Dimensions {
	for {
		ev, ok := c.click.Update(gtx.Source)
		if !ok {
			break
		}
		switch ev.Kind {
		case gesture.KindPress:
			m.activate(gtx, c.id.window, c.id)
		case gesture.KindClick:
			m.press(gtx, c.id)
		}
	}
	for {
		ev, ok := gtx.Event(
			key.FocusFilter{Target: c},
			key.Filter{Focus: c, Name: key.NameReturn},
			key.Filter{Focus: c, Name: key.NameEnter},
			key.Filter{Focus: c, Name: key.NameSpace},
			key.Filter{Focus: c, Name: key.NameDeleteBackward},
			key.Filter{Focus: c, Name: key.NameUpArrow},
			key.Filter{Focus: c, Name: key.NameDownArrow},
			key.Filter{Focus: c, Name: key.NameLeftArrow},
			key.Filter{Focus: c, Name: key.NameRightArrow},
		)
		if !ok {
			break
		}
		m.controlKey(gtx, c, ev)
	}
	dims := w(gtx)
	defer clip.Rect(image.Rectangle{Max: dims.Size}).Push(gtx.Ops).Pop()
	class := semantic.Button
	if c.id.kind == "field" {
		class = semantic.Editor
	}
	semantic.ClassOp(class).Add(gtx.Ops)
	semantic.LabelOp(label).Add(gtx.Ops)
	semantic.DescriptionOp(c.id.kind).Add(gtx.Ops)
	semantic.EnabledOp(true).Add(gtx.Ops)
	c.click.Add(gtx.Ops)
	event.Op(gtx.Ops, c)
	return dims
}

func (m *Model) controlKey(gtx layout.Context, c *control, ev event.Event) {
	id := c.id
	p := m.presentations[id.window]
	switch e := ev.(type) {
	case key.FocusEvent:
		if e.Focus {
			m.current = c
			m.scope = id.window
			m.syncGridFocus(id.window)
		} else if m.current == c {
			m.current = nil
		}
	case key.EditEvent:
		if p == nil || id.kind != "field" {
			return
		}
		value := p.values[id.id] + strings.ReplaceAll(e.Text, "\n", " ")
		if len(value) <= 4096 {
			p.values[id.id] = value
		}
	case key.Event:
		if e.State != key.Press || p == nil {
			return
		}
		switch e.Name {
		case key.NameDeleteBackward:
			if id.kind == "field" {
				p.values[id.id] = trimLastCluster(p.values[id.id])
			}
		case key.NameUpArrow:
			p.item = max(0, p.item-1)
		case key.NameDownArrow:
			p.item = min(max(0, len(p.view.Items)-1), p.item+1)
		case key.NameLeftArrow, key.NameRightArrow:
			if id.kind == "cell" {
				delta := 1
				if e.Name == key.NameLeftArrow {
					delta = -1
				}
				m.moveCell(gtx, id, delta)
			}
		case key.NameReturn, key.NameEnter, key.NameSpace:
			m.press(gtx, id)
		}
	}
}

// moveCell implements the bloom adapter's grid arrow navigation: linear delta,
// clamped, skipping disabled cells, with key focus following the selection.
func (m *Model) moveCell(gtx layout.Context, id controlID, delta int) {
	p := m.presentations[id.window]
	ws := m.states[id.window]
	if p == nil || ws == nil || p.view.Grid == nil || len(p.view.Grid.Cells) == 0 {
		return
	}
	m.syncGridFocus(id.window)
	next := max(0, min(len(p.view.Grid.Cells)-1, p.cell+delta))
	if next == p.cell || p.view.Grid.Cells[next].Disabled {
		return
	}
	p.cell = next
	if c := ws.cells[p.view.Grid.Cells[p.cell].ID]; c != nil {
		m.current = c
		m.remembered[id.window] = c.id
		gtx.Execute(key.FocusCmd{Tag: c})
	}
}

// body renders the presentation subset the fixtures use, in the same order as
// the bloom adapter: items, fields, actions, blocks, meters, grid, status.
func (m *Model) body(gtx layout.Context, w windows.Window, ws *windowState) layout.Dimensions {
	p := m.presentations[w.ID]
	if p == nil {
		return layout.Dimensions{}
	}
	var children []layout.FlexChild
	line := func(text string, c color.NRGBA) layout.FlexChild {
		return layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.label(gtx, text, c)
		})
	}
	for i, item := range p.view.Items {
		marker := "  "
		if i == p.item {
			marker = "> "
		}
		children = append(children, line(marker+oneLine(item.Label+" — "+item.Detail), m.colors.text))
	}
	for _, f := range p.view.Fields {
		field := f
		c := ws.fields[field.ID]
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.control(gtx, c, oneLine(field.Label), func(gtx layout.Context) layout.Dimensions {
				return m.controlLine(gtx, c, oneLine(field.Label+": ["+p.values[field.ID]+"]"))
			})
		}))
	}
	for _, a := range p.view.Actions {
		action := a
		if action.Disabled {
			children = append(children, line("("+oneLine(action.Label)+")", m.colors.muted))
			continue
		}
		c := ws.actions[action.ID]
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.control(gtx, c, oneLine(action.Label), func(gtx layout.Context) layout.Dimensions {
				return m.controlLine(gtx, c, "["+oneLine(action.Label)+"]")
			})
		}))
	}
	for _, b := range p.view.Blocks {
		text := ""
		if b.Label != "" {
			text = b.Label + "\n"
		}
		text += b.Text
		block := ui.Sanitize(text)
		children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
			return m.label(gtx, block, m.colors.text)
		}))
	}
	for _, meter := range p.view.Meters {
		children = append(children, line(fmt.Sprintf("%s %.0f%%", oneLine(meter.Label), min(max(meter.Value, 0), 1)*100), m.colors.text))
	}
	if g := p.view.Grid; g != nil && len(g.Cells) > 0 {
		children = append(children, line(oneLine(g.Label), m.colors.text))
		columns := max(1, g.Columns)
		for start := 0; start < len(g.Cells); start += columns {
			end := min(len(g.Cells), start+columns)
			row := make([]layout.FlexChild, 0, end-start)
			for _, cell := range g.Cells[start:end] {
				cell := cell
				if cell.Disabled {
					row = append(row, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
						return m.label(gtx, "("+oneLine(cell.Label)+")", m.colors.muted)
					}))
					continue
				}
				c := ws.cells[cell.ID]
				row = append(row, layout.Flexed(1, func(gtx layout.Context) layout.Dimensions {
					return m.control(gtx, c, oneLine(cell.Label), func(gtx layout.Context) layout.Dimensions {
						return m.controlLine(gtx, c, "["+oneLine(cell.Label)+"]")
					})
				}))
			}
			children = append(children, layout.Rigid(func(gtx layout.Context) layout.Dimensions {
				return layout.Flex{Axis: layout.Horizontal}.Layout(gtx, row...)
			}))
		}
	}
	if p.view.Status != "" {
		children = append(children, line("Status: "+oneLine(p.view.Status), m.colors.text))
	}
	return layout.Flex{Axis: layout.Vertical}.Layout(gtx, children...)
}

// controlLine draws control text; focused controls paint the theme focus fill
// behind the line, recorded before the text so the glyphs stay on top.
func (m *Model) controlLine(gtx layout.Context, c *control, text string) layout.Dimensions {
	focused := m.current == c || gtx.Focused(c)
	fg := m.colors.text
	if focused {
		fg = m.colors.focusText
	}
	macro := op.Record(gtx.Ops)
	dims := m.label(gtx, text, fg)
	call := macro.Stop()
	if focused {
		paint.FillShape(gtx.Ops, m.colors.focusFill, clip.Rect(image.Rectangle{Max: dims.Size}).Op())
	}
	call.Add(gtx.Ops)
	return dims
}

func (m *Model) label(gtx layout.Context, text string, c color.NRGBA) layout.Dimensions {
	macro := op.Record(gtx.Ops)
	paint.ColorOp{Color: c}.Add(gtx.Ops)
	mat := macro.Stop()
	return widget.Label{}.Layout(gtx, m.shaper, m.font, textSize, text, mat)
}
