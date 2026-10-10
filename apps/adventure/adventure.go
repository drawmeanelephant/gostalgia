// Package adventure is Gostalgia's hand-authored text adventure, "The
// Brass Elephant": a compact world of rooms, exits, and items behind a
// verb+noun parser, proving the presentation contract can carry a game
// loop. Narrative renders as view items — a bounded transcript that drops
// its oldest lines — commands arrive through the single input field, and
// look/inventory/help are also actions. Game state persists to
// /apps/data/com.gostalgia.adventure/save.json via atomic fs/save after
// every command, and a relaunch with a live save offers continue-or-new.
// Parser failures and unknown verbs produce in-fiction transcript lines,
// never IPC errors.
package adventure

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Adventure.
const ID = "com.gostalgia.adventure"

// statePath is the single persisted document inside the app-private
// partition. fs/save stages and renames atomically, so a crash mid-write
// leaves the previous save intact.
const statePath = "/apps/data/com.gostalgia.adventure/save.json"

const stateVersion = 1

// Transcript bounds. The view keeps the newest transcriptView lines as
// items (plus one notice item when older lines were dropped); the saved
// document keeps a longer tail so a resumed game still reads coherently.
const (
	transcriptView = 63
	transcriptKeep = 200
	maxLineLen     = 240
	maxCmdLen      = 160
)

// End states. endedDead ends the run with a death; endedWon with victory.
const (
	endedDead = "dead"
	endedWon  = "won"
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

// diskState is the persisted document: position, flags, item locations,
// the end state, and a bounded transcript tail.
type diskState struct {
	Version    int               `json:"version"`
	Room       string            `json:"room"`
	Inventory  []string          `json:"inventory"`
	Locations  map[string]string `json:"locations,omitempty"`
	Flags      []string          `json:"flags"`
	Turns      int               `json:"turns"`
	Ended      string            `json:"ended"`
	Transcript []string          `json:"transcript"`
}

// Adventure is the running in-process instance. All state is guarded by mu.
type Adventure struct {
	app *sdk.Context

	mu         sync.Mutex
	loaded     bool
	pending    bool // a live save waits for continue-or-new
	g          *game
	transcript []string
	dirty      bool   // in-memory change has not reached disk
	statusNote string // transient feedback line; cleared on each action
	lastError  string
}

// Factory constructs a fresh, uninitialized Adventure instance.
func Factory() (sdk.Instance, error) { return &Adventure{}, nil }

// Init registers routes and the presentation callbacks. State loads
// lazily on first use so Init stays prompt.
func (a *Adventure) Init(app *sdk.Context) error {
	if err := validateWorld(); err != nil {
		return err
	}
	a.app = app
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", a.stateRoute},
		{"command", a.commandRoute},
		{"restart", a.restartRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(a.view, a.act)
}

// Run blocks until the instance is canceled. There is no live tick: the
// world changes only when the player commands it.
func (a *Adventure) Run(ctx context.Context) error {
	a.app.Log.Info("adventure running")
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (a *Adventure) Stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.loaded && a.dirty {
		if err := a.persistLocked(ctx); err != nil && a.app.Log != nil {
			a.app.Log.Warn("adventure: final save failed", "err", err)
		}
	}
	return nil
}

// ensureLoadedLocked reads and decodes save.json once. A missing file is
// a fresh expedition. A corrupt or wrong-version file is a fresh
// expedition with an honest note; the bad bytes are left alone until the
// next real change saves over them. A valid save renders its transcript
// tail and waits pending a continue-or-new choice.
func (a *Adventure) ensureLoadedLocked(ctx context.Context) error {
	if a.loaded {
		return nil
	}
	a.loaded = true
	a.g = newGame()
	a.transcript = append(introLines(), a.g.lookLines()...)

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := a.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		if looksDenied(err) {
			a.lastError = fmt.Sprintf("storage access denied (%v) — progress will not be saved", err)
		}
		return nil // no save yet: fresh expedition
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		a.statusNote = "previous save unreadable — starting a new expedition"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		a.statusNote = "previous save unreadable — starting a new expedition"
		return nil
	}
	a.g.loadLocked(&st)
	a.transcript = st.Transcript
	changed := a.sanitizeLocked()
	if len(a.transcript) == 0 {
		a.transcript = append(introLines(), a.g.lookLines()...)
		changed = true
	}
	a.pending = true
	if changed {
		// The loaded file needed repair; write the healed copy back now so
		// a crash does not reload the same damage. Best-effort: in-memory
		// state is already sound, so a failure only shows up in the status.
		if err := a.persistLocked(ctx); err != nil {
			a.statusNote = fmt.Sprintf("save failed: %v", err)
		}
	}
	return nil
}

// looksDenied reports whether an fs error is a grant denial rather than a
// missing file. The two are intentionally different: absence is normal on
// first launch, denial means nothing the player does will persist.
func looksDenied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "denied") || strings.Contains(msg, "permission")
}

