// Package delve is Gostalgia's roguelike, "Delve — Beneath the Brass
// Elephant": eight generated levels under the tavern from Adventure,
// monsters, loot, hunger, XP, permadeath, and a persisted score ledger.
//
// The game core lives in apps/delve/engine — a pure, stdlib-only,
// deterministic package. This file owns everything IPC: routes, the
// presentation snapshot, action dispatch, fs/save persistence to the
// app-private partition, and sound blips for event kinds.
//
// CONTRACT SKELETON: the app-shell task owns this file and view.go and
// may restructure freely. The seams that keep it testable without the
// engine: newGame/reviveGame are package vars, and gameEngine is a
// narrow interface over engine.Game, so tests substitute a fake.
package delve

import (
	"context"
	_ "embed"
	"encoding/json"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Delve.
const ID = "com.gostalgia.delve"

// statePath is the persisted run inside the app-private partition; the
// score ledger lives beside it. fs/save stages and renames atomically.
const (
	statePath  = "/apps/data/com.gostalgia.delve/save.json"
	scoresPath = "/apps/data/com.gostalgia.delve/scores.json"
)

// gameEngine is the slice of engine.Game the shell drives. Tests
// substitute a fake so app logic runs without the full engine.
type gameEngine interface {
	Command(engine.Cmd) []engine.Event
	MapView() []string
	LogTail(int) []string
	Stats() engine.Stats
	Flags() engine.Flags
	Inventory() []engine.InvEntry
	Outcome() engine.Outcome
	FinalScore() engine.Score
	Marshal() ([]byte, error)
}

// newGame starts a run; reviveGame restores one from Marshal bytes.
// Package vars so tests can inject fakes.
var (
	newGame    = func(cfg engine.Config) (gameEngine, error) { return engine.New(cfg) }
	reviveGame = func(b []byte) (gameEngine, error) { return engine.Unmarshal(b) }
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid builtin manifest is a programming error
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte { return append([]byte(nil), manifestJSON...) }

// Delve is the running in-process instance. SKELETON: the app-shell
// task fills in state, persistence, view composition, and action
// dispatch.
type Delve struct {
	app *sdk.Context
	g   gameEngine
}

// Factory constructs a fresh, uninitialized Delve instance.
func Factory() (sdk.Instance, error) { return &Delve{}, nil }

// Init registers routes and the presentation callbacks.
func (d *Delve) Init(app *sdk.Context) error {
	d.app = app
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", d.stateRoute},
		{"command", d.commandRoute},
		{"restart", d.restartRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(d.view, d.act)
}

// Run blocks until the instance is canceled. Delve is turn-based: the
// world only moves when the player does.
func (d *Delve) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save. A failed flush is logged, never fatal.
func (d *Delve) Stop(ctx context.Context) error { return nil }

func (d *Delve) stateRoute(ctx context.Context, p json.RawMessage) (any, error) {
	outcome := engine.OutcomeOngoing
	if d.g != nil {
		outcome = d.g.Outcome()
	}
	return map[string]any{"outcome": int(outcome)}, nil
}

func (d *Delve) commandRoute(ctx context.Context, p json.RawMessage) (any, error) {
	return map[string]any{"ok": true}, nil
}

func (d *Delve) restartRoute(ctx context.Context, p json.RawMessage) (any, error) {
	return map[string]any{"ok": true}, nil
}

func (d *Delve) view(ctx context.Context) (sdk.View, error) {
	return sdk.View{Title: "Delve", State: sdk.ViewReady}, nil
}

func (d *Delve) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	return d.view(ctx)
}
