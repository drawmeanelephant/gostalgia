package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/events"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/ipc"
	"gostalgia/sdk"
)

type callerFunc func(context.Context, string, any, any) error

func (f callerFunc) Call(ctx context.Context, method string, params, out any) error {
	return f(ctx, method, params, out)
}

func screenData() sdk.View {
	return sdk.View{
		Version: 1, Instance: "launch", Title: "Screen", State: sdk.ViewReady,
		Items:   []sdk.Item{{ID: "one", Label: "One", Detail: "first"}, {ID: "two", Label: "Two", Detail: "second"}},
		Fields:  []sdk.Field{{ID: "text", Label: "Text", Required: true}},
		Actions: []sdk.Action{{ID: "send", Label: "Send"}},
	}
}

// padData is a version-2 snapshot exercising every new element: a multi-line
// art block, meters (one out of range for the clamp), and a grid pad with a
// cell-declared action, a disabled cell, and a datum cell.
func padData() sdk.View {
	return sdk.View{
		Version: 2, Instance: "launch", Title: "Pad", State: sdk.ViewReady,
		Items:   []sdk.Item{{ID: "one", Label: "One", Detail: "first"}},
		Fields:  []sdk.Field{{ID: "text", Label: "Text"}},
		Actions: []sdk.Action{{ID: "adopt", Label: "Adopt"}},
		Blocks:  []sdk.Block{{ID: "art", Label: "Mascot", Text: "(\\_/)\n(o.o)\n> ^ <"}},
		Meters: []sdk.Meter{
			{ID: "charge", Label: "Charge", Value: 0.5},
			{ID: "hunger", Label: "Hunger", Value: 1.4},
		},
		Grid: &sdk.Grid{Label: "Pad", Columns: 2, Cells: []sdk.Cell{
			{ID: "nw", Label: "NW", Action: "press"},
			{ID: "ne", Label: "NE", Action: "press"},
			{ID: "sw", Label: "SW"},
			{ID: "se", Label: "SE", Disabled: true},
		}},
		Status: "waiting",
	}
}

func screenStatus() appStatus {
	var a appStatus
	a.Manifest.ID, a.Manifest.Name, a.Running, a.PID = "com.test.screen", "Screen", true, 42
	return a
}

func writeJSON(out, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func TestPresentationLoadingFocusAndSemanticActions(t *testing.T) {
	var received sdk.ActionRequest
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		v := screenData()
		if strings.HasSuffix(method, "/action") {
			received = params.(sdk.ActionRequest)
			v.Status = "Sent " + received.Values["text"]
		}
		return writeJSON(out, v)
	}), nil)
	a := screenStatus()
	m.apps = []appStatus{a}
	_, open := m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if m.presentation == nil || !strings.Contains(m.View().Content, "Loading") {
		t.Fatal("opening has no loading state")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "ignored while loading"})
	batch := open().(tea.BatchMsg)
	m.Update(batch[0]())
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "héllo"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, action := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action == nil || !m.presentation.busy || len(m.input) != 0 {
		t.Fatal("app did not own focus while shell retained terminal ownership")
	}
	m.Update(action())
	if received.Action != "send" || received.Values["text"] != "héllo" || received.ItemID != "two" ||
		received.Instance != "launch" || received.Version != 1 || received.RequestID == "" {
		t.Fatalf("semantic action routing: %+v", received)
	}
	if m.presentation.busy || !strings.Contains(m.View().Content, "Sent héllo") {
		t.Fatal("action snapshot not rendered")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.presentation != nil {
		t.Fatal("Esc did not return focus to the shell")
	}
}

