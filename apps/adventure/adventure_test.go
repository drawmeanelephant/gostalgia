package adventure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gostalgia/sdk"
)

// harness models the app-private VFS partition. deny simulates a missing
// fs grant. There is no clock: the game moves only when the player does.
type harness struct {
	deny    bool  // fs/read+fs/save denied: simulates missing grants
	readErr error // if set, the next fs/read fails once with this error
	seq     atomic.Int64

	fmu   sync.Mutex // guards files; concurrent handlers share the map
	files map[string][]byte

	app      *Adventure
	handlers map[string]sdk.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		files:    map[string][]byte{},
		handlers: map[string]sdk.Handler{},
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	av := inst.(*Adventure)
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := av.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = av
	return h
}

func (h *harness) call(_ context.Context, method string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		Path string `json:"path"`
		Data string `json:"data_base64"`
	}
	_ = json.Unmarshal(raw, &p)
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	h.fmu.Lock()
	defer h.fmu.Unlock()
	if h.deny {
		return &sdk.Error{Code: sdk.CodePermissionDenied, Message: "permission denied: caller not authorized"}
	}
	switch method {
	case "fs/read":
		if h.readErr != nil {
			err := h.readErr
			h.readErr = nil
			return err
		}
		data, ok := h.files[p.Path]
		if !ok {
			return &sdk.Error{Code: sdk.CodeNotFound, Message: fmt.Sprintf("not_found: %s", p.Path)}
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	}
	return fmt.Errorf("unknown method %s", method)
}

func (h *harness) savedFile(t *testing.T) []byte {
	t.Helper()
	h.fmu.Lock()
	defer h.fmu.Unlock()
	data, ok := h.files[statePath]
	if !ok {
		t.Fatalf("no saved state at %s", statePath)
	}
	return append([]byte(nil), data...)
}

func (h *harness) view(t *testing.T, version int) sdk.View {
	t.Helper()
	res, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		t.Fatalf("view v%d: %v", version, err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view v%d does not validate: %v", version, err)
	}
	return v
}

func (h *harness) act(t *testing.T, version int, action string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, action, values)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) tryAct(version int, action string, values map[string]string) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req := sdk.ActionRequest{
		Version:   version,
		Instance:  cur.(sdk.View).Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)),
		Action:    action,
		Values:    values,
	}
	raw, _ := json.Marshal(req)
	res, err := h.handlers["action"](context.Background(), raw)
	if err != nil {
		return sdk.View{}, err
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		return sdk.View{}, fmt.Errorf("action view does not validate: %w", err)
	}
	return v, nil
}

// cmd is shorthand for typing into the command field and pressing Do it.
func (h *harness) cmd(t *testing.T, input string) sdk.View {
	t.Helper()
	return h.act(t, 1, "command", map[string]string{"cmd": input})
}

func (h *harness) route(t *testing.T, name string, params, out any) {
	t.Helper()
	res, err := h.tryRoute(name, params)
	if err != nil {
		t.Fatalf("route %s: %v", name, err)
	}
	if out != nil {
		b, _ := json.Marshal(res)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("route %s decode: %v", name, err)
		}
	}
}

func (h *harness) tryRoute(name string, params any) (any, error) {
	hd, ok := h.handlers[name]
	if !ok {
		return nil, fmt.Errorf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	return hd(context.Background(), raw)
}

func findAction(v sdk.View, id string) *sdk.Action {
	for i := range v.Actions {
		if v.Actions[i].ID == id {
			return &v.Actions[i]
		}
	}
	return nil
}

func labels(v sdk.View) []string {
	out := make([]string, 0, len(v.Items))
	for _, it := range v.Items {
		out = append(out, it.Label)
	}
	return out
}

func transcriptHas(v sdk.View, sub string) bool {
	for _, it := range v.Items {
		if strings.Contains(it.Label, sub) {
			return true
		}
	}
	return false
}

// --- manifest ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "adventure" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "fs.read": true, "fs.write": true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for _, c := range m.Permissions {
		if !want[c] {
			t.Errorf("unexpected permission %q", c)
		}
	}
	if !json.Valid(ManifestJSON()) {
		t.Error("ManifestJSON is not valid JSON")
	}
}

