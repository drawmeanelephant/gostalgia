package delve

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// fakeGameEngine is a scripted engine.Game for the gameEngine seam: it
// records commands, replays queued events, and reports settable vitals.
// Every access runs under the Delve mutex, so it needs no lock of its own.
type fakeGameEngine struct {
	cfg      engine.Config
	cmds     []engine.Cmd
	events   []engine.Event // replayed, then cleared, by the next Command
	onCmd    func(engine.Cmd) []engine.Event
	outcome  engine.Outcome
	stats    engine.Stats
	flags    engine.Flags
	inv      []engine.InvEntry
	mapLines []string
	log      []string
	score    engine.Score
	blob     []byte
	mErr     error
}

func (f *fakeGameEngine) Command(c engine.Cmd) []engine.Event {
	f.cmds = append(f.cmds, c)
	if f.onCmd != nil {
		return f.onCmd(c)
	}
	ev := f.events
	f.events = nil
	return ev
}

func (f *fakeGameEngine) MapView() []string            { return f.mapLines }
func (f *fakeGameEngine) LogTail(n int) []string       { return tailLines(f.log, n) }
func (f *fakeGameEngine) Stats() engine.Stats          { return f.stats }
func (f *fakeGameEngine) Flags() engine.Flags          { return f.flags }
func (f *fakeGameEngine) Inventory() []engine.InvEntry { return f.inv }
func (f *fakeGameEngine) Outcome() engine.Outcome      { return f.outcome }
func (f *fakeGameEngine) FinalScore() engine.Score     { return f.score }
func (f *fakeGameEngine) Marshal() ([]byte, error)     { return f.blob, f.mErr }

func newFakeEngine() *fakeGameEngine {
	lines := make([]string, engine.MapHeight)
	for i := range lines {
		lines[i] = strings.Repeat(".", engine.MapWidth)
	}
	return &fakeGameEngine{
		stats: engine.Stats{
			HP: 10, MaxHP: 12, Level: 1, XP: 7, XPNext: 20,
			Satiety: 800, Depth: 1, Turns: 3, Kills: 1,
			Attack: 3, Defense: 1, Weapon: "dagger",
		},
		mapLines: lines,
		log:      []string{"You descend into the Underhall.", "It is dark down here."},
		inv: []engine.InvEntry{
			{ID: 3, Label: "a) dagger", Detail: "+3", Kind: engine.ItemWeapon, Equipped: true},
			{ID: 7, Label: "b) potion of healing", Kind: engine.ItemPotion},
		},
		blob:  []byte("fake-run-bytes"),
		score: engine.Score{Name: "Rex", Value: 120, Depth: 2, Outcome: "slain by a goblin", Turns: 44},
	}
}

