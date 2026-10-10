package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testView() View {
	return View{
		Title: "Test", State: ViewReady,
		Items:   []Item{{ID: "one", Label: "One"}},
		Fields:  []Field{{ID: "text", Label: "Text", Required: true}},
		Actions: []Action{{ID: "submit", Label: "Submit"}, {ID: "off", Label: "Off", Disabled: true}},
	}
}

func presentTest(t *testing.T, snapshot func(context.Context) (View, error), action func(context.Context, ActionRequest) (View, error)) map[string]Handler {
	t.Helper()
	routes := make(map[string]Handler)
	c := NewContext(Manifest{}, nil, nil, func(name string, h Handler) error {
		if routes[name] != nil {
			return errors.New("duplicate")
		}
		routes[name] = h
		return nil
	})
	if err := c.Present(snapshot, action); err != nil {
		t.Fatal(err)
	}
	return routes
}

func invokePresentation(t *testing.T, h Handler, params any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return h(context.Background(), raw)
}

func TestPresentationValidation(t *testing.T) {
	good := testView()
	good.Version, good.Instance = PresentationVersion, "launch"
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*View){
		func(v *View) { v.Version = PresentationVersion + 1 },
		func(v *View) { v.Version = MinPresentationVersion - 1 },
		func(v *View) { v.Instance = "" },
		func(v *View) { v.Title = " " },
		func(v *View) { v.State = "future" },
		func(v *View) { v.State = ViewError },
		func(v *View) { v.Items = append(v.Items, v.Items[0]) },
		func(v *View) { v.Fields = append(v.Fields, v.Fields[0]) },
		func(v *View) { v.Actions = append(v.Actions, v.Actions[0]) },
		func(v *View) { v.Items = make([]Item, 101) },
		func(v *View) { v.Fields = []Field{{ID: "text", Label: "Text", Value: strings.Repeat("x", 4097)}} },
		func(v *View) { v.Actions = []Action{{ID: "bad/id", Label: "Bad"}} },
		func(v *View) {
			v.Version = 1
			v.Blocks = []Block{{ID: "art", Text: "x"}}
		},
		func(v *View) {
			v.Version = 1
			v.Meters = []Meter{{ID: "fuel", Label: "Fuel"}}
		},
		func(v *View) {
			v.Version = 1
			v.Grid = &Grid{Columns: 1, Cells: []Cell{{ID: "a", Label: "A"}}}
		},
		func(v *View) { v.Blocks = make([]Block, 5) },
		func(v *View) { v.Blocks = []Block{{ID: "bad id", Text: "x"}} },
		func(v *View) {
			v.Blocks = []Block{{ID: "art", Label: strings.Repeat("x", 257), Text: "x"}}
		},
		func(v *View) {
			v.Blocks = []Block{{ID: "art", Text: strings.Repeat("x", 16385)}}
		},
		func(v *View) {
			v.Blocks = []Block{{ID: "art", Text: strings.Repeat("x\n", 33)}}
		},
		func(v *View) { v.Meters = make([]Meter, 9) },
		func(v *View) { v.Meters = []Meter{{ID: "fuel", Label: " "}} },
		func(v *View) { v.Meters = []Meter{{ID: "fuel", Label: "Fuel", Value: math.NaN()}} },
		func(v *View) { v.Meters = []Meter{{ID: "fuel", Label: "Fuel", Value: math.Inf(1)}} },
		func(v *View) { v.Grid = &Grid{Columns: 0} },
		func(v *View) { v.Grid = &Grid{Columns: 17} },
		func(v *View) { v.Grid = &Grid{Label: "Pad", Columns: 1} },
		func(v *View) {
			v.Grid = &Grid{Columns: 1, Cells: []Cell{{ID: "a", Label: "A"}, {ID: "a", Label: "B"}}}
		},
		func(v *View) {
			v.Grid = &Grid{Columns: 1, Cells: []Cell{{ID: "a", Label: "A", Action: "bad/action"}}}
		},
		func(v *View) { v.Grid = &Grid{Label: strings.Repeat("x", 257), Columns: 1} },
		func(v *View) {
			v.Version = 2
			v.Region = &Region{ID: "log", Text: "x"}
		},
		func(v *View) {
			v.Version = 2
			v.Fields = []Field{{ID: "text", Label: "Text", Multiline: true}}
		},
		func(v *View) { v.Region = &Region{ID: "bad id", Text: "x"} },
		func(v *View) { v.Region = &Region{ID: "log", Label: strings.Repeat("x", 257)} },
		func(v *View) { v.Region = &Region{ID: "log", Text: strings.Repeat("x", 65537)} },
		func(v *View) { v.Region = &Region{ID: "log", Text: strings.Repeat("x\n", 2049)} },
		func(v *View) {
			v.Fields = []Field{{ID: "text", Label: "Text", Multiline: true, Value: strings.Repeat("x", 32769)}}
		},
	} {
		v := good
		change(&v)
		if err := v.Validate(); err == nil {
			t.Errorf("accepted invalid view: %+v", v)
		}
	}
	for _, state := range []ViewState{ViewLoading, ViewError} {
		v := good
		v.State, v.Error = state, "recoverable failure"
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	// One trailing newline terminates the last line rather than counting as
	// another: exactly maxBlockLines lines ending in a newline still pass.
	v := good
	v.Blocks = []Block{{ID: "art", Text: strings.Repeat("x\n", maxBlockLines)}}
	if err := v.Validate(); err != nil {
		t.Fatalf("trailing newline counted as an extra line: %v", err)
	}
}