// --- presentation ---

func TestFreshViewIsTheOpening(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if v.State != sdk.ViewReady {
		t.Fatalf("fresh view state = %q", v.State)
	}
	if !transcriptHas(v, "THE BRASS ELEPHANT") || !transcriptHas(v, "Dusty Foyer") {
		t.Fatalf("fresh transcript = %v", labels(v))
	}
	for _, id := range []string{"command", "look", "inventory", "restart", "help"} {
		if a := findAction(v, id); a == nil || a.Disabled {
			t.Errorf("fresh action %s should be present and enabled", id)
		}
	}
	if len(v.Fields) != 1 || v.Fields[0].ID != "cmd" {
		t.Fatalf("fields = %+v", v.Fields)
	}
	// Version-2 requests get the same v1 surface.
	if v2 := h.view(t, 2); len(v2.Blocks) != 0 || len(v2.Meters) != 0 || v2.Grid != nil {
		t.Error("v2 view should carry no version-2 elements")
	}
}

func TestCommandsEchoAndReplyInTranscript(t *testing.T) {
	h := newHarness(t)
	v := h.cmd(t, "take lamp")
	if !transcriptHas(v, "> take lamp") {
		t.Fatalf("command echo missing: %v", labels(v))
	}
	if !transcriptHas(v, "You take the brass lamp") {
		t.Fatalf("reply missing: %v", labels(v))
	}
	if !strings.Contains(v.Status, "turn") {
		t.Errorf("status = %q, want room + turn", v.Status)
	}
	// Button actions echo identically, so commands and actions stay in sync.
	v = h.act(t, 1, "inventory", nil)
	if !transcriptHas(v, "> inventory") || !transcriptHas(v, "carrying: brass lamp") {
		t.Fatalf("action transcript = %v", labels(v))
	}
	v = h.act(t, 1, "look", nil)
	if !transcriptHas(v, "== Dusty Foyer ==") {
		t.Fatalf("look action transcript = %v", labels(v))
	}
}