// harness models the app-private VFS partition plus the sound service.
// The newGame/reviveGame package vars are pointed at fakes for each test.
type harness struct {
	deny     bool // fs/read+fs/save denied: simulates missing grants
	noSound  bool // sound/status reports unsupported
	soundErr bool // sound/play calls fail
	seq      atomic.Int64

	fmu    sync.Mutex
	files  map[string][]byte
	sounds []string // raw params of each sound/play call

	app      *Delve
	handlers map[string]sdk.Handler

	game      *fakeGameEngine // returned by newGame
	revived   *fakeGameEngine // returned by reviveGame
	newCalls  []engine.Config
	revCalls  [][]byte
	newErr    error
	reviveErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		files:    map[string][]byte{},
		handlers: map[string]sdk.Handler{},
		game:     newFakeEngine(),
		revived:  newFakeEngine(),
	}
	origNew, origRevive := newGame, reviveGame
	newGame = func(cfg engine.Config) (gameEngine, error) {
		if h.newErr != nil {
			return nil, h.newErr
		}
		h.newCalls = append(h.newCalls, cfg)
		// A fresh run reuses the shared fake with run-scoped state reset;
		// configured vitals (stats, flags, inventory) persist.
		h.game.cfg = cfg
		h.game.cmds = nil
		h.game.events = nil
		h.game.onCmd = nil
		h.game.outcome = engine.OutcomeOngoing
		return h.game, nil
	}
	reviveGame = func(b []byte) (gameEngine, error) {
		if h.reviveErr != nil {
			return nil, h.reviveErr
		}
		h.revCalls = append(h.revCalls, append([]byte(nil), b...))
		return h.revived, nil
	}
	t.Cleanup(func() { newGame, reviveGame = origNew, origRevive })

	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	dv := inst.(*Delve)
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := dv.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = dv
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
	switch method {
	case "fs/read":
		if h.deny {
			return errors.New("permission denied: caller not authorized")
		}
		data, ok := h.files[p.Path]
		if !ok {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		if h.deny {
			return errors.New("permission denied: caller not authorized")
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	case "session/whoami":
		return reply(map[string]any{"capabilities": []string{sdk.CapSound}})
	case "sound/status":
		return reply(map[string]any{"supported": !h.noSound, "player": "test-player"})
	case "sound/play":
		if h.soundErr {
			return errors.New("platform: audio playback busy")
		}
		h.sounds = append(h.sounds, string(raw))
		return reply(map[string]any{"played": true})
	}
	return fmt.Errorf("unknown method %s", method)
}

func (h *harness) savedFile(t *testing.T, path string) []byte {
	t.Helper()
	h.fmu.Lock()
	defer h.fmu.Unlock()
	data, ok := h.files[path]
	if !ok {
		t.Fatalf("no saved state at %s", path)
	}
	return append([]byte(nil), data...)
}

func (h *harness) seedFile(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	h.fmu.Lock()
	h.files[path] = data
	h.fmu.Unlock()
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

// tryAct drives the real action route: it fetches a view first so the
// request carries a live instance, exactly like the shell does.
func (h *harness) tryAct(version int, req sdk.ActionRequest) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req.Version = version
	req.Instance = cur.(sdk.View).Instance
	req.RequestID = fmt.Sprintf("req_%d", h.seq.Add(1))
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

func (h *harness) act(t *testing.T, version int, req sdk.ActionRequest) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, req)
	if err != nil {
		t.Fatalf("action %s: %v", req.Action, err)
	}
	return v
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

func findField(v sdk.View, id string) *sdk.Field {
	for i := range v.Fields {
		if v.Fields[i].ID == id {
			return &v.Fields[i]
		}
	}
	return nil
}

func findBlock(v sdk.View, id string) *sdk.Block {
	for i := range v.Blocks {
		if v.Blocks[i].ID == id {
			return &v.Blocks[i]
		}
	}
	return nil
}

func findMeter(v sdk.View, id string) *sdk.Meter {
	for i := range v.Meters {
		if v.Meters[i].ID == id {
			return &v.Meters[i]
		}
	}
	return nil
}

func findItem(v sdk.View, id string) *sdk.Item {
	for i := range v.Items {
		if v.Items[i].ID == id {
			return &v.Items[i]
		}
	}
	return nil
}

// startRun begins a delve through the "new" action and returns the view.
func (h *harness) startRun(t *testing.T, name string) sdk.View {
	t.Helper()
	values := map[string]string{}
	if name != "" {
		values["name"] = name
	}
	return h.act(t, 3, sdk.ActionRequest{Action: "new", Values: values})
}

// --- manifest ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "delve" {
		t.Errorf("manifest = %s/%s", m.ID, m.Entrypoint)
	}
	want := map[string]bool{"ipc": true, "fs.read": true, "fs.write": true, "sound": true}
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

// --- pre-run screen ---

func TestFreshViewIsPreRunScreen(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 3)
	if v.State != sdk.ViewReady || v.Title != delveTitle {
		t.Fatalf("fresh view = %q/%q", v.State, v.Title)
	}
	if findField(v, "name") == nil {
		t.Error("pre-run view should carry the name field")
	}
	for _, id := range []string{"new", "help"} {
		if a := findAction(v, id); a == nil || a.Disabled {
			t.Errorf("pre-run action %s should be offered", id)
		}
	}
	if v.Grid != nil || len(v.Blocks) != 0 || len(v.Meters) != 0 {
		t.Error("pre-run view should carry no world surfaces")
	}
	if !strings.Contains(v.Status, "cellar stairs") {
		t.Errorf("pre-run status = %q", v.Status)
	}
	if findItem(v, "ledger_empty") == nil {
		t.Errorf("empty ledger should render honestly: %+v", v.Items)
	}
}

// --- run view composition ---