// loadLocked installs loaded state into the game. Caller holds mu.
func (g *game) loadLocked(st *diskState) {
	g.room = st.Room
	g.turns = st.Turns
	g.ended = st.Ended
	for _, id := range st.Inventory {
		g.inv[id] = true
		delete(g.loc, id)
	}
	for _, f := range st.Flags {
		g.flags[f] = true
	}
	for id, roomID := range st.Locations {
		if !g.inv[id] {
			g.loc[id] = roomID
		}
	}
}

// sanitizeLocked repairs loaded state in place and reports whether it
// changed anything: unknown rooms/items/end states dropped or reset,
// carried items reconciled against placed ones, and the transcript
// bounded to its stored tail.
func (a *Adventure) sanitizeLocked() bool {
	changed := false
	g := a.g
	if _, ok := rooms[g.room]; !ok {
		g.room = startRoom
		changed = true
	}
	if g.ended != "" && g.ended != endedDead && g.ended != endedWon {
		g.ended = ""
		changed = true
	}
	if g.turns < 0 {
		g.turns = 0
		changed = true
	}
	for id := range g.inv {
		if _, ok := items[id]; !ok {
			delete(g.inv, id)
			changed = true
		}
	}
	for id, roomID := range g.loc {
		_, itemOK := items[id]
		_, roomOK := rooms[roomID]
		if !itemOK || !roomOK || g.inv[id] {
			delete(g.loc, id)
			changed = true
		}
	}
	// An item that is nowhere — not carried, not in a room — returns home.
	for id := range items {
		if !g.inv[id] && g.loc[id] == "" {
			g.loc[id] = homeRoom(id)
			changed = true
		}
	}
	if len(a.transcript) > transcriptKeep {
		a.transcript = append([]string(nil), a.transcript[len(a.transcript)-transcriptKeep:]...)
		changed = true
	}
	kept := a.transcript[:0]
	for _, line := range a.transcript {
		line = sanitizeLine(line)
		if line == "" {
			changed = true
			continue
		}
		kept = append(kept, line)
	}
	a.transcript = kept
	return changed
}

// homeRoom reports the room an item starts in.
func homeRoom(id string) string {
	for roomID, r := range rooms {
		for _, it := range r.Items {
			if it == id {
				return roomID
			}
		}
	}
	return startRoom
}

// persistLocked writes the whole save document atomically. Callers hold mu.
func (a *Adventure) persistLocked(ctx context.Context) error {
	g := a.g
	inv := make([]string, 0, len(g.inv))
	for id := range g.inv {
		inv = append(inv, id)
	}
	sort.Strings(inv)
	locs := make(map[string]string, len(g.loc))
	for id, roomID := range g.loc {
		if roomID != homeRoom(id) {
			locs[id] = roomID
		}
	}
	flags := make([]string, 0, len(g.flags))
	for f := range g.flags {
		flags = append(flags, f)
	}
	sort.Strings(flags)
	data, err := json.Marshal(diskState{
		Version:    stateVersion,
		Room:       g.room,
		Inventory:  inv,
		Locations:  locs,
		Flags:      flags,
		Turns:      g.turns,
		Ended:      g.ended,
		Transcript: a.transcript,
	})
	if err != nil {
		return err
	}
	err = a.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		a.dirty = false
	}
	return err
}

// saveLocked persists and wraps a failure so the report says what failed,
// not just why the fs call did. Callers hold mu.
func (a *Adventure) saveLocked(ctx context.Context) {
	if err := a.persistLocked(ctx); err != nil {
		a.statusNote = fmt.Sprintf("save failed: %v", err)
	}
}

// say appends transcript lines, dropping the oldest past transcriptKeep.
// Callers hold mu.
func (a *Adventure) say(lines ...string) {
	for _, line := range lines {
		if line = sanitizeLine(line); line != "" {
			a.transcript = append(a.transcript, line)
		}
	}
	if len(a.transcript) > transcriptKeep {
		a.transcript = append([]string(nil), a.transcript[len(a.transcript)-transcriptKeep:]...)
	}
	a.dirty = true
}

