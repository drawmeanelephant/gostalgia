// Package delve is Gostalgia's roguelike, "Delve — Beneath the Brass
// Elephant": eight generated levels under the tavern from Adventure,
// monsters, loot, hunger, XP, permadeath, and a persisted score ledger.
//
// The game core lives in apps/delve/engine — a pure, stdlib-only,
// deterministic package. This file owns everything IPC: routes, action
// dispatch, fs/save persistence to the app-private partition, and the
// continue-or-new ceremony around a live save. view.go composes the
// presentation snapshot; sound.go maps event kinds to sound/play blips.
//
// The seams that keep the shell testable without the engine: newGame and
// reviveGame are package vars, and gameEngine is a narrow interface over
// engine.Game, so tests substitute a fake.
package delve

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gostalgia/apps/delve/engine"
	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Delve.
const ID = "com.gostalgia.delve"

// statePath is the persisted run inside the app-private partition; the
// score ledger lives beside it. fs/save stages and renames atomically, so
// a crash mid-write leaves the previous document intact.
const (
	statePath  = "/apps/data/com.gostalgia.delve/save.json"
	scoresPath = "/apps/data/com.gostalgia.delve/scores.json"
)

const stateVersion = 1

// Bounds: the log region keeps a short tail, the ledger is a top-10
// table, and a delver's name is a short engraving, not a biography.
const (
	logTailLines = 40
	maxScores    = 10
	maxNameLen   = 24
	maxItemIDLen = 16
)

// delveTitle is the chrome title; end states suffix it.
const delveTitle = "Delve — Beneath the Brass Elephant"

// moveAction is the shared action every movement-pad cell declares;
// cell_id carries which compass step (or wait) was pressed.
const moveAction = "move"

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

// diskState is the persisted run document: the engine's Marshal bytes
// (which carry the RNG state, so a resumed run stays deterministic) plus
// the bookkeeping the shell owns — who delved, when, on which seed.
type diskState struct {
	Version    int    `json:"version"`
	Run        string `json:"run"` // base64 engine.Marshal bytes
	PlayerName string `json:"player_name,omitempty"`
	Started    int64  `json:"started,omitempty"` // unix seconds
	Seed       uint64 `json:"seed"`
}

// diskScores is the persisted ledger: a bounded top-10 of finished runs,
// sorted by descending value.
type diskScores struct {
	Version int            `json:"version"`
	Scores  []engine.Score `json:"scores"`
}

// Delve is the running in-process instance. All state is guarded by mu.
type Delve struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	pending    bool // a live save waits for continue-or-new
	g          gameEngine
	name       string // delver name, carried into new runs
	seed       uint64
	started    int64
	scored     bool // this run's ledger entry was recorded
	dirty      bool // in-memory change has not reached disk
	showHelp   bool // the region shows help instead of the log
	scores     []engine.Score
	sound      int
	statusNote string // transient feedback line; cleared on each action
	lastError  string
}

