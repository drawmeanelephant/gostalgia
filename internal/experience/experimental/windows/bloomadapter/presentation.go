package bloomadapter

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/windows"
	"gostalgia/sdk"
)

type presentationState struct {
	view       sdk.View
	values     map[string]string
	item       int
	cell       int
	requestSeq int
	actions    []sdk.ActionRequest
}

func newPresentationState(view sdk.View) *presentationState {
	p := &presentationState{view: view, values: make(map[string]string)}
	for _, field := range view.Fields {
		p.values[field.ID] = field.Value
	}
	return p
}

func (p *presentationState) request(view sdk.View, action, cellID string) sdk.ActionRequest {
	p.requestSeq++
	values := make(map[string]string, len(p.values))
	for id, value := range p.values {
		values[id] = value
	}
	request := sdk.ActionRequest{
		Version:   view.Version,
		Instance:  view.Instance,
		RequestID: fmt.Sprintf("experimental-%d", p.requestSeq),
		Action:    action,
		Values:    values,
		CellID:    cellID,
	}
	if len(view.Items) > 0 {
		request.ItemID = view.Items[min(p.item, len(view.Items)-1)].ID
	}
	p.actions = append(p.actions, cloneRequest(request))
	p.view.Status = action + ": " + values["text"]
	return request
}

func (m *Model) presentationKey(msg tea.Msg) {
	id, ok := m.service.Focused()
	if !ok {
		return
	}
	p := m.presentations[id]
	if p == nil {
		return
	}
	switch {
	case isKey(msg, "up"):
		p.item = max(0, p.item-1)
	case isKey(msg, "down"):
		p.item = min(max(0, len(p.view.Items)-1), p.item+1)
	case isKey(msg, "left", "right", "shift+left", "shift+right"):
		m.moveCell(p, msg)
	case isKey(msg, "backspace"):
		if control, ok := m.focus.Current(); ok && control.window == id && control.kind == "field" {
			p.values[control.id] = trimLastCluster(p.values[control.id])
		}
	default:
		text := keyText(msg)
		if text == "" {
			return
		}
		control, ok := m.focus.Current()
		if !ok || control.window != id || control.kind != "field" {
			return
		}
		value := p.values[control.id] + strings.ReplaceAll(text, "\n", " ")
		if len(value) <= 4096 {
			p.values[control.id] = value
		}
	}
}

func (m *Model) moveCell(p *presentationState, msg tea.Msg) {
	if p.view.Grid == nil || len(p.view.Grid.Cells) == 0 {
		return
	}
	control, ok := m.focus.Current()
	if !ok || control.kind != "cell" {
		return
	}
	m.syncGridFocus()
	delta := 1
	if isKey(msg, "left", "shift+left") {
		delta = -1
	}
	next := max(0, min(len(p.view.Grid.Cells)-1, p.cell+delta))
	if next == p.cell || p.view.Grid.Cells[next].Disabled {
		return
	}
	p.cell = next
	m.focus.Set(controlID{window: control.window, kind: "cell", id: p.view.Grid.Cells[p.cell].ID})
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