func TestPresentationCancellationAndLateReplies(t *testing.T) {
	var canceled sdk.CancelRequest
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		if strings.HasSuffix(method, "/cancel") {
			canceled = params.(sdk.CancelRequest)
			return nil
		}
		return writeJSON(out, screenData())
	}), nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	m.Update(viewMsg{epoch: epoch, data: screenData()})
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "x"})
	_, action := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if action == nil {
		t.Fatal("action not dispatched")
	}
	oldSeq, request := m.presentation.seq, m.presentation.request
	_, cancel := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cancel == nil {
		t.Fatal("no cancellation route")
	}
	m.Update(cancel())
	if canceled.RequestID != request || canceled.Instance != "launch" || m.presentation.busy {
		t.Fatal("Esc did not cancel the owning operation")
	}
	late := screenData()
	late.Status = "late reply must not render"
	m.Update(viewMsg{epoch: epoch, seq: oldSeq, data: late, action: true})
	if strings.Contains(m.View().Content, late.Status) {
		t.Fatal("canceled operation replaced current view")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m.openView(screenStatus())
	m.Update(viewMsg{epoch: epoch, data: late})
	if m.presentation.data.Status == late.Status {
		t.Fatal("dismissed view's reply reopened it")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.presentation != nil || !m.shelf {
		t.Fatal("global shelf hotkey lost ownership")
	}
}

func TestPresentationLifecycleRetraction(t *testing.T) {
	for _, cause := range []string{"exit", "relaunch", "transport", "instance"} {
		t.Run(cause, func(t *testing.T) {
			m := New(context.Background(), noopCaller{}, nil)
			a := screenStatus()
			m.openView(a)
			epoch := m.presentation.epoch
			m.Update(viewMsg{epoch: epoch, data: screenData()})
			msg := viewCheckMsg{epoch: epoch, apps: []appStatus{a}}
			switch cause {
			case "exit":
				msg.apps[0].Running = false
			case "relaunch":
				msg.apps[0].PID++
			case "transport":
				msg.err = errors.New("connection closed")
			case "instance":
				data := screenData()
				data.Instance = "new-launch"
				msg.data = &data
			}
			m.Update(msg)
			if m.presentation != nil || !strings.Contains(m.View().Content, "view") {
				t.Fatal("lost app still owns focus or view")
			}
			m.Update(viewMsg{epoch: epoch, data: screenData()})
			if m.presentation != nil {
				t.Fatal("late lifecycle reply restored retracted view")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "prompt works"})
			if string(m.input) != "prompt works" {
				t.Fatal("shell focus was not restored")
			}
		})
	}
}

func TestPresentationErrorsBoundsAndTerminalControls(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	m.Update(viewMsg{epoch: epoch, err: errors.New("view unavailable")})
	if !strings.Contains(m.View().Content, "view unavailable") {
		t.Fatal("load error banner missing")
	}
	data := screenData()
	data.Title += "\x1b]52;c;clipboard\a"
	data.Error = "failure\r\x1b[31m"
	data.State = sdk.ViewError
	data.Items[0].Detail = strings.Repeat("界", 5000)
	m.Update(viewMsg{epoch: epoch, data: data})
	for _, size := range [][2]int{{80, 24}, {30, 10}, {20, 8}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		if strings.Contains(view, "\a") || strings.Contains(view, "\r") || strings.Contains(view, "clipboard") {
			t.Fatal("app-controlled terminal sequences were rendered")
		}
		if lipgloss.Height(view) > size[1] || lipgloss.Width(view) > size[0] {
			t.Fatal("app view escaped terminal bounds")
		}
	}
	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.presentation.data.Title != data.Title {
		t.Fatal("global quit was routed to an app")
	}
}