// Factory constructs a fresh, uninitialized Delve instance.
func Factory() (sdk.Instance, error) { return &Delve{now: time.Now}, nil }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (d *Delve) Init(app *sdk.Context) error {
	d.app = app
	if d.now == nil {
		d.now = time.Now
	}
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
// dungeon only moves when the player does.
func (d *Delve) Run(ctx context.Context) error {
	d.app.Log.Info("delve running")
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (d *Delve) Stop(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded && d.dirty {
		if err := d.persistLocked(ctx); err != nil && d.app.Log != nil {
			d.app.Log.Warn("delve: final save failed", "err", err)
		}
	}
	return nil
}

// ensureLoadedLocked reads and decodes save.json and scores.json once. A
// missing save means no delve has begun — the view shows the pre-run
// screen. A corrupt or wrong-version file is a fresh start with an
// honest note; the bad bytes are left alone until the next real change
// saves over them. A valid save revives its run and waits pending a
// continue-or-new choice — unless the run already ended, in which case
// its tombstone or victory screen shows directly.
func (d *Delve) ensureLoadedLocked(ctx context.Context) error {
	if d.loaded {
		return nil
	}
	d.loaded = true
	d.loadScoresLocked(ctx)

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := d.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		if looksDenied(err) {
			d.lastError = fmt.Sprintf("storage access denied (%v) — runs will not be saved", err)
		}
		return nil // no save yet: the stairs wait
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		d.statusNote = "previous save unreadable — the stairs wait for a fresh delve"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		d.statusNote = "previous save unreadable — the stairs wait for a fresh delve"
		return nil
	}
	run, err := base64.StdEncoding.DecodeString(st.Run)
	if err != nil {
		d.statusNote = "previous save unreadable — the stairs wait for a fresh delve"
		return nil
	}
	g, err := reviveGame(run)
	if err != nil {
		d.statusNote = "previous save unreadable — the stairs wait for a fresh delve"
		return nil
	}
	d.g = g
	d.name = sanitizeName(st.PlayerName)
	d.seed = st.Seed
	d.started = st.Started
	if g.Outcome() == engine.OutcomeOngoing {
		d.pending = true
	} else {
		// The run already ended; its ledger entry was recorded when it
		// did, so show the tombstone/victory without re-scoring.
		d.scored = true
	}
	return nil
}

// loadScoresLocked reads the ledger once, tolerating absence and damage:
// a missing ledger is an empty one, a corrupt one heals on the next
// recorded score. It never blocks loading the run itself.
func (d *Delve) loadScoresLocked(ctx context.Context) {
	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	if err := d.app.Call(ctx, "fs/read", map[string]string{"path": scoresPath}, &out); err != nil {
		return
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		return
	}
	var sc diskScores
	if err := json.Unmarshal(data, &sc); err != nil {
		return
	}
	d.scores = sc.Scores
	sortScores(d.scores)
	if len(d.scores) > maxScores {
		d.scores = d.scores[:maxScores]
	}
}

// looksDenied reports whether an fs error is a grant denial rather than a
// missing file. The two are intentionally different: absence is normal on
// first launch, denial means nothing the player does will persist.
func looksDenied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "denied") || strings.Contains(msg, "permission")
}

// persistLocked writes the whole save document atomically. Callers hold mu.
func (d *Delve) persistLocked(ctx context.Context) error {
	if d.g == nil {
		return nil // nothing delved yet, nothing to store
	}
	run, err := d.g.Marshal()
	if err != nil {
		return err
	}
	data, err := json.Marshal(diskState{
		Version:    stateVersion,
		Run:        base64.StdEncoding.EncodeToString(run),
		PlayerName: d.name,
		Started:    d.started,
		Seed:       d.seed,
	})
	if err != nil {
		return err
	}
	err = d.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		d.dirty = false
	}
	return err
}

// saveLocked persists and wraps a failure so the report says what failed,
// not just why the fs call did. Callers hold mu.
func (d *Delve) saveLocked(ctx context.Context) {
	if err := d.persistLocked(ctx); err != nil {
		d.statusNote = fmt.Sprintf("save failed: %v", err)
	}
}

// recordScoreLocked appends the finished run's ledger entry to
// scores.json: top-10 by value, descending. Callers hold mu.
func (d *Delve) recordScoreLocked(ctx context.Context) {
	d.scored = true
	d.scores = append(d.scores, d.g.FinalScore())
	sortScores(d.scores)
	if len(d.scores) > maxScores {
		d.scores = d.scores[:maxScores]
	}
	data, err := json.Marshal(diskScores{Version: stateVersion, Scores: d.scores})
	if err != nil {
		d.statusNote = fmt.Sprintf("ledger failed: %v", err)
		return
	}
	if err := d.app.Call(ctx, "fs/save", map[string]any{
		"path":        scoresPath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil); err != nil {
		d.statusNote = fmt.Sprintf("ledger failed: %v", err)
	}
}

// sortScores orders the ledger by descending value, stable for ties.
func sortScores(sc []engine.Score) {
	sort.SliceStable(sc, func(i, j int) bool { return sc[i].Value > sc[j].Value })
}

// commandLocked runs one engine command when a run allows it, then
// persists. Returns the turn's events for sound feedback. Callers hold mu.
func (d *Delve) commandLocked(ctx context.Context, c engine.Cmd) []engine.Event {
	switch {
	case d.g == nil:
		d.statusNote = "the delve is not yet open — begin a New delve"
		return nil
	case d.pending:
		d.statusNote = "an earlier delve waits — Continue, or begin a New delve"
		return nil
	case d.g.Outcome() != engine.OutcomeOngoing:
		d.statusNote = "this delve has ended — begin a New delve"
		return nil
	}
	events := d.g.Command(c)
	d.dirty = true
	if d.g.Outcome() != engine.OutcomeOngoing && !d.scored {
		d.recordScoreLocked(ctx)
	}
	d.saveLocked(ctx)
	return events
}

// itemCommandLocked resolves a presentation item id ("item_N") and runs
// the matching inventory command. Callers hold mu.
func (d *Delve) itemCommandLocked(ctx context.Context, kind engine.CmdKind, rawID string) []engine.Event {
	id, ok := parseItemID(rawID)
	if !ok {
		d.statusNote = "select an item from the pack first"
		return nil
	}
	return d.commandLocked(ctx, engine.Cmd{Kind: kind, ItemID: id})
}

// newRunLocked abandons whatever run exists and starts a fresh delve on a
// fresh seed, carrying a name from the field when the player typed one.
// Callers hold mu.
func (d *Delve) newRunLocked(ctx context.Context, rawName string) {
	if n := sanitizeName(rawName); n != "" {
		d.name = n
	}
	d.seed = rand.Uint64()
	d.started = d.now().Unix()
	g, err := newGame(engine.Config{Seed: d.seed, Name: d.name})
	if err != nil {
		d.g = nil
		d.pending = false
		d.statusNote = "the delve is not yet open — the Brass Elephant's cellar stays locked"
		return
	}
	d.g = g
	d.pending = false
	d.scored = false
	d.dirty = true
	d.saveLocked(ctx)
}

// cellCmd maps a movement-pad cell id to its command: the eight compass
// directions plus the center wait.
func cellCmd(cellID string) (engine.Cmd, bool) {
	switch cellID {
	case "nw":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirNW}, true
	case "n":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirN}, true
	case "ne":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirNE}, true
	case "w":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirW}, true
	case "wait":
		return engine.Cmd{Kind: engine.CmdWait}, true
	case "e":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirE}, true
	case "sw":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirSW}, true
	case "s":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirS}, true
	case "se":
		return engine.Cmd{Kind: engine.CmdMove, Dir: engine.DirSE}, true
	}
	return engine.Cmd{}, false
}