func TestPresentationTypedRoutesAndInputs(t *testing.T) {
	var calls atomic.Int32
	routes := presentTest(t, func(context.Context) (View, error) { return testView(), nil },
		func(ctx context.Context, p ActionRequest) (View, error) {
			calls.Add(1)
			v := testView()
			v.Status = p.Values["text"] + ":" + p.ItemID
			return v, nil
		})
	out, err := invokePresentation(t, routes["view"], ViewRequest{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	v := out.(View)
	p := ActionRequest{Version: 1, Instance: v.Instance, RequestID: "request", Action: "submit", ItemID: "one", Values: map[string]string{"text": "hello"}}
	out, err = invokePresentation(t, routes["action"], p)
	if err != nil || out.(View).Status != "hello:one" {
		t.Fatalf("action: %v, %v", out, err)
	}
	for _, change := range []func(*ActionRequest){
		func(p *ActionRequest) { p.Version = PresentationVersion + 1 },
		func(p *ActionRequest) { p.Version = MinPresentationVersion - 1 },
		func(p *ActionRequest) { p.Instance = "previous-launch" },
		func(p *ActionRequest) { p.RequestID = "" },
		func(p *ActionRequest) { p.Action = "missing" },
		func(p *ActionRequest) { p.Action = "off" },
		func(p *ActionRequest) { p.ItemID = "missing" },
		func(p *ActionRequest) { p.Values = nil },
		func(p *ActionRequest) { p.Values = map[string]string{"text": "yes", "extra": "no"} },
		func(p *ActionRequest) { p.Values = map[string]string{"text": strings.Repeat("x", 4097)} },
	} {
		bad := p
		change(&bad)
		if _, err := invokePresentation(t, routes["action"], bad); err == nil {
			t.Errorf("accepted bad action: %+v", bad)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid input reached callback")
	}
	for _, raw := range []string{"", "null", `{"version":0}`, `{"version":4}`, `{"version":1,"unknown":true}`, `{"version":1} {}`, `[]`} {
		if _, err := routes["view"](context.Background(), json.RawMessage(raw)); err == nil {
			t.Errorf("accepted params %q", raw)
		}
	}
	if _, err := invokePresentation(t, routes["cancel"], CancelRequest{Version: 1, Instance: "stale", RequestID: "request"}); err == nil {
		t.Fatal("stale instance cancellation accepted")
	}
}

func TestPresentationCancellation(t *testing.T) {
	started := make(chan struct{})
	var calls atomic.Int32
	routes := presentTest(t, func(context.Context) (View, error) { return testView(), nil },
		func(ctx context.Context, p ActionRequest) (View, error) {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			return View{}, ctx.Err()
		})
	out, err := invokePresentation(t, routes["view"], ViewRequest{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	p := ActionRequest{Version: 1, Instance: out.(View).Instance, RequestID: "active", Action: "submit", Values: map[string]string{"text": "hello"}}
	raw, _ := json.Marshal(p)
	done := make(chan error, 1)
	go func() { _, err := routes["action"](context.Background(), raw); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("action did not start")
	}
	if _, err := invokePresentation(t, routes["action"], p); err == nil {
		t.Fatal("duplicate active request accepted")
	}
	if _, err := invokePresentation(t, routes["cancel"], CancelRequest{Version: 1, Instance: p.Instance, RequestID: p.RequestID}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("action was not canceled")
	}
	// Cancel may arrive first on a multiplexed socket.
	p.RequestID = "early"
	if _, err := invokePresentation(t, routes["cancel"], CancelRequest{Version: 1, Instance: p.Instance, RequestID: p.RequestID}); err != nil {
		t.Fatal(err)
	}
	if _, err := invokePresentation(t, routes["action"], p); !errors.Is(err, context.Canceled) || calls.Load() != 1 {
		t.Fatal("early cancellation reached action callback")
	}
	// Direct callback callers also preserve parent cancellation/deadlines.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.RequestID = "parent"
	raw, _ = json.Marshal(p)
	if _, err := routes["action"](ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancellation lost: %v", err)
	}
}

func TestPresentationCallbackErrorsAndLoading(t *testing.T) {
	for _, state := range []ViewState{ViewReady, ViewLoading, ViewError} {
		t.Run(string(state), func(t *testing.T) {
			routes := presentTest(t, func(context.Context) (View, error) {
				v := testView()
				v.State, v.Error = state, "error banner"
				return v, nil
			}, func(context.Context, ActionRequest) (View, error) { return View{}, errors.New("operation failed") })
			out, err := invokePresentation(t, routes["view"], ViewRequest{Version: 1})
			if err != nil {
				t.Fatal(err)
			}
			p := ActionRequest{Version: 1, Instance: out.(View).Instance, RequestID: "request", Action: "submit", Values: map[string]string{"text": "x"}}
			if _, err := invokePresentation(t, routes["action"], p); err == nil {
				t.Fatal("loading action or callback failure became success")
			}
		})
	}
	c := NewContext(Manifest{}, nil, nil, nil)
	if err := c.Present(nil, nil); err == nil {
		t.Fatal("nil callbacks accepted")
	}
}

func gridView() View {
	return View{
		Title: "Pad", State: ViewReady,
		Items:  []Item{{ID: "one", Label: "One"}},
		Fields: []Field{{ID: "text", Label: "Text", Required: true}},
		Actions: []Action{
			{ID: "submit", Label: "Submit"},
			{ID: "off", Label: "Off", Disabled: true},
		},
		Blocks: []Block{{ID: "art", Label: "Mascot", Text: "(\\_/)\n(o.o)\n> ^ <"}},
		Meters: []Meter{{ID: "charge", Label: "Charge", Value: 1.5}},
		Grid: &Grid{Label: "Pad", Columns: 2, Cells: []Cell{
			{ID: "nw", Label: "NW", Action: "move"},
			{ID: "ne", Label: "NE", Action: "move"},
			{ID: "sw", Label: "SW"},
			{ID: "se", Label: "SE", Disabled: true},
			{ID: "ko", Label: "KO", Action: "off"},
		}},
	}
}

func TestPresentationVersionNegotiation(t *testing.T) {
	var seen []int
	routes := presentTest(t, func(ctx context.Context) (View, error) {
		seen = append(seen, PresentationRequestVersion(ctx))
		return testView(), nil
	}, func(ctx context.Context, p ActionRequest) (View, error) { return testView(), nil })
	for _, version := range []int{1, 2, 3} {
		out, err := invokePresentation(t, routes["view"], ViewRequest{Version: version})
		if err != nil {
			t.Fatal(err)
		}
		v := out.(View)
		if v.Version != version || len(v.Blocks) != 0 || v.Grid != nil {
			t.Fatalf("negotiated view: %+v", v)
		}
		if err := v.Validate(); err != nil {
			t.Fatalf("negotiated version %d view did not validate: %v", version, err)
		}
	}
	if len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("callbacks did not observe negotiated versions: %v", seen)
	}
	if PresentationRequestVersion(context.Background()) != 0 {
		t.Fatal("request version leaked outside a callback")
	}
}

// areaView is a version-3 snapshot: a multiline field plus a scroll region.
func areaView() View {
	return View{
		Title: "Doc", State: ViewReady,
		Fields:  []Field{{ID: "body", Label: "Body", Multiline: true}},
		Actions: []Action{{ID: "save", Label: "Save"}},
		Region:  &Region{ID: "preview", Label: "Preview", Text: "line 1\nline 2\nline 3\n"},
	}
}

func TestPresentationV3ElementsAndAreaInput(t *testing.T) {
	var last ActionRequest
	routes := presentTest(t, func(context.Context) (View, error) { return areaView(), nil },
		func(ctx context.Context, p ActionRequest) (View, error) {
			last = p
			return areaView(), nil
		})
	out, err := invokePresentation(t, routes["view"], ViewRequest{Version: 3})
	if err != nil {
		t.Fatal(err)
	}
	v := out.(View)
	if v.Region == nil || len(v.Fields) != 1 || !v.Fields[0].Multiline {
		t.Fatalf("version 3 snapshot dropped elements: %+v", v)
	}
	instance := v.Instance
	// A version-2 request must fail rather than silently drop the new elements.
	for _, version := range []int{1, 2} {
		if _, err := invokePresentation(t, routes["view"], ViewRequest{Version: version}); err == nil {
			t.Fatalf("version-%d request accepted a snapshot carrying version-3 elements", version)
		}
	}
	// Multiline values keep newlines verbatim and use the larger budget,
	// which neither shrinks to nor exceeds the contract's allowance.
	for _, tc := range []struct {
		request string
		size    int
		ok      bool
	}{
		{"r1", 4096, true},
		{"r2", 32768, true},
		{"r3", 32769, false},
	} {
		p := ActionRequest{Version: 3, Instance: instance, RequestID: tc.request, Action: "save",
			Values: map[string]string{"body": strings.Repeat("x", tc.size)}}
		_, err := invokePresentation(t, routes["action"], p)
		if ok := err == nil; ok != tc.ok {
			t.Fatalf("%d-byte multiline value: err = %v", tc.size, err)
		}
	}
	p := ActionRequest{Version: 3, Instance: instance, RequestID: "r4", Action: "save",
		Values: map[string]string{"body": "a\nb\n"}}
	if _, err := invokePresentation(t, routes["action"], p); err != nil || last.Values["body"] != "a\nb\n" {
		t.Fatalf("multiline value lost its newlines: %v, %q", err, last.Values["body"])
	}
}

func TestPresentationV2ElementsAndCellActions(t *testing.T) {
	var last ActionRequest
	routes := presentTest(t, func(context.Context) (View, error) { return gridView(), nil },
		func(ctx context.Context, p ActionRequest) (View, error) {
			last = p
			return gridView(), nil
		})
	out, err := invokePresentation(t, routes["view"], ViewRequest{Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	v := out.(View)
	if v.Version != 2 || len(v.Blocks) != 1 || len(v.Meters) != 1 || v.Grid == nil || len(v.Grid.Cells) != 5 {
		t.Fatalf("version 2 snapshot dropped elements: %+v", v)
	}
	instance := v.Instance
	// A version-1 request must fail rather than silently drop the new elements.
	if _, err := invokePresentation(t, routes["view"], ViewRequest{Version: 1}); err == nil {
		t.Fatal("version-1 request accepted a snapshot carrying version-2 elements")
	}
	base := ActionRequest{Version: 2, Instance: instance, RequestID: "r1", Values: map[string]string{"text": "x"}}
	for _, change := range []struct {
		name  string
		apply func(*ActionRequest)
		ok    bool
	}{
		{"cell action with its cell_id", func(p *ActionRequest) { p.Action, p.CellID = "move", "nw" }, true},
		{"cell action without cell_id", func(p *ActionRequest) { p.Action = "move" }, false},
		{"cell action with wrong cell", func(p *ActionRequest) { p.Action, p.CellID = "move", "sw" }, false},
		{"disabled cell", func(p *ActionRequest) { p.Action, p.CellID = "move", "se" }, false},
		{"unknown cell", func(p *ActionRequest) { p.CellID = "cell-404" }, false},
		{"empty action via datum cell", func(p *ActionRequest) { p.CellID = "sw" }, false},
		{"empty action via action cell", func(p *ActionRequest) { p.CellID = "nw" }, false},
		{"cell action vetoed by disabled top-level", func(p *ActionRequest) {
			p.Action, p.CellID = "off", "ko"
		}, false},
		{"list action with datum cell", func(p *ActionRequest) { p.Action, p.CellID = "submit", "sw" }, true},
		{"list action with item and cell", func(p *ActionRequest) {
			p.Action, p.ItemID, p.CellID = "submit", "one", "nw"
		}, true},
		{"disabled list action with cell", func(p *ActionRequest) { p.Action, p.CellID = "off", "nw" }, false},
	} {
		p := base
		change.apply(&p)
		_, err := invokePresentation(t, routes["action"], p)
		if ok := err == nil; ok != change.ok {
			t.Fatalf("%s: err = %v", change.name, err)
		}
	}
	if last.Action != "submit" || last.CellID != "nw" || last.ItemID != "one" {
		t.Fatalf("action routing lost cell/item selection: %+v", last)
	}
	// A version-1 action request cannot validate against a version-2 snapshot.
	p := base
	p.Version = 1
	if _, err := invokePresentation(t, routes["action"], p); err == nil {
		t.Fatal("version-1 action accepted against version-2 snapshot")
	}
}
