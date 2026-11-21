package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"gostalgia/sdk"
)

type externalApp struct {
	state string
}

func (a *externalApp) Init(ctx *sdk.Context) error {
	a.state = "initialized"
	_ = ctx.Handle("greet", func(c context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Name string `json:"name"`
		}
		_ = sdk.DecodeParams(raw, &p)
		return map[string]string{
			"greeting": "hello " + p.Name,
			"state":    a.state,
		}, nil
	})
	_ = ctx.Handle("crash", func(c context.Context, raw json.RawMessage) (any, error) {
		os.Exit(42)
		return nil, nil
	})
	_ = ctx.Handle("hang", func(c context.Context, raw json.RawMessage) (any, error) {
		select {}
	})
	// presentation routes
	viewFn := func(c context.Context) (sdk.View, error) {
		v := sdk.View{
			State:  sdk.ViewReady,
			Title:  "External Test App",
			Status: a.state,
			Fields: []sdk.Field{
				{ID: "state", Label: "State"},
			},
			Actions: []sdk.Action{
				{ID: "set_state", Label: "Set State"},
			},
		}
		if sdk.PresentationRequestVersion(c) >= 2 {
			v.Blocks = []sdk.Block{{
				ID: "mascot", Label: "Mascot",
				Text: " (\\_/)\n (o.o)\n / >\\",
			}}
			v.Meters = []sdk.Meter{
				{ID: "charge", Label: "Charge", Value: 0.42},
				{ID: "hunger", Label: "Hunger", Value: 1.4},
			}
			v.Grid = &sdk.Grid{Label: "Pad", Columns: 3, Cells: []sdk.Cell{
				{ID: "feed", Label: "Feed", Action: "press"},
				{ID: "pet", Label: "Pet", Action: "press"},
				{ID: "nap", Label: "Nap", Disabled: true},
				{ID: "stats", Label: "Stats"},
				{ID: "play", Label: "Play", Action: "press"},
				{ID: "vet", Label: "Vet", Action: "press"},
			}}
			v.Actions = append(v.Actions, sdk.Action{ID: "reset", Label: "Reset"})
		}
		return v, nil
	}
	actionFn := func(c context.Context, r sdk.ActionRequest) (sdk.View, error) {
		switch r.Action {
		case "set_state":
			a.state = r.Values["state"]
		case "press":
			a.state = "pressed " + r.CellID
		case "reset":
			a.state = "initialized"
		}
		return viewFn(c)
	}
	return ctx.Present(viewFn, actionFn)
}

func (a *externalApp) Run(ctx context.Context) error {
	_, _ = fmt.Fprintln(os.Stdout, "external app started")
	<-ctx.Done()
	_, _ = fmt.Fprintln(os.Stdout, "external app shutting down")
	return ctx.Err()
}

func (a *externalApp) Stop(ctx context.Context) error {
	_, _ = fmt.Fprintln(os.Stdout, "external app stopped")
	return nil
}

func main() {
	if err := sdk.Serve(&externalApp{}); err != nil {
		fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
		os.Exit(1)
	}
}