func TestPresentationRefreshPreservesSemanticFocusAndVisibleSelection(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.openView(screenStatus())
	m.applyView(screenData())
	v := m.presentation
	v.values["text"], v.focus, v.item = "draft", 1, 1
	data := screenData()
	data.Fields = append([]sdk.Field{{ID: "new", Label: "New"}}, data.Fields...)
	data.Items[0], data.Items[1] = data.Items[1], data.Items[0]
	m.applyView(data)
	if v.values["text"] != "draft" || v.focus != 2 || v.item != 0 || v.data.Items[v.item].ID != "two" {
		t.Fatal("refresh moved semantic focus/selection or discarded a draft")
	}
	data.Items = nil
	for i := 0; i < 100; i++ {
		data.Items = append(data.Items, sdk.Item{ID: fmt.Sprintf("item-%d", i), Label: fmt.Sprintf("ROW-%03d", i)})
	}
	m.applyView(data)
	m.height = 16
	if !strings.Contains(m.View().Content, "[Send]") {
		t.Fatal("focused action hidden below a long list")
	}
	v.item, v.itemFocus = 99, true
	if !strings.Contains(m.View().Content, "ROW-099") {
		t.Fatal("selected item hidden below a long list")
	}
	m.Update(viewCheckMsg{epoch: v.epoch, apps: []appStatus{screenStatus()}, viewErr: errors.New("temporary snapshot failure")})
	if m.presentation == nil || !strings.Contains(m.View().Content, "temporary snapshot failure") {
		t.Fatal("recoverable snapshot failure did not keep an error banner")
	}
}

// Regression for #179: an item with no Detail rendered a dangling colon.
func TestPresentationItemWithoutDetailRendersNoColon(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	data := screenData()
	data.Items = []sdk.Item{
		{ID: "one", Label: "THE BRASS ELEPHANT"},
		{ID: "two", Label: "Two", Detail: "second"},
	}
	m.presentation = &appView{data: data, instance: data.Instance, values: map[string]string{}}
	view := ansi.Strip(m.View().Content)
	if strings.Contains(view, "THE BRASS ELEPHANT:") {
		t.Fatalf("item without detail rendered a dangling colon:\n%s", view)
	}
	if !strings.Contains(view, "Two: second") {
		t.Fatalf("item with detail lost its join:\n%s", view)
	}
}

func TestPresentationUsesExplicitThemeAndFocusTokens(t *testing.T) {
	tokens := theme.Nostalgia()
	tokens.Focus.Marker = "*"
	custom := NewWithTheme(context.Background(), noopCaller{}, nil, tokens, ui.Plain)
	data := screenData()
	data.Actions[0].Disabled = true
	custom.presentation = &appView{
		data: data, instance: data.Instance, focus: 1,
		values: map[string]string{"text": "e\u0301👩‍💻界"},
	}
	view := custom.View().Content
	if strings.Contains(view, "\x1b") || !strings.Contains(view, "* One") ||
		!strings.Contains(view, "[Send] (disabled)") || strings.Contains(view, "* [Send]") ||
		!strings.Contains(view, "(Enter) ACTION") {
		t.Fatalf("app view ignored plain mode, focus marker, or disabled state:\n%s", view)
	}
	var rendered []string
	for _, tokens := range []theme.Theme{theme.Nostalgia(), theme.Midnight()} {
		m := NewWithTheme(context.Background(), noopCaller{}, nil, tokens, ui.TrueColor)
		m.presentation = &appView{
			data: data, instance: data.Instance, values: map[string]string{"text": "e\u0301👩‍💻界"},
		}
		rendered = append(rendered, m.View().Content)
		for _, size := range []tea.WindowSizeMsg{
			{Width: 30, Height: 10}, {Width: 80, Height: 24}, {Width: 120, Height: 40}, {Width: 240, Height: 80},
		} {
			m.Update(size)
			if view := m.View().Content; lipgloss.Width(view) != size.Width || lipgloss.Height(view) != size.Height {
				t.Fatalf("app view escaped %dx%d bounds", size.Width, size.Height)
			}
		}
	}
	if rendered[0] == rendered[1] || ansi.Strip(rendered[0]) != ansi.Strip(rendered[1]) {
		t.Fatal("app palettes must differ without changing layout")
	}
}