func TestNewGameStartsRun(t *testing.T) {
	h := newHarness(t)
	v := h.startRun(t, "Rex")
	if len(h.newCalls) != 1 {
		t.Fatalf("newGame calls = %d", len(h.newCalls))
	}
	cfg := h.newCalls[0]
	if cfg.Name != "Rex" || cfg.Seed == 0 {
		t.Errorf("engine config = %+v", cfg)
	}
	if b := findBlock(v, "map"); b == nil || strings.Count(b.Text, "\n")+1 != engine.MapHeight {
		t.Fatalf("map block = %+v", b)
	}
	for _, id := range []string{"hp", "satiety", "xp"} {
		if findMeter(v, id) == nil {
			t.Errorf("meter %s missing", id)
		}
	}
	if v.Region == nil || v.Region.ID != "log" || !strings.Contains(v.Region.Text, "Underhall") {
		t.Fatalf("log region = %+v", v.Region)
	}
	if v.Grid == nil || len(v.Grid.Cells) != 9 || v.Grid.Columns != 3 {
		t.Fatalf("pad = %+v", v.Grid)
	}
	for _, id := range []string{"pickup", "descend", "ascend", "rest", "use", "equip", "drop", "new", "help"} {
		if findAction(v, id) == nil {
			t.Errorf("run action %s missing", id)
		}
	}
	if it := findItem(v, "item_3"); it == nil || !strings.Contains(it.Detail, "equipped") {
		t.Errorf("inventory item_3 = %+v", it)
	}
	if !strings.Contains(v.Status, "Underhall — level 1") {
		t.Errorf("status = %q", v.Status)
	}
}

func TestMovePadDispatchesDirections(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	want := map[string]engine.Cmd{
		"nw":   {Kind: engine.CmdMove, Dir: engine.DirNW},
		"n":    {Kind: engine.CmdMove, Dir: engine.DirN},
		"ne":   {Kind: engine.CmdMove, Dir: engine.DirNE},
		"w":    {Kind: engine.CmdMove, Dir: engine.DirW},
		"e":    {Kind: engine.CmdMove, Dir: engine.DirE},
		"sw":   {Kind: engine.CmdMove, Dir: engine.DirSW},
		"s":    {Kind: engine.CmdMove, Dir: engine.DirS},
		"se":   {Kind: engine.CmdMove, Dir: engine.DirSE},
		"wait": {Kind: engine.CmdWait},
	}
	for cellID, cmd := range want {
		h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: cellID})
		got := h.game.cmds[len(h.game.cmds)-1]
		if got != cmd {
			t.Errorf("cell %s -> %+v, want %+v", cellID, got, cmd)
		}
	}
	if len(h.game.cmds) != len(want) {
		t.Errorf("commands = %d, want %d", len(h.game.cmds), len(want))
	}
	if _, err := h.tryAct(3, sdk.ActionRequest{Action: "move", CellID: "nowhere"}); err == nil {
		t.Error("unknown cell should be an IPC error")
	}
}

func TestItemActionsPassItemID(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	for action, kind := range map[string]engine.CmdKind{
		"use": engine.CmdUseItem, "equip": engine.CmdEquip, "drop": engine.CmdDrop,
	} {
		h.act(t, 3, sdk.ActionRequest{Action: action, ItemID: "item_7"})
		got := h.game.cmds[len(h.game.cmds)-1]
		if got.Kind != kind || got.ItemID != 7 {
			t.Errorf("%s -> %+v, want kind %d item 7", action, got, kind)
		}
	}
	// A malformed item id is an in-fiction nudge, never an IPC error.
	v := h.act(t, 3, sdk.ActionRequest{Action: "use", ItemID: ""})
	if !strings.Contains(v.Status, "select an item") {
		t.Errorf("empty item select status = %q", v.Status)
	}
}