func TestMalformedCommandsAreInFiction(t *testing.T) {
	h := newHarness(t)
	for input, want := range map[string]string{
		"":                  "silence isn't a command",
		"   ":               "silence isn't a command",
		"frotz the grue":    "can't",
		"xyzzy":             "can't",
		"take telescope":    "don't see",
		"go sideways":       "isn't a direction",
		"take the map now ": "verb and a thing",
	} {
		v := h.cmd(t, input)
		if !transcriptHas(v, want) {
			t.Errorf("input %q transcript = %v, want %q", input, labels(v), want)
		}
		if v.State != sdk.ViewReady || v.Error != "" {
			t.Errorf("input %q should never produce an error view: %q", input, v.Error)
		}
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(1, "launch_missiles", nil); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

func TestGameEndsStayHonest(t *testing.T) {
	h := newHarness(t)
	// Descend in the dark: fatal.
	v := h.cmd(t, "go down")
	if !transcriptHas(v, "expedition ends here") {
		t.Fatalf("death transcript = %v", labels(v))
	}
	if v.Title != "Adventure — you died" {
		t.Errorf("dead title = %q", v.Title)
	}
	// look/inventory buttons are disabled post-end; help and restart live.
	if a := findAction(v, "look"); a != nil {
		t.Error("look action should not be offered after death")
	}
	if a := findAction(v, "restart"); a == nil || a.Disabled {
		t.Error("restart should be offered after death")
	}
	// The parser answers only restart/help now.
	v = h.cmd(t, "look")
	if !transcriptHas(v, "expedition is over") {
		t.Errorf("post-death command transcript = %v", labels(v))
	}
	// Restart begins anew.
	v = h.act(t, 1, "restart", nil)
	if !transcriptHas(v, "begins again") || transcriptHas(v, "expedition ends here") {
		t.Fatalf("restart transcript = %v", labels(v))
	}
	if !strings.Contains(v.Title, "Dusty Foyer") {
		t.Errorf("post-restart title = %q", v.Title)
	}
}

func TestWalkthroughThroughTheContract(t *testing.T) {
	h := newHarness(t)
	steps := []string{
		"take lamp", "use lamp", "go north", "take note", "go south",
		"go down", "take key", "go up", "go east", "use key",
		"go north", "take elephant", "go south", "go west", "go west",
	}
	var v sdk.View
	for _, step := range steps {
		v = h.cmd(t, step)
	}
	if v.Title != "Adventure — escaped!" {
		t.Fatalf("final title = %q, want the victory state", v.Title)
	}
	if !transcriptHas(v, "elephant is yours again") {
		t.Fatalf("victory transcript = %v", labels(v))
	}
}

// --- transcript bounds ---

func TestTranscriptDropsOldestWithinBound(t *testing.T) {
	h := newHarness(t)
	// Each bogus command adds two lines (echo + reply): 80 commands overflow
	// the view bound comfortably.
	for i := 0; i < 80; i++ {
		h.cmd(t, "frotz")
	}
	v := h.view(t, 1)
	if len(v.Items) != transcriptView+1 {
		t.Fatalf("items = %d, want %d transcript lines + notice", len(v.Items), transcriptView)
	}
	if v.Items[0].ID != "earlier" || !strings.Contains(v.Items[0].Label, "earlier lines") {
		t.Fatalf("first item should be the drop notice: %+v", v.Items[0])
	}
	last := v.Items[len(v.Items)-1]
	if !strings.Contains(last.Label, "can't") {
		t.Errorf("newest line should be at the bottom: %q", last.Label)
	}
	// The stored transcript keeps a longer tail, bounded by transcriptKeep.
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil {
		t.Fatalf("save parses: %v", err)
	}
	if len(st.Transcript) > transcriptKeep {
		t.Errorf("saved transcript = %d lines, bound is %d", len(st.Transcript), transcriptKeep)
	}
	if len(st.Transcript) <= transcriptView {
		t.Errorf("saved transcript = %d lines, want more than the view keeps", len(st.Transcript))
	}
}

// --- persistence ---

// seedSave writes a mid-game save directly, as a previous launch would.
func (h *harness) seedSave(t *testing.T, st diskState) {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	h.fmu.Lock()
	h.files[statePath] = data
	h.fmu.Unlock()
}

func TestRelaunchOffersContinueOrNew(t *testing.T) {
	h := newHarness(t)
	h.seedSave(t, diskState{
		Version:   stateVersion,
		Room:      "cellar",
		Inventory: []string{"lamp", "key"},
		Flags:     []string{"lamp_lit"},
		Turns:     6,
		Transcript: []string{
			"— THE BRASS ELEPHANT —", "> go down", "== Flooded Cellar ==",
		},
	})
	v := h.view(t, 1)
	if !strings.Contains(v.Status, "earlier expedition") {
		t.Fatalf("pending status = %q", v.Status)
	}
	for _, id := range []string{"continue", "new_game"} {
		if a := findAction(v, id); a == nil || a.Disabled {
			t.Errorf("pending action %s should be offered", id)
		}
	}
	if a := findAction(v, "look"); a != nil {
		t.Error("look should not be offered while a save waits pending")
	}
	// The saved transcript tail is on screen so the player remembers.
	if !transcriptHas(v, "Flooded Cellar") {
		t.Errorf("pending transcript = %v", labels(v))
	}
	// A typed command nudges to the choice, never an IPC error.
	v = h.cmd(t, "look")
	if !transcriptHas(v, "continue the saved expedition") {
		t.Errorf("pending nudge = %v", labels(v))
	}
	// Continue resumes exactly where the save left off.
	v = h.act(t, 1, "continue", nil)
	if findAction(v, "continue") != nil {
		t.Error("continue should be gone once resumed")
	}
	if !transcriptHas(v, "pick up where you left off") || !transcriptHas(v, "== Flooded Cellar ==") {
		t.Fatalf("resumed transcript = %v", labels(v))
	}
	var out struct {
		Room      string   `json:"room"`
		Inventory []string `json:"inventory"`
		Flags     []string `json:"flags"`
		Turns     int      `json:"turns"`
		Pending   bool     `json:"pending"`
	}
	h.route(t, "state", nil, &out)
	if out.Room != "cellar" || out.Pending || out.Turns != 6 {
		t.Fatalf("resumed state = %+v", out)
	}
	if len(out.Inventory) != 2 || len(out.Flags) != 1 || out.Flags[0] != "lamp_lit" {
		t.Errorf("resumed inventory/flags = %+v", out)
	}
}

func TestNewGameDiscardsSave(t *testing.T) {
	h := newHarness(t)
	h.seedSave(t, diskState{
		Version:    stateVersion,
		Room:       "study",
		Inventory:  []string{"elephant"},
		Turns:      40,
		Transcript: []string{"old history"},
	})
	h.view(t, 1)
	v := h.act(t, 1, "new_game", nil)
	if transcriptHas(v, "old history") {
		t.Error("new game should clear the saved transcript")
	}
	if !transcriptHas(v, "Dusty Foyer") {
		t.Errorf("new game transcript = %v", labels(v))
	}
	var out struct {
		Room  string `json:"room"`
		Turns int    `json:"turns"`
	}
	h.route(t, "state", nil, &out)
	if out.Room != "foyer" || out.Turns != 0 {
		t.Errorf("new game state = %+v", out)
	}
}

func TestTypedContinueAndRestartWhilePending(t *testing.T) {
	h := newHarness(t)
	h.seedSave(t, diskState{
		Version: stateVersion, Room: "kitchen", Turns: 3,
		Transcript: []string{"earlier line"},
	})
	v := h.cmd(t, "continue")
	if !transcriptHas(v, "== Cold Kitchen ==") {
		t.Fatalf("typed continue should resume in the kitchen: %v", labels(v))
	}
	if findAction(v, "continue") != nil {
		t.Error("should no longer be pending after typed continue")
	}
}

func TestPersistenceRoundTripMidGame(t *testing.T) {
	h := newHarness(t)
	h.cmd(t, "take lamp")
	h.cmd(t, "use lamp")
	h.cmd(t, "go north")
	h.cmd(t, "take note")
	h.cmd(t, "go south")
	h.cmd(t, "drop lamp")
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	// New instance, same partition. The save waits pending; continuing
	// restores room, flags, inventory, and where the lamp was left.
	h2 := newHarness(t)
	h2.files = h.files
	v := h2.view(t, 1)
	if findAction(v, "continue") == nil {
		t.Fatal("relaunch with a live save should offer continue")
	}
	v = h2.act(t, 1, "continue", nil)
	var out struct {
		Room      string   `json:"room"`
		Inventory []string `json:"inventory"`
		Flags     []string `json:"flags"`
	}
	h2.route(t, "state", nil, &out)
	if out.Room != "foyer" || len(out.Flags) != 1 || out.Flags[0] != "lamp_lit" {
		t.Fatalf("restored state = %+v", out)
	}
	if len(out.Inventory) != 1 || out.Inventory[0] != "note" {
		t.Errorf("restored inventory = %v", out.Inventory)
	}
	// The dropped lamp is still on the foyer floor.
	v = h2.act(t, 1, "look", nil)
	if !transcriptHas(v, "brass lamp") {
		t.Errorf("dropped lamp should be where it was left: %v", labels(v))
	}
	// And the transcript survived: the earlier commands are still there.
	if !transcriptHas(v, "> take lamp") {
		t.Errorf("transcript tail should have round-tripped: %v", labels(v))
	}
}

func TestCorruptSaveStartsFreshWithHonestNote(t *testing.T) {
	for _, bad := range [][]byte{
		[]byte("{not json"),
		[]byte(`{"version":99,"room":"foyer"}`),
		[]byte(""),
	} {
		h := newHarness(t)
		h.files[statePath] = bad
		v := h.view(t, 1)
		if v.State != sdk.ViewReady {
			t.Fatalf("corrupt save (%q) should not take down the view: %q", bad, v.State)
		}
		if !strings.Contains(v.Status, "unreadable") {
			t.Errorf("corrupt save (%q) status = %q, want an honest note", bad, v.Status)
		}
		if !transcriptHas(v, "THE BRASS ELEPHANT") || findAction(v, "continue") != nil {
			t.Errorf("corrupt save (%q) should start fresh, not pending", bad)
		}
		// The app stays playable; the next command saves over the bad bytes.
		v = h.cmd(t, "look")
		var st diskState
		if err := json.Unmarshal(h.savedFile(t), &st); err != nil {
			t.Errorf("post-corrupt save should parse: %v", err)
		}
	}
}

func TestSanitizeRepairsLoadedSave(t *testing.T) {
	h := newHarness(t)
	h.seedSave(t, diskState{
		Version:   stateVersion,
		Room:      "dungeon", // no such room
		Inventory: []string{"lamp", "phantom-item"},
		Locations: map[string]string{
			"key":     "dungeon", // bad room
			"note":    "kitchen", // a real move, kept
			"phantom": "kitchen", // bad item
		},
		Flags: []string{"lamp_lit", "bogus-flag"},
		Turns: -3,
		Ended: "undead",
		Transcript: []string{
			"fine line",
			"bad\nline",
			strings.Repeat("x", 400),
		},
	})
	v := h.view(t, 1)
	if findAction(v, "continue") == nil {
		t.Fatal("a repairable save should still offer continue")
	}
	v = h.act(t, 1, "continue", nil)
	var out struct {
		Room      string   `json:"room"`
		Inventory []string `json:"inventory"`
		Ended     string   `json:"ended"`
		Turns     int      `json:"turns"`
	}
	h.route(t, "state", nil, &out)
	if out.Room != "foyer" || out.Ended != "" || out.Turns != 0 {
		t.Errorf("repaired state = %+v", out)
	}
	if len(out.Inventory) != 1 || out.Inventory[0] != "lamp" {
		t.Errorf("repaired inventory = %v", out.Inventory)
	}
	if h.app.g.loc["note"] != "kitchen" || h.app.g.loc["key"] != "cellar" {
		t.Errorf("repaired locations: note=%q key=%q", h.app.g.loc["note"], h.app.g.loc["key"])
	}
	// The healed copy was written back: it parses to sound state.
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || st.Room != "foyer" {
		t.Fatalf("healed save = %v/%+v", err, st)
	}
}

func TestDeniedStorageKeepsWorking(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	v := h.view(t, 1)
	if v.State != sdk.ViewReady {
		t.Fatalf("denied storage should not be a crash: %q", v.State)
	}
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("denied read should be an honest banner: %q", v.Error)
	}
	v = h.cmd(t, "take lamp")
	if !transcriptHas(v, "You take the brass lamp") {
		t.Error("the game should still play while storage is denied")
	}
	if !strings.Contains(v.Status, "save failed") {
		t.Errorf("failed save should be an honest status: %q", v.Status)
	}
	h.fmu.Lock()
	_, wrote := h.files[statePath]
	h.fmu.Unlock()
	if wrote {
		t.Error("denied save should not have produced a file")
	}

	// The grant lands later: the next command persists everything.
	h.deny = false
	h.cmd(t, "look")
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Inventory) != 1 {
		t.Fatalf("after grant, save should hold the lamp: %v/%+v", err, st)
	}
}