// itemSID renders an engine inventory id as a presentation item id. The
// contract's entry pattern wants a leading letter, so the raw int gets an
// "item_" prefix rather than a bare strconv.
func itemSID(id int) string { return "item_" + strconv.Itoa(id) }

// parseItemID inverts itemSID; the reconstructed id must match exactly so
// "item_3junk" cannot reach a real entry.
func parseItemID(s string) (int, bool) {
	if len(s) > maxItemIDLen {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(s, "item_%d", &n); err != nil || itemSID(n) != s {
		return 0, false
	}
	return n, true
}

// sanitizeName normalizes a delver name: trimmed, single-line, bounded
// by runes so a multibyte character is never split.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > maxNameLen {
		s = strings.TrimSpace(string(r[:maxNameLen]))
	}
	return s
}

// --- presentation actions ---

func (d *Delve) view(ctx context.Context) (sdk.View, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: delveTitle, State: sdk.ViewError, Error: err.Error()}, nil
	}
	return d.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (d *Delve) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	d.mu.Lock()
	if err := d.ensureLoadedLocked(ctx); err != nil {
		d.mu.Unlock()
		return sdk.View{Title: delveTitle, State: sdk.ViewError, Error: err.Error()}, nil
	}
	d.lastError = ""
	d.statusNote = ""

	var events []engine.Event
	switch req.Action {
	case moveAction:
		cmd, ok := cellCmd(req.CellID)
		if !ok {
			d.mu.Unlock()
			return sdk.View{}, fmt.Errorf("unknown move cell %q", req.CellID)
		}
		events = d.commandLocked(ctx, cmd)
	case "pickup":
		events = d.commandLocked(ctx, engine.Cmd{Kind: engine.CmdPickup})
	case "descend":
		events = d.commandLocked(ctx, engine.Cmd{Kind: engine.CmdDescend})
	case "ascend":
		events = d.commandLocked(ctx, engine.Cmd{Kind: engine.CmdAscend})
	case "rest":
		events = d.commandLocked(ctx, engine.Cmd{Kind: engine.CmdRest})
	case "use":
		events = d.itemCommandLocked(ctx, engine.CmdUseItem, req.ItemID)
	case "equip":
		events = d.itemCommandLocked(ctx, engine.CmdEquip, req.ItemID)
	case "drop":
		events = d.itemCommandLocked(ctx, engine.CmdDrop, req.ItemID)
	case "new":
		d.newRunLocked(ctx, req.Values["name"])
	case "continue":
		if !d.pending {
			d.mu.Unlock()
			return sdk.View{}, fmt.Errorf("no delve to continue")
		}
		d.pending = false
		d.statusNote = "you pick up the descent where you left off"
	case "help":
		d.showHelp = !d.showHelp
	default:
		d.mu.Unlock()
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	v := d.viewLocked(sdk.PresentationRequestVersion(ctx))
	d.mu.Unlock()
	d.playSounds(ctx, events)
	return v, nil
}