func TestActionsDimByFlags(t *testing.T) {
	h := newHarness(t)
	v := h.startRun(t, "")
	for _, id := range []string{"pickup", "descend", "ascend"} {
		if a := findAction(v, id); a == nil || !a.Disabled {
			t.Errorf("%s should be disabled without the flag", id)
		}
	}
	h.game.flags = engine.Flags{CanPickup: true, CanDescend: true, CanAscend: true}
	v = h.view(t, 3)
	for _, id := range []string{"pickup", "descend", "ascend"} {
		if a := findAction(v, id); a == nil || a.Disabled {
			t.Errorf("%s should be enabled with the flag", id)
		}
	}
	// Item actions dim when no suitable kind is in the pack.
	h.game.inv = []engine.InvEntry{{ID: 9, Label: "a) iron ration", Kind: engine.ItemFood}}
	v = h.view(t, 3)
	if a := findAction(v, "equip"); a == nil || !a.Disabled {
		t.Error("equip should be disabled with nothing equippable")
	}
	if a := findAction(v, "use"); a == nil || a.Disabled {
		t.Error("use should stay enabled with food in the pack")
	}
	h.game.inv = nil
	v = h.view(t, 3)
	if a := findAction(v, "drop"); a == nil || !a.Disabled {
		t.Error("drop should be disabled with an empty pack")
	}
}

// --- ended runs ---

func TestDeadRunShowsTombstoneAndScores(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "Rex")
	h.game.onCmd = func(engine.Cmd) []engine.Event {
		h.game.outcome = engine.OutcomeDead
		return []engine.Event{{Kind: engine.EvDeath, Text: "The goblin bites."}, {Kind: engine.EvLose, Text: "You die."}}
	}
	v := h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "s"})
	if v.Title != delveTitle+" — you died" {
		t.Errorf("dead title = %q", v.Title)
	}
	b := findBlock(v, "end")
	if b == nil || !strings.Contains(b.Text, "HERE LIES") || !strings.Contains(b.Text, "slain by a goblin") {
		t.Fatalf("tombstone = %+v", b)
	}
	if v.Grid != nil {
		t.Error("tombstone view should carry no pad")
	}
	if findField(v, "name") == nil {
		t.Error("tombstone is a new-run screen: the name field should show")
	}
	if findAction(v, "new") == nil || findAction(v, "rest") != nil || findAction(v, "pickup") != nil {
		t.Error("ended run should offer only new/help actions")
	}
	// The ledger recorded the final score.
	var sc diskScores
	if err := json.Unmarshal(h.savedFile(t, scoresPath), &sc); err != nil {
		t.Fatalf("scores parse: %v", err)
	}
	if len(sc.Scores) != 1 || sc.Scores[0].Value != 120 || sc.Scores[0].Outcome != "slain by a goblin" {
		t.Fatalf("ledger = %+v", sc.Scores)
	}
	// Further commands reach the engine never; the route reports the end.
	var out struct {
		Outcome int `json:"outcome"`
	}
	h.route(t, "command", map[string]any{"kind": "wait"}, &out)
	if out.Outcome != int(engine.OutcomeDead) {
		t.Errorf("post-death route outcome = %d", out.Outcome)
	}
	if len(h.game.cmds) != 1 {
		t.Errorf("dead run should not consume commands: %d", len(h.game.cmds))
	}
}

func TestWonRunShowsVictory(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	h.game.onCmd = func(engine.Cmd) []engine.Event {
		h.game.outcome = engine.OutcomeWon
		h.game.score = engine.Score{Name: "Rex", Value: 999, Depth: 1, Outcome: "won", Turns: 300}
		return []engine.Event{{Kind: engine.EvWin, Text: "You escape with the Brass Idol!"}}
	}
	v := h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "n"})
	if v.Title != delveTitle+" — you escaped!" {
		t.Errorf("won title = %q", v.Title)
	}
	b := findBlock(v, "end")
	if b == nil || !strings.Contains(b.Text, "BRASS IDOL") || !strings.Contains(b.Text, "999 points") {
		t.Fatalf("victory block = %+v", b)
	}
}

func TestLedgerKeepsTopTenSorted(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 12; i++ {
		h.startRun(t, "")
		h.game.onCmd = func(engine.Cmd) []engine.Event {
			h.game.outcome = engine.OutcomeDead
			return nil
		}
		h.game.score = engine.Score{Value: (i%7)*100 + i, Depth: 1, Outcome: "slain", Turns: i}
		h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "s"})
	}
	var sc diskScores
	if err := json.Unmarshal(h.savedFile(t, scoresPath), &sc); err != nil {
		t.Fatalf("scores parse: %v", err)
	}
	if len(sc.Scores) != maxScores {
		t.Fatalf("ledger = %d entries, bound is %d", len(sc.Scores), maxScores)
	}
	for i := 1; i < len(sc.Scores); i++ {
		if sc.Scores[i-1].Value < sc.Scores[i].Value {
			t.Errorf("ledger not sorted desc at %d: %+v", i, sc.Scores)
		}
	}
}