func TestStopFlushesPendingSave(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	h.cmd(t, "take lamp")
	h.deny = false
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st diskState
	if err := json.Unmarshal(h.savedFile(t), &st); err != nil || len(st.Inventory) != 1 {
		t.Fatalf("stop should flush the pending save: %v/%+v", err, st)
	}
}

// --- routes ---

func TestRoutes(t *testing.T) {
	h := newHarness(t)
	// command route runs the same parser; failures stay in-fiction.
	var cmdOut struct {
		Lines []string `json:"lines"`
		Room  string   `json:"room"`
	}
	h.route(t, "command", map[string]string{"text": "xyzzy"}, &cmdOut)
	if len(cmdOut.Lines) == 0 || !strings.Contains(cmdOut.Lines[len(cmdOut.Lines)-1], "can't") {
		t.Fatalf("command route on unknown verb = %+v", cmdOut)
	}
	h.route(t, "command", map[string]string{"text": "go north"}, &cmdOut)
	if cmdOut.Room != "kitchen" {
		t.Fatalf("command route go north -> %q", cmdOut.Room)
	}
	if _, err := h.tryRoute("command", map[string]int{"text": 7}); err == nil {
		t.Error("malformed command params should be an IPC error")
	}
	var stOut struct {
		Room string `json:"room"`
	}
	h.route(t, "state", nil, &stOut)
	if stOut.Room != "kitchen" {
		t.Errorf("state route room = %q", stOut.Room)
	}
	var rsOut struct {
		Restarted bool `json:"restarted"`
	}
	h.route(t, "restart", nil, &rsOut)
	if !rsOut.Restarted {
		t.Error("restart route should report restarted")
	}
	h.route(t, "state", nil, &stOut)
	if stOut.Room != "foyer" {
		t.Errorf("post-restart room = %q", stOut.Room)
	}
}