// --- programmatic IPC routes ---

// stateRoute returns the outcome and a compact score/stat summary.
func (d *Delve) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	out := map[string]any{
		"run":     d.g != nil,
		"pending": d.pending,
		"name":    d.name,
		"scores":  d.scores,
	}
	if d.g != nil {
		st := d.g.Stats()
		out["outcome"] = int(d.g.Outcome())
		out["depth"] = st.Depth
		out["hp"] = st.HP
		out["max_hp"] = st.MaxHP
		out["level"] = st.Level
		out["gold"] = st.Gold
		out["turns"] = st.Turns
		out["kills"] = st.Kills
		out["score"] = d.g.FinalScore()
	} else {
		out["outcome"] = int(engine.OutcomeOngoing)
	}
	return out, nil
}

// commandRoute runs one engine command programmatically — the headless
// surface parallel to the presentation actions. Params name the action
// vocabulary: {"kind":"move","dir":"se"}, {"kind":"use","item_id":3}, and
// the rest. A saved run waiting pending resumes on the first command.
func (d *Delve) commandRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Kind   string `json:"kind"`
		Dir    string `json:"dir"`
		ItemID int    `json:"item_id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	cmd, err := routeCmd(params.Kind, params.Dir, params.ItemID)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	if err := d.ensureLoadedLocked(ctx); err != nil {
		d.mu.Unlock()
		return nil, err
	}
	d.lastError = ""
	d.statusNote = ""
	if d.g == nil {
		d.mu.Unlock()
		return nil, fmt.Errorf("no delve in progress — call restart to begin one")
	}
	d.pending = false // an explicit command chooses to continue the save
	events := d.commandLocked(ctx, cmd)
	outcome := int(d.g.Outcome())
	depth := d.g.Stats().Depth
	d.mu.Unlock()
	d.playSounds(ctx, events)
	return map[string]any{
		"events":  events,
		"outcome": outcome,
		"depth":   depth,
	}, nil
}

// routeCmd validates route params into an engine command. Unknown kinds
// and bad directions are honest IPC errors — there is no player standing
// in the fiction to be confused by them.
func routeCmd(kind, dir string, itemID int) (engine.Cmd, error) {
	switch kind {
	case "move":
		d, ok := routeDir(dir)
		if !ok {
			return engine.Cmd{}, fmt.Errorf("dir must be one of n,ne,e,se,s,sw,w,nw")
		}
		return engine.Cmd{Kind: engine.CmdMove, Dir: d}, nil
	case "wait":
		return engine.Cmd{Kind: engine.CmdWait}, nil
	case "rest":
		return engine.Cmd{Kind: engine.CmdRest}, nil
	case "pickup":
		return engine.Cmd{Kind: engine.CmdPickup}, nil
	case "descend":
		return engine.Cmd{Kind: engine.CmdDescend}, nil
	case "ascend":
		return engine.Cmd{Kind: engine.CmdAscend}, nil
	case "use":
		return engine.Cmd{Kind: engine.CmdUseItem, ItemID: itemID}, nil
	case "equip":
		return engine.Cmd{Kind: engine.CmdEquip, ItemID: itemID}, nil
	case "drop":
		return engine.Cmd{Kind: engine.CmdDrop, ItemID: itemID}, nil
	}
	return engine.Cmd{}, fmt.Errorf("unknown kind %q — want move, wait, rest, pickup, descend, ascend, use, equip, or drop", kind)
}

// routeDir maps a compass name to its direction.
func routeDir(s string) (engine.Dir, bool) {
	switch s {
	case "n":
		return engine.DirN, true
	case "ne":
		return engine.DirNE, true
	case "e":
		return engine.DirE, true
	case "se":
		return engine.DirSE, true
	case "s":
		return engine.DirS, true
	case "sw":
		return engine.DirSW, true
	case "w":
		return engine.DirW, true
	case "nw":
		return engine.DirNW, true
	}
	return 0, false
}

// restartRoute abandons any current run and begins a fresh delve on a
// fresh seed. An optional name rides into engine.Config.Name.
func (d *Delve) restartRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	d.lastError = ""
	d.statusNote = ""
	d.newRunLocked(ctx, params.Name)
	if d.g == nil {
		return nil, fmt.Errorf("the delve is not yet open — engine unavailable")
	}
	return map[string]any{"restarted": true, "seed": d.seed}, nil
}