// --- persistence ---

func TestCommandsPersistAfterEveryTurn(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "Rex")
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "ne"})
	var st diskState
	if err := json.Unmarshal(h.savedFile(t, statePath), &st); err != nil {
		t.Fatalf("save parses: %v", err)
	}
	if st.Version != stateVersion || st.PlayerName != "Rex" || st.Seed != h.newCalls[0].Seed {
		t.Errorf("save = %+v", st)
	}
	run, err := base64.StdEncoding.DecodeString(st.Run)
	if err != nil || string(run) != "fake-run-bytes" {
		t.Errorf("run blob = %q/%v", run, err)
	}
	if st.Started == 0 {
		t.Error("started should be stamped")
	}
}

func seedSave(t *testing.T, h *harness, st diskState) {
	t.Helper()
	h.seedFile(t, statePath, st)
}

func TestRelaunchOffersContinueOrNew(t *testing.T) {
	h := newHarness(t)
	seedSave(t, h, diskState{
		Version:    stateVersion,
		Run:        base64.StdEncoding.EncodeToString([]byte("revived-blob")),
		PlayerName: "Old Delver",
		Seed:       99,
		Started:    1700000000,
	})
	v := h.view(t, 3)
	if !strings.Contains(v.Status, "earlier delve") {
		t.Fatalf("pending status = %q", v.Status)
	}
	for _, id := range []string{"continue", "new"} {
		if a := findAction(v, id); a == nil || a.Disabled {
			t.Errorf("pending action %s should be offered", id)
		}
	}
	if len(h.revCalls) != 1 || string(h.revCalls[0]) != "revived-blob" {
		t.Fatalf("revive calls = %q", h.revCalls)
	}
	// The frozen world is on screen but the pad is dimmed.
	if b := findBlock(v, "map"); b == nil {
		t.Error("pending view should still show the map")
	}
	if v.Grid == nil {
		t.Fatal("pending view should still show the pad")
	}
	for _, c := range v.Grid.Cells {
		if !c.Disabled {
			t.Errorf("cell %s should be dimmed while pending", c.ID)
		}
	}
	// Continue resumes the revived run.
	v = h.act(t, 3, sdk.ActionRequest{Action: "continue"})
	if findAction(v, "continue") != nil {
		t.Error("continue should be gone once resumed")
	}
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "n"})
	if len(h.revived.cmds) != 1 || h.revived.cmds[0].Dir != engine.DirN {
		t.Fatalf("resumed run commands = %+v", h.revived.cmds)
	}
}

func TestNewWhilePendingDiscardsSave(t *testing.T) {
	h := newHarness(t)
	seedSave(t, h, diskState{
		Version: stateVersion,
		Run:     base64.StdEncoding.EncodeToString([]byte("revived-blob")),
	})
	v := h.act(t, 3, sdk.ActionRequest{Action: "new", Values: map[string]string{"name": "Fresh"}})
	if findAction(v, "continue") != nil {
		t.Error("new delve should clear pending")
	}
	if len(h.newCalls) != 1 || h.newCalls[0].Name != "Fresh" {
		t.Fatalf("new calls = %+v", h.newCalls)
	}
	if len(h.revived.cmds) != 0 {
		t.Error("the revived run should never see a command")
	}
}

func TestRevivedEndedRunShowsEndScreen(t *testing.T) {
	h := newHarness(t)
	h.revived.outcome = engine.OutcomeDead
	h.revived.score = engine.Score{Name: "Old", Value: 5, Depth: 3, Outcome: "starved"}
	seedSave(t, h, diskState{
		Version:    stateVersion,
		Run:        base64.StdEncoding.EncodeToString([]byte("dead-run")),
		PlayerName: "Old",
	})
	v := h.view(t, 3)
	if findAction(v, "continue") != nil {
		t.Error("an ended save should not offer continue")
	}
	if b := findBlock(v, "end"); b == nil || !strings.Contains(b.Text, "HERE LIES") {
		t.Fatalf("ended save should show the tombstone: %+v", v.Blocks)
	}
}