// --- concurrency ---

func TestConcurrentHandlersAndActions(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	instance := v.Instance

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				raw, _ := json.Marshal(map[string]string{"text": fmt.Sprintf("frotz %d", i)})
				if _, err := h.handlers["command"](context.Background(), raw); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := h.handlers["view"](context.Background(), json.RawMessage(`{"version":1}`)); err != nil {
					errs <- err
				}
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				req := sdk.ActionRequest{
					Version:   1,
					Instance:  instance,
					RequestID: fmt.Sprintf("c_%d_%d", g, i),
					Action:    "help",
				}
				raw, _ := json.Marshal(req)
				if _, err := h.handlers["action"](context.Background(), raw); err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent handler error: %v", err)
	}
	var out struct {
		TranscriptLines int `json:"transcript_lines"`
	}
	h.route(t, "state", nil, &out)
	if out.TranscriptLines > transcriptKeep {
		t.Errorf("transcript = %d lines, bound is %d", out.TranscriptLines, transcriptKeep)
	}
}

// A transient fs/read failure (a canceled call racing the first view, a
// host I/O error, the read-size bound) is not "no save yet": the app must
// say so honestly, retry the load, and never let a fresh expedition
// overwrite the saved one.
func TestTransientReadErrorNeverOverwritesSave(t *testing.T) {
	h := newHarness(t)
	h.seedSave(t, diskState{
		Version:   stateVersion,
		Room:      "cellar",
		Inventory: []string{"lamp", "key"},
		Flags:     []string{"lamp_lit"},
		Turns:     6,
		Transcript: []string{
			"— THE BRASS ELEPHANT —", "> go down", "== Flooded Cellar ==",
		},
	})
	h.readErr = context.Canceled // first fs/read fails once, transiently

	v := h.view(t, 1)
	if v.State != sdk.ViewError || v.Error == "" {
		t.Fatalf("transient read failure view = %+v, want an honest error", v)
	}

	// The retry revives the real save: only a loaded file offers the
	// pending continue-or-new choice.
	v = h.view(t, 1)
	if v.State == sdk.ViewError || !strings.Contains(v.Status, "earlier expedition") {
		t.Fatalf("retry did not revive the pending save: %+v", v)
	}
	h.act(t, 1, "continue", nil)
	h.cmd(t, "look")

	data, ok := h.files[statePath]
	if !ok {
		t.Fatal("no saved state")
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("post-change save is not valid json: %v", err)
	}
	if st.Room != "cellar" || len(st.Inventory) != 2 {
		t.Fatalf("persisted expedition after transient failure = %+v, want the seeded run kept", st)
	}
}
