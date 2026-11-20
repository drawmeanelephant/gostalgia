package gioadapter

import (
	"fmt"

	"gostalgia/sdk"
)

// presentationState mirrors the bloom adapter: the adapter owns selection and
// draft state for the unchanged SDK snapshot.
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