func TestCorruptSaveHealsFresh(t *testing.T) {
	for _, bad := range [][]byte{
		[]byte("{not json"),
		[]byte(`{"version":99,"run":"eA=="}`),
		[]byte(""),
	} {
		h := newHarness(t)
		h.files[statePath] = bad
		v := h.view(t, 3)
		if v.State != sdk.ViewReady {
			t.Fatalf("corrupt save (%q) should not take down the view: %q", bad, v.State)
		}
		if !strings.Contains(v.Status, "unreadable") {
			t.Errorf("corrupt save (%q) status = %q, want an honest note", bad, v.Status)
		}
		if findField(v, "name") == nil || findAction(v, "continue") != nil {
			t.Errorf("corrupt save (%q) should land on the pre-run screen", bad)
		}
		// The bad bytes are left alone until a real change saves over them.
		h.fmu.Lock()
		still := h.files[statePath]
		h.fmu.Unlock()
		if string(still) != string(bad) {
			t.Errorf("corrupt save (%q) was overwritten without a real change", bad)
		}
		v = h.startRun(t, "")
		if findField(v, "name") != nil {
			t.Error("run started: name field should be gone")
		}
		var st diskState
		if err := json.Unmarshal(h.savedFile(t, statePath), &st); err != nil || st.Version != stateVersion {
			t.Errorf("post-corrupt save should parse: %v/%+v", err, st)
		}
	}
}

func TestReviveFailureHealsFresh(t *testing.T) {
	h := newHarness(t)
	h.reviveErr = errors.New("engine: unknown save version")
	seedSave(t, h, diskState{Version: stateVersion, Run: base64.StdEncoding.EncodeToString([]byte("x"))})
	v := h.view(t, 3)
	if !strings.Contains(v.Status, "unreadable") {
		t.Errorf("revive failure status = %q", v.Status)
	}
	if findField(v, "name") == nil {
		t.Error("revive failure should land on the pre-run screen")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "Rex")
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "e"})
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// New instance, same partition: the save waits pending, continue
	// picks up the run where Marshal left it.
	h2 := newHarness(t)
	h2.files = h.files
	v := h2.view(t, 3)
	if findAction(v, "continue") == nil {
		t.Fatal("relaunch with a live save should offer continue")
	}
	if string(h2.revCalls[0]) != "fake-run-bytes" {
		t.Fatalf("revive got %q", h2.revCalls)
	}
	v = h2.act(t, 3, sdk.ActionRequest{Action: "continue"})
	if !strings.Contains(v.Status, "pick up the descent") {
		t.Errorf("resumed status = %q", v.Status)
	}
}

func TestDeniedStorageKeepsWorking(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	v := h.view(t, 3)
	if v.State != sdk.ViewReady {
		t.Fatalf("denied storage should not be a crash: %q", v.State)
	}
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("denied read should be an honest banner: %q", v.Error)
	}
	v = h.startRun(t, "Rex")
	if v.State != sdk.ViewReady {
		t.Errorf("denied save should still start the run: %q", v.State)
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
	// The grant lands later: the next turn persists everything.
	h.deny = false
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "n"})
	var st diskState
	if err := json.Unmarshal(h.savedFile(t, statePath), &st); err != nil || st.PlayerName == "" {
		t.Fatalf("after grant, save should hold the run: %v/%+v", err, st)
	}
}

func TestStopFlushesPendingSave(t *testing.T) {
	h := newHarness(t)
	h.deny = true
	h.startRun(t, "")
	h.deny = false
	if err := h.app.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	var st diskState
	if err := json.Unmarshal(h.savedFile(t, statePath), &st); err != nil {
		t.Fatalf("stop should flush the pending save: %v", err)
	}
}

// --- presentation misc ---

func TestHelpTogglesRegion(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	v := h.act(t, 3, sdk.ActionRequest{Action: "help"})
	if v.Region == nil || v.Region.ID != "help" || !strings.Contains(v.Region.Text, "Controls") {
		t.Fatalf("help region = %+v", v.Region)
	}
	v = h.act(t, 3, sdk.ActionRequest{Action: "help"})
	if v.Region == nil || v.Region.ID != "log" {
		t.Fatalf("untoggled region = %+v", v.Region)
	}
}