func TestPresentationGridNavigationAndCellActions(t *testing.T) {
	var received sdk.ActionRequest
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		v := padData()
		if strings.HasSuffix(method, "/action") {
			received = params.(sdk.ActionRequest)
			v.Status = "pressed " + received.CellID
		}
		return writeJSON(out, v)
	}), nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	m.Update(viewMsg{epoch: epoch, data: padData()})
	v := m.presentation
	if v.version != 2 {
		t.Fatalf("negotiated version = %d, want 2", v.version)
	}
	// The grid is the last tab stop: field, action, then grid.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if !v.gridFocused() {
		t.Fatal("tab ring did not reach the grid stop")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	if v.cell != 1 {
		t.Fatalf("cell = %d, want 1", v.cell)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("cell enter did not dispatch its action")
	}
	m.Update(cmd())
	if received.Action != "press" || received.CellID != "ne" || received.Version != 2 ||
		received.Instance != "launch" || received.RequestID == "" {
		t.Fatalf("cell action routing: %+v", received)
	}
	// Down lands on the disabled SE cell; activation is a no-op.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("disabled cell dispatched an action")
	}
	// Left lands on the datum cell (no action); activation is a no-op.
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("datum cell dispatched an action")
	}
	// Up clamps back to NW rather than wrapping.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if v.cell != 0 {
		t.Fatalf("cell = %d after clamping, want 0", v.cell)
	}
	// A list action rides along with the selected cell_id and item_id.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if v.gridFocused() {
		t.Fatal("shift+tab stayed on the grid")
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("focused action did not dispatch")
	}
	m.Update(cmd())
	if received.Action != "adopt" || received.CellID != "nw" || received.ItemID != "one" {
		t.Fatalf("list action lost selection: %+v", received)
	}
	// Refresh preserves grid focus and selection by ID.
	v.focus = len(v.data.Fields) + len(v.data.Actions)
	v.cell = 3
	data := padData()
	data.Grid.Cells[0], data.Grid.Cells[3] = data.Grid.Cells[3], data.Grid.Cells[0]
	m.applyView(data)
	if !v.gridFocused() || v.data.Grid.Cells[v.cell].ID != "se" {
		t.Fatalf("refresh lost grid focus/selection: focus=%d cell=%d", v.focus, v.cell)
	}
	// After the swap the disabled se cell sits at index 0; a list action must
	// not attach its cell_id, which the SDK would reject as meaningless input.
	v.cell = 0
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("list action did not dispatch with a disabled cell selected")
	}
	m.Update(cmd())
	if received.Action != "adopt" || received.CellID != "" {
		t.Fatalf("disabled cell leaked cell_id into a list action: %+v", received)
	}
}

func TestPresentationVersionNegotiationFallback(t *testing.T) {
	var requested []int
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		if strings.HasSuffix(method, "/view") {
			p := params.(sdk.ViewRequest)
			requested = append(requested, p.Version)
			if p.Version > 1 {
				return errors.New("presentation: unsupported version 2")
			}
		}
		return writeJSON(out, screenData())
	}), nil)
	batch := m.openView(screenStatus())().(tea.BatchMsg)
	m.Update(batch[0]())
	if v := m.presentation; v == nil || v.version != 1 {
		t.Fatalf("negotiated version = %+v", m.presentation)
	}
	if len(requested) != 3 || requested[0] != 3 || requested[1] != 2 || requested[2] != 1 {
		t.Fatalf("negotiation requests = %v, want [3 2 1]", requested)
	}
	// A version-2 app answers at 2 without a fallback request.
	m2 := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		return writeJSON(out, padData())
	}), nil)
	batch = m2.openView(screenStatus())().(tea.BatchMsg)
	m2.Update(batch[0]())
	if v := m2.presentation; v == nil || v.version != 2 || v.data.Grid == nil {
		t.Fatalf("version-2 negotiation: %+v", m2.presentation)
	}
}

