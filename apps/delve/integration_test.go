package delve

// integration_test.go drives the app with the REAL engine wired in —
// no fakes — so the seam between presentation, persistence, and game
// logic is exercised end to end.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// wireReal points the package seams at the true engine for this test.
// The seed is pinned: the app draws a fresh rand.Uint64 per run, and
// ~3% of seeds kill a scripted mover inside 30 turns — a random seed
// here is a CI flake, not coverage.
func wireReal() {
	newGame = func(cfg engine.Config) (gameEngine, error) {
		cfg.Seed = 1
		return engine.New(cfg)
	}
	reviveGame = func(b []byte) (gameEngine, error) { return engine.Unmarshal(b) }
}

// TestLiveRunThroughApp starts a real run, plays turns through the
// action surface, and checks the real data lands in the snapshot: map
// dimensions, meters, log region, and a save blob that the engine can
// re-read.
func TestLiveRunThroughApp(t *testing.T) {
	h := newHarness(t)
	wireReal()

	v := h.startRun(t, "Rex")
	if v.State != sdk.ViewReady {
		t.Fatalf("state = %q, err = %q", v.State, v.Error)
	}
	b := findBlock(v, "map")
	if b == nil {
		t.Fatal("no map block")
	}
	lines := strings.Split(b.Text, "\n")
	if len(lines) != engine.MapHeight {
		t.Fatalf("map has %d lines, want %d", len(lines), engine.MapHeight)
	}
	for i, ln := range lines {
		if n := len([]rune(ln)); n != engine.MapWidth {
			t.Fatalf("map line %d is %d runes, want %d", i, n, engine.MapWidth)
		}
	}
	if !strings.Contains(b.Text, string(engine.GlyphPlayer)) {
		t.Error("map does not draw the player glyph")
	}
	if findMeter(v, "hp") == nil || findMeter(v, "satiety") == nil {
		t.Error("missing vitals meters")
	}
	if v.Region == nil || !strings.Contains(v.Region.Text, "Brass Elephant") {
		t.Errorf("log region missing intro, got %q", v.Region)
	}
	if v.Grid == nil || len(v.Grid.Cells) != 9 {
		t.Fatal("movement pad missing or malformed")
	}

	// Play thirty real turns and confirm the world moves. The pad
	// disappears when the run ends, so stop politely if a seed change
	// ever turns the scripted mover into a corpse.
	moves := []string{"e", "s", "e", "w", "n", "se", "sw", "ne"}
	for i := 0; i < 30; i++ {
		if h.app.g.Outcome() != engine.OutcomeOngoing {
			break
		}
		v = h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: moves[i%len(moves)]})
		if v.State == sdk.ViewError {
			t.Fatalf("turn %d: view error %q", i, v.Error)
		}
	}

	// The save landed and the engine can read its own bytes back.
	raw := h.savedFile(t, statePath)
	var doc struct {
		Version int    `json:"version"`
		Run     string `json:"run"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("save doc: %v", err)
	}
	run, err := base64.StdEncoding.DecodeString(doc.Run)
	if err != nil {
		t.Fatalf("save run blob: %v", err)
	}
	g2, err := engine.Unmarshal(run)
	if err != nil {
		t.Fatalf("engine cannot read its own save: %v", err)
	}
	if g2.Outcome() != engine.OutcomeOngoing {
		t.Fatalf("revived outcome = %d", g2.Outcome())
	}
	if g2.Player.Name != "Rex" {
		t.Errorf("revived name = %q", g2.Player.Name)
	}

	// A real combat should have produced sound blips at some point, or
	// at minimum the sound route was exercised without breaking turns.
	// (Monster contact is seed-dependent; the assertions above are the
	// load-bearing ones.)
}

// TestLiveReviveRoundTrip persists mid-run then relaunches a fresh app
// instance against the saved bytes: continue must restore the live
// engine state, not a fake.
func TestLiveReviveRoundTrip(t *testing.T) {
	h := newHarness(t)
	wireReal()

	v := h.startRun(t, "Zed")
	if v.State != sdk.ViewReady {
		t.Fatalf("start: %q", v.Error)
	}
	for i := 0; i < 10; i++ {
		h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "e"})
	}
	raw := h.savedFile(t, statePath)

	// Fresh harness = fresh app instance, same filesystem.
	h2 := newHarness(t)
	wireReal()
	h2.files[statePath] = raw

	v2 := h2.view(t, 3)
	// A live save offers continue-or-new; continue resumes the run.
	if findAction(v2, "continue") == nil {
		t.Fatal("revived save did not offer continue")
	}
	v3 := h2.act(t, 3, sdk.ActionRequest{Action: "continue"})
	b := findBlock(v3, "map")
	if b == nil {
		t.Fatal("continued run shows no map")
	}
	if !strings.Contains(b.Text, string(engine.GlyphPlayer)) {
		t.Error("continued map lacks the player")
	}
}