func TestXPMaxMeterGuard(t *testing.T) {
	h := newHarness(t)
	h.game.stats.XPNext = 0
	v := h.startRun(t, "")
	m := findMeter(v, "xp")
	if m == nil || m.Value != 1.0 || !strings.Contains(m.Label, "max") {
		t.Fatalf("max-level xp meter = %+v", m)
	}
	// Zero MaxHP must never produce a non-finite meter either.
	h.game.stats.MaxHP = 0
	h.game.stats.HP = 0
	v = h.view(t, 3)
	if m := findMeter(v, "hp"); m == nil || m.Value != 0 {
		t.Fatalf("zero-hp meter = %+v", m)
	}
}

func TestVersionDegradation(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	v2 := h.view(t, 2)
	if v2.Region != nil {
		t.Error("v2 must not carry the scroll region")
	}
	if findBlock(v2, "map") == nil || findBlock(v2, "log") == nil || v2.Grid == nil || len(v2.Meters) != 3 {
		t.Error("v2 should keep map, log block, meters, and pad")
	}
	v1 := h.view(t, 1)
	if len(v1.Blocks) != 0 || len(v1.Meters) != 0 || v1.Grid != nil || v1.Region != nil {
		t.Error("v1 must carry no version-2/3 elements")
	}
	if findItem(v1, "map00") == nil || findItem(v1, "log00") == nil || findItem(v1, "item_3") == nil {
		t.Error("v1 should render map, log, and pack as items")
	}
	if len(v1.Items) > 100 {
		t.Errorf("v1 items = %d, over the 100 cap", len(v1.Items))
	}
}

func TestUnknownActionIsIPCError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(3, sdk.ActionRequest{Action: "launch_missiles"}); err == nil {
		t.Error("unknown action should be an IPC error")
	}
}

// --- sound ---

func TestEventSoundsFireByKind(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	h.game.events = []engine.Event{
		{Kind: engine.EvHit, Text: "You hit."},
		{Kind: engine.EvDeath, Text: "The rat dies."},
		{Kind: engine.EvMessage, Text: "quiet."},
		{Kind: engine.EvHit, Text: "You hit again."},
	}
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "e"})
	if len(h.sounds) != 2 {
		t.Fatalf("sound calls = %v", h.sounds)
	}
	// Priority order: death's bark before the hit beep; dupes deduped.
	if !strings.Contains(h.sounds[0], `"bark"`) || !strings.Contains(h.sounds[1], `"beep"`) {
		t.Errorf("sounds = %v", h.sounds)
	}
}

func TestSoundStingersAndBlips(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	h.game.events = []engine.Event{{Kind: engine.EvLevelUp, Text: "Level 2!"}, {Kind: engine.EvPickup, Text: "You take the dagger."}}
	h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "n"})
	if len(h.sounds) != 2 || !strings.Contains(h.sounds[0], "notes") || !strings.Contains(h.sounds[1], "notes") {
		t.Fatalf("level-up arpeggio + pickup blip = %v", h.sounds)
	}
}

func TestSoundFailuresNeverBreakCommands(t *testing.T) {
	h := newHarness(t)
	h.soundErr = true
	h.startRun(t, "")
	h.game.events = []engine.Event{{Kind: engine.EvHit, Text: "You hit."}}
	v := h.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "e"})
	if v.State != sdk.ViewReady || v.Error != "" {
		t.Fatalf("sound failure surfaced: %q/%q", v.State, v.Error)
	}
	if len(h.game.cmds) != 1 {
		t.Error("command still ran despite dead audio")
	}
	// An unsupported host never even sees the play call.
	h2 := newHarness(t)
	h2.noSound = true
	h2.startRun(t, "")
	h2.game.events = []engine.Event{{Kind: engine.EvDeath, Text: "It dies."}}
	h2.act(t, 3, sdk.ActionRequest{Action: "move", CellID: "w"})
	if len(h2.sounds) != 0 {
		t.Errorf("unsupported host should get no play calls: %v", h2.sounds)
	}
}

// --- routes ---