func TestPresentationRenegotiatesAfterFailedOpen(t *testing.T) {
	var requested []int
	failV1 := true
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		switch {
		case method == "app/list":
			return writeJSON(out, []appStatus{screenStatus()})
		case strings.HasSuffix(method, "/view"):
			p := params.(sdk.ViewRequest)
			requested = append(requested, p.Version)
			if p.Version > 1 {
				return errors.New("presentation: unsupported version 2")
			}
			if failV1 {
				failV1 = false
				return errors.New("i/o timeout")
			}
		}
		return writeJSON(out, screenData())
	}), nil)
	batch := m.openView(screenStatus())().(tea.BatchMsg)
	m.Update(batch[0]())
	v := m.presentation
	if v == nil || v.instance != "" || v.data.State != sdk.ViewError {
		t.Fatalf("open failure did not leave an unnegotiated error view: %+v", v)
	}
	// The poll renegotiates instead of hammering a version-1 app at version 2
	// forever; the stamped version is adopted on recovery.
	m.Update(m.checkView()())
	if v.instance != "launch" || v.version != 1 || v.data.State != sdk.ViewReady {
		t.Fatalf("renegotiation did not recover: %+v", v)
	}
	if len(requested) != 6 ||
		requested[0] != 3 || requested[1] != 2 || requested[2] != 1 ||
		requested[3] != 3 || requested[4] != 2 || requested[5] != 1 {
		t.Fatalf("negotiation requests = %v, want [3 2 1 3 2 1]", requested)
	}
}

func TestPresentationRejectsNewerStampedVersion(t *testing.T) {
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		if method == "app/list" {
			return writeJSON(out, []appStatus{screenStatus()})
		}
		return writeJSON(out, padData()) // always stamps version 2
	}), nil)
	m.openView(screenStatus())
	v := m.presentation
	// A v1-negotiated session must refuse a v2-stamped reply: a raw-IPC app
	// cannot serve elements the shell never asked for.
	v.instance, v.version = "launch", 1
	m.Update(m.checkView()())
	if v.banner == "" || v.data.Grid != nil {
		t.Fatalf("newer-stamped snapshot adopted: banner=%q grid=%v", v.banner, v.data.Grid)
	}
}

func TestFetchViewStepsDownAndRejectsNewerStamp(t *testing.T) {
	var requested []int
	client := callerFunc(func(ctx context.Context, method string, params, out any) error {
		p := params.(sdk.ViewRequest)
		requested = append(requested, p.Version)
		if p.Version > 2 {
			return errors.New("presentation: unsupported version 3")
		}
		return writeJSON(out, padData())
	})
	data, version, err := fetchView(context.Background(), client, "com.test.screen", 3)
	if err != nil || version != 2 || data.Version != 2 {
		t.Fatalf("fetchView = %v, %d, %v", data, version, err)
	}
	if len(requested) != 2 || requested[0] != 3 || requested[1] != 2 {
		t.Fatalf("step-down requests = %v, want [3 2]", requested)
	}
	// A reply stamped newer than requested is refused outright.
	_, _, err = fetchView(context.Background(), client, "com.test.screen", 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("newer stamp accepted: %v", err)
	}
}

func TestPresentationV2ElementsStayInBounds(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	data := padData()
	// Wide-but-valid art: 32 full-width lines sit inside the line/byte budget,
	// and the contract itself must accept them before the renderer clips.
	data.Blocks[0].Text = strings.Repeat("界"+strings.Repeat("=", 200)+"\n", 32)
	data.Grid.Label += "\x1b[31m\r"
	if err := data.Validate(); err != nil {
		t.Fatalf("oversized-but-legal art rejected: %v", err)
	}
	// What the renderer cannot host, the contract rejects.
	for _, text := range []string{strings.Repeat("x\n", 33), strings.Repeat("x", 16385)} {
		bad := padData()
		bad.Blocks[0].Text = text
		if err := bad.Validate(); err == nil {
			t.Fatalf("contract accepted a block of %d bytes", len(text))
		}
	}
	m.Update(viewMsg{epoch: epoch, data: data})
	for _, size := range [][2]int{{80, 24}, {30, 10}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View().Content
		if strings.Contains(view, "\a") || strings.Contains(view, "\r") {
			t.Fatal("version-2 elements leaked terminal controls")
		}
		if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
			t.Fatalf("version-2 view escaped %dx%d bounds", size[0], size[1])
		}
	}
}