// sanitizeLine normalizes one transcript line: single-line, bounded.
func sanitizeLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if len(s) > maxLineLen {
		s = strings.TrimSpace(s[:maxLineLen-1]) + "…"
	}
	return s
}

// sanitizeCmd normalizes the input-field text before it reaches the
// parser: single-line, trimmed, bounded.
func sanitizeCmd(s string) string {
	s = sanitizeLine(s)
	if len(s) > maxCmdLen {
		s = strings.TrimSpace(s[:maxCmdLen-1]) + "…"
	}
	return s
}

// --- presentation ---

func (a *Adventure) view(ctx context.Context) (sdk.View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Adventure", State: sdk.ViewError, Error: err.Error()}, nil
	}
	return a.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (a *Adventure) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Adventure", State: sdk.ViewError, Error: err.Error()}, nil
	}
	a.lastError = ""
	a.statusNote = ""

	switch req.Action {
	case "command":
		a.runCommandLocked(ctx, req.Values["cmd"])
	case "look":
		a.runLocked(ctx, "look")
	case "inventory":
		a.runLocked(ctx, "inventory")
	case "help":
		a.runLocked(ctx, "help")
	case "restart":
		a.restartLocked(ctx)
	case "continue":
		if !a.pending {
			return sdk.View{}, fmt.Errorf("no expedition to continue")
		}
		a.say("> continue")
		a.continueLocked(ctx)
	case "new_game":
		if !a.pending {
			return sdk.View{}, fmt.Errorf("no expedition to replace")
		}
		a.newGameLocked(ctx)
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	return a.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// runLocked echoes a command word into the transcript and appends its
// reply. Callers hold mu.
func (a *Adventure) runLocked(ctx context.Context, input string) {
	a.say("> " + input)
	if a.pending && input != "help" {
		a.say("First: continue the saved expedition, or start a new one.")
	} else {
		lines, reset := a.g.command(input)
		if reset {
			a.transcript = nil
		}
		a.say(lines...)
	}
	a.saveLocked(ctx)
}

// runCommandLocked runs the input-field text. While a save waits pending,
// typed "continue"/"new"/"restart" words make the choice directly; every
// other word gets an in-fiction nudge. Callers hold mu.
func (a *Adventure) runCommandLocked(ctx context.Context, raw string) {
	input := sanitizeCmd(raw)
	if input != "" {
		a.say("> " + input)
	}
	if input == "" {
		a.say(`(silence isn't a command — try "help")`)
		a.saveLocked(ctx)
		return
	}
	if a.pending {
		switch strings.ToLower(strings.Fields(input)[0]) {
		case "continue":
			a.continueLocked(ctx)
		case "restart", "new":
			a.newGameLocked(ctx)
		case "help":
			a.say(helpLines()...)
			a.saveLocked(ctx)
		default:
			a.say("First: continue the saved expedition, or start a new one.")
			a.saveLocked(ctx)
		}
		return
	}
	lines, reset := a.g.command(input)
	if reset {
		a.transcript = a.transcript[:0]
	}
	a.say(lines...)
	a.saveLocked(ctx)
}

// continueLocked resumes a pending save. Callers hold mu.
func (a *Adventure) continueLocked(ctx context.Context) {
	a.pending = false
	if a.g.ended != "" {
		a.say("The expedition already ended — 'restart' begins a new one.")
	} else {
		a.say("You pick up where you left off.")
		a.say(a.g.lookLines()...)
	}
	a.saveLocked(ctx)
}

// restartLocked starts the expedition over. Callers hold mu.
func (a *Adventure) restartLocked(ctx context.Context) {
	a.g.reset()
	a.transcript = nil
	a.say(append(introLines(), "(The expedition begins again.)")...)
	a.say(a.g.lookLines()...)
	a.saveLocked(ctx)
}

// newGameLocked clears the save-era transcript and starts fresh. Callers
// hold mu.
func (a *Adventure) newGameLocked(ctx context.Context) {
	a.g.reset()
	a.transcript = nil
	a.say(introLines()...)
	a.say(a.g.lookLines()...)
	a.saveLocked(ctx)
}

// viewLocked builds the snapshot: on version 3 the whole kept transcript
// lands in the scroll region; older contracts get the transcript tail as
// items, newest at the bottom. The command field and the actions that
// currently apply round it out either way.
func (a *Adventure) viewLocked(version int) sdk.View {
	var items []sdk.Item
	var region *sdk.Region
	if version >= 3 {
		region = &sdk.Region{
			ID:    "transcript",
			Label: "Transcript",
			Text:  strings.Join(a.transcript, "\n"),
		}
	} else {
		start := 0
		dropped := 0
		if len(a.transcript) > transcriptView {
			start = len(a.transcript) - transcriptView
			dropped = start
		}
		items = make([]sdk.Item, 0, len(a.transcript)-start+1)
		if dropped > 0 {
			items = append(items, sdk.Item{
				ID:     "earlier",
				Label:  fmt.Sprintf("… %d earlier lines", dropped),
				Detail: fmt.Sprintf("the transcript keeps the newest %d lines", transcriptView),
			})
		}
		for i := start; i < len(a.transcript); i++ {
			items = append(items, sdk.Item{
				ID:    fmt.Sprintf("t%03d", i-start),
				Label: a.transcript[i],
			})
		}
	}

	actions := []sdk.Action{{ID: "command", Label: "Do it"}}
	switch {
	case a.pending:
		actions = append(actions,
			sdk.Action{ID: "continue", Label: "Continue"},
			sdk.Action{ID: "new_game", Label: "New game"})
	case a.g.ended != "":
		actions = append(actions, sdk.Action{ID: "restart", Label: "Restart"})
	default:
		actions = append(actions,
			sdk.Action{ID: "look", Label: "Look"},
			sdk.Action{ID: "inventory", Label: "Inventory"},
			sdk.Action{ID: "restart", Label: "Restart"})
	}
	actions = append(actions, sdk.Action{ID: "help", Label: "Help"})

	return sdk.View{
		Title:   a.titleLocked(),
		State:   sdk.ViewReady,
		Items:   items,
		Region:  region,
		Fields:  []sdk.Field{{ID: "cmd", Label: "Command"}},
		Actions: actions,
		Status:  a.statusLocked(),
		Error:   a.lastError,
	}
}

// titleLocked puts the room — or the ending — in the chrome.
func (a *Adventure) titleLocked() string {
	switch {
	case a.pending:
		return "Adventure"
	case a.g.ended == endedDead:
		return "Adventure — you died"
	case a.g.ended == endedWon:
		return "Adventure — escaped!"
	default:
		return "Adventure — " + rooms[a.g.room].Name
	}
}

// statusLocked picks the line under the transcript: transient notes win,
// then end-state banners, then the honest where-you-are summary.
func (a *Adventure) statusLocked() string {
	if a.statusNote != "" {
		return a.statusNote
	}
	switch {
	case a.pending:
		return "an earlier expedition is in progress — Continue, or begin a New game"
	case a.g.ended == endedDead:
		return "the expedition has ended — restart to try again"
	case a.g.ended == endedWon:
		return "you escaped with the Brass Elephant"
	default:
		return fmt.Sprintf("%s · turn %d · carrying %d", rooms[a.g.room].Name, a.g.turns, len(a.g.inv))
	}
}

// --- programmatic IPC routes ---

func (a *Adventure) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	inv := make([]string, 0, len(a.g.inv))
	for id := range a.g.inv {
		inv = append(inv, id)
	}
	sort.Strings(inv)
	flags := make([]string, 0, len(a.g.flags))
	for f := range a.g.flags {
		flags = append(flags, f)
	}
	sort.Strings(flags)
	return map[string]any{
		"room":             a.g.room,
		"room_name":        rooms[a.g.room].Name,
		"inventory":        inv,
		"flags":            flags,
		"turns":            a.g.turns,
		"ended":            a.g.ended,
		"pending":          a.pending,
		"transcript_lines": len(a.transcript),
	}, nil
}

// commandRoute runs one parser command programmatically. Bad input is an
// in-fiction reply, not an IPC error; only malformed params fail the call.
func (a *Adventure) commandRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Text string `json:"text"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	a.runCommandLocked(ctx, params.Text)
	tail := a.transcript
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return map[string]any{
		"lines":   tail,
		"room":    a.g.room,
		"turns":   a.g.turns,
		"ended":   a.g.ended,
		"pending": a.pending,
	}, nil
}

func (a *Adventure) restartRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	a.pending = false
	a.restartLocked(ctx)
	return map[string]any{"restarted": true}, nil
}