func TestRoutes(t *testing.T) {
	h := newHarness(t)
	// No run yet: command errors honestly, restart begins one.
	if _, err := h.tryRoute("command", map[string]any{"kind": "move", "dir": "n"}); err == nil {
		t.Error("command with no run should be an IPC error")
	}
	var rsOut struct {
		Restarted bool   `json:"restarted"`
		Seed      uint64 `json:"seed"`
	}
	h.route(t, "restart", map[string]string{"name": "Rex"}, &rsOut)
	if !rsOut.Restarted || rsOut.Seed == 0 {
		t.Fatalf("restart = %+v", rsOut)
	}
	var cmdOut struct {
		Events  []engine.Event `json:"events"`
		Outcome int            `json:"outcome"`
		Depth   int            `json:"depth"`
	}
	h.route(t, "command", map[string]any{"kind": "move", "dir": "se"}, &cmdOut)
	if got := h.game.cmds[len(h.game.cmds)-1]; got.Kind != engine.CmdMove || got.Dir != engine.DirSE {
		t.Fatalf("route command -> %+v", got)
	}
	h.route(t, "command", map[string]any{"kind": "use", "item_id": 7}, &cmdOut)
	if got := h.game.cmds[len(h.game.cmds)-1]; got.Kind != engine.CmdUseItem || got.ItemID != 7 {
		t.Fatalf("route item command -> %+v", got)
	}
	for _, bad := range []map[string]any{
		{"kind": "move", "dir": "sideways"},
		{"kind": "frobnicate"},
	} {
		if _, err := h.tryRoute("command", bad); err == nil {
			t.Errorf("malformed command %v should be an IPC error", bad)
		}
	}
	var stOut struct {
		Outcome int  `json:"outcome"`
		Depth   int  `json:"depth"`
		HP      int  `json:"hp"`
		Run     bool `json:"run"`
	}
	h.route(t, "state", nil, &stOut)
	if !stOut.Run || stOut.HP != 10 || stOut.Depth != 1 {
		t.Errorf("state = %+v", stOut)
	}
	// Restart abandons the run for a fresh seed.
	h.route(t, "restart", nil, &rsOut)
	if len(h.newCalls) != 2 {
		t.Errorf("restart should have started a second run: %d calls", len(h.newCalls))
	}
}

func TestCommandRouteResumesPendingSave(t *testing.T) {
	h := newHarness(t)
	seedSave(t, h, diskState{
		Version: stateVersion,
		Run:     base64.StdEncoding.EncodeToString([]byte("revived-blob")),
	})
	h.view(t, 3)
	var out struct {
		Outcome int `json:"outcome"`
	}
	h.route(t, "command", map[string]any{"kind": "wait"}, &out)
	if len(h.revived.cmds) != 1 || h.revived.cmds[0].Kind != engine.CmdWait {
		t.Fatalf("pending command -> %+v", h.revived.cmds)
	}
	if h.app.pending {
		t.Error("an explicit command should resume the pending save")
	}
}

// --- engine seam ---

// TestEngineUnavailableIsGraceful points the seams at the real package:
// while engine.New still answers errPending the shell must show an honest
// status, not panic. When the engine lands this test exercises a real run
// instead — either way it proves the seam compiles against the contract.
func TestEngineUnavailableIsGraceful(t *testing.T) {
	h := newHarness(t)
	newGame = func(cfg engine.Config) (gameEngine, error) { return engine.New(cfg) }
	reviveGame = func(b []byte) (gameEngine, error) { return engine.Unmarshal(b) }
	v := h.startRun(t, "")
	if v.State != sdk.ViewReady {
		t.Fatalf("engine error should not take down the view: %q", v.State)
	}
	if h.app.g == nil {
		// Engine still pending: the honest closed-cellar note.
		if !strings.Contains(v.Status, "delve is not yet open") {
			t.Errorf("unavailable engine status = %q", v.Status)
		}
		return
	}
	// Engine landed: a real run should be underway.
	if findBlock(v, "map") == nil || v.Grid == nil {
		t.Error("live engine should produce the full run view")
	}
}

// --- concurrency ---

func TestConcurrentHandlersAndActions(t *testing.T) {
	h := newHarness(t)
	h.startRun(t, "")
	v := h.view(t, 3)
	instance := v.Instance

	var wg sync.WaitGroup
	errs := make(chan error, 256)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				raw, _ := json.Marshal(map[string]any{"kind": "wait"})
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
				if _, err := h.handlers["view"](context.Background(), json.RawMessage(`{"version":3}`)); err != nil {
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
					Version:   3,
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
}