// areaData is a version-3 snapshot: a multiline field plus a scroll region
// holding a 40-line transcript.
func areaData() sdk.View {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%02d", i)
	}
	return sdk.View{
		Version: 3, Instance: "launch", Title: "Doc", State: sdk.ViewReady,
		Fields:  []sdk.Field{{ID: "body", Label: "Body", Multiline: true}},
		Actions: []sdk.Action{{ID: "save", Label: "Save"}},
		Region:  &sdk.Region{ID: "log", Label: "Transcript", Text: strings.Join(lines, "\n")},
		Status:  "ready",
	}
}

func TestPresentationV3MultilineFieldEditing(t *testing.T) {
	var received sdk.ActionRequest
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		v := areaData()
		if strings.HasSuffix(method, "/action") {
			received = params.(sdk.ActionRequest)
		}
		return writeJSON(out, v)
	}), nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	m.Update(viewMsg{epoch: epoch, data: areaData()})
	v := m.presentation
	if v.version != 3 {
		t.Fatalf("negotiated version = %d, want 3", v.version)
	}
	// Focus lands on the multiline field: runes edit, Enter is a newline,
	// not the first action. Actions still fire from their own focus stop.
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "first"})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("Enter inside a multiline field dispatched an action")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "second"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if v.values["body"] != "first\nsecon" {
		t.Fatalf("multiline draft = %q, want %q", v.values["body"], "first\nsecon")
	}
	m.height = 24
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "first") || !strings.Contains(view, "secon") {
		t.Fatalf("multiline draft not rendered on separate lines:\n%s", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("focused action did not dispatch")
	}
	m.Update(cmd())
	if received.Action != "save" || received.Values["body"] != "first\nsecon" || received.Version != 3 {
		t.Fatalf("multiline value lost in action routing: %+v", received)
	}
}

func TestPresentationV3RegionScrollAndPinning(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.presentation = &appView{
		data: areaData(), instance: "launch", version: 3,
		values: map[string]string{}, regionPinned: true,
	}
	v := m.presentation
	m.height = 16
	// The region is the last focus stop: field, action, then region.
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if !v.regionFocused() {
		t.Fatal("tab ring did not reach the region stop")
	}
	// Pinned shows the tail; the head is scrolled out.
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "L39") {
		t.Fatalf("pinned region lost its tail:\n%s", view)
	}
	if strings.Contains(view, "L00") {
		t.Fatalf("pinned region showed its head:\n%s", view)
	}
	tail := v.regionTop
	if tail == 0 {
		t.Fatal("region did not scroll to its tail")
	}
	// Up scrolls inside the region instead of moving item focus.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if v.regionPinned || v.regionTop != tail-1 || v.itemFocus {
		t.Fatalf("up did not scroll the region: top=%d pinned=%v itemFocus=%v", v.regionTop, v.regionPinned, v.itemFocus)
	}
	// Down back to the tail re-pins.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if !v.regionPinned || v.regionTop != tail {
		t.Fatalf("down did not re-pin the tail: top=%d pinned=%v", v.regionTop, v.regionPinned)
	}
	// Page keys move the whole window.
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if v.regionTop != tail-v.regionWindow {
		t.Fatalf("pgup moved %d lines, want %d", tail-v.regionTop, v.regionWindow)
	}
	// Enter on a region is a no-op.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("region dispatched an action")
	}
	// Unpinned: a fresh snapshot keeps the user's offset. Pinned: it follows
	// appended lines. A new region id resets to the tail.
	v.regionPinned = false
	v.regionTop = 3
	data := areaData()
	data.Region.Text += "\nL40\nL41"
	m.applyView(data)
	if v.regionTop != 3 || v.regionPinned {
		t.Fatalf("unpinned refresh moved the region: top=%d pinned=%v", v.regionTop, v.regionPinned)
	}
	v.regionPinned = true
	m.applyView(data)
	m.View()
	if !v.regionPinned || !strings.Contains(ansi.Strip(m.View().Content), "L41") {
		t.Fatal("pinned refresh did not follow the appended tail")
	}
	data.Region = &sdk.Region{ID: "other", Label: "Transcript", Text: data.Region.Text}
	m.applyView(data)
	if v.regionTop != 0 || !v.regionPinned {
		t.Fatalf("region id change did not reset scroll: top=%d pinned=%v", v.regionTop, v.regionPinned)
	}
}

func TestPresentationV3InvalidationRefreshesView(t *testing.T) {
	fetches := 0
	m := New(context.Background(), callerFunc(func(ctx context.Context, method string, params, out any) error {
		if strings.HasSuffix(method, "/view") {
			fetches++
			v := areaData()
			v.Status = fmt.Sprintf("fetch %d", fetches)
			return writeJSON(out, v)
		}
		return writeJSON(out, screenData())
	}), nil)
	m.openView(screenStatus())
	epoch := m.presentation.epoch
	m.Update(viewMsg{epoch: epoch, data: areaData(), version: 3})
	v := m.presentation
	// A subscription the socket client would have opened.
	m.viewSub = &ipc.ClientSubscription{Events: make(chan ipc.Notification, 4)}
	push := func(source string) tea.Msg {
		return viewEventMsg{ok: true, event: ipc.Notification{
			Event: events.Record{Type: viewEventTopic, Source: "app/" + source},
		}}
	}
	// An unrelated app's push does not refetch this view.
	if _, cmd := m.Update(push("com.other.app")); v.refreshing || v.refreshQueued {
		t.Fatal("foreign app invalidation triggered a refetch")
	} else if cmd == nil {
		t.Fatal("event consumer was not re-armed")
	}
	// A matching push starts one in-flight refetch at the negotiated version.
	_, cmd := m.Update(push("com.test.screen"))
	if cmd == nil || !v.refreshing {
		t.Fatal("matching invalidation did not refetch")
	}
	batch := cmd().(tea.BatchMsg)
	// A second push while the fetch is out queues exactly one follow-up.
	m.Update(push("com.test.screen"))
	if !v.refreshQueued {
		t.Fatal("concurrent invalidation did not queue a follow-up")
	}
	msg := batch[1]() // batch[0] is the re-armed event wait
	_, followUp := m.Update(msg)
	if v.refreshQueued || !v.refreshing || followUp == nil {
		t.Fatalf("queued refresh did not drain into one follow-up: queued=%v refreshing=%v", v.refreshQueued, v.refreshing)
	}
	m.Update(followUp())
	if v.refreshing || v.data.Status != "fetch 2" || fetches != 2 {
		t.Fatalf("refresh replies not applied in order: status=%q fetches=%d", v.data.Status, fetches)
	}
	// A stale refresh reply still releases the in-flight flag.
	v.refreshing = true
	m.Update(viewMsg{epoch: epoch, seq: v.seq + 9, refresh: true, data: areaData()})
	if v.refreshing {
		t.Fatal("dropped refresh wedged the in-flight flag")
	}
	// A dead stream clears the subscription so the next open retries it.
	m.Update(viewEventMsg{ok: false})
	if m.viewSub != nil {
		t.Fatal("closed subscription was not released")
	}
}

func TestPresentationV3SubscriptionLifecycle(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	// The in-process caller lacks Subscribe: openView degrades to polling.
	m.openView(screenStatus())
	if m.viewSub != nil {
		t.Fatal("subscription opened without a subscriber client")
	}
	// A negotiated subscription installs and arms the event wait.
	sub := &ipc.ClientSubscription{Events: make(chan ipc.Notification, 1)}
	if cmd := m.updatePresentation(viewSubscribedMsg{sub: sub}); cmd == nil || m.viewSub != sub {
		t.Fatal("subscription did not install")
	}
	m.updatePresentation(viewSubscribedMsg{err: errors.New("no socket")})
	if m.viewSub != nil {
		t.Fatal("failed subscribe left a stale subscription")
	}
}
