// Package musictoy is Gostalgia's chiptune step sequencer: a 16-step,
// 4-row pattern grid where each row is a pitched voice. Toggling cells
// composes a loop; Play renders one pass of the pattern into a bounded
// sound/play note sequence — steps with several rows set arpeggiate them
// inside the step's timeslice, the classic chip chord trick.
//
// The version-2 snapshot renders the pattern as a real 16-column grid
// (64 cells, the pad maximum). Each cell carries its row's voice identity
// as text — l/L lead, h/H high, o/O low, b/B bass, upper case meaning a set
// step — so the four voices stay distinguishable without color, and the
// grid label spells the voices and their current scale pitches. Version-1
// requests get the four rows as items plus a compact row-edit field. The
// working pattern and a bounded library of named patterns persist in
// /apps/data/com.gostalgia.musictoy/patterns.json via atomic fs/save.
//
// Playback is honest about its bounds: sound/play caps a clip at 64 notes
// and 10 seconds, and one pass here can never exceed 64 notes or (at the
// 60bpm tempo floor) 4 seconds, so a press fires exactly one bounded pass
// and says so. The host player is fire-and-forget with no stop primitive,
// which is why there is a Play action and no Stop — the clip always ends
// on its own within a few seconds. A successful sound/play acknowledgement
// starts a non-persisted static "Playing" deadline for exactly the
// acknowledged duration; composition and unrelated messages leave it
// alone, and a failed attempt never starts or extends one. On hosts without
// audio, or without the sound grant, composing and saving keep working and
// the view states plainly that playback is unavailable.
package musictoy

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Musictoy.
const ID = "com.gostalgia.musictoy"

// statePath is the persisted document inside the app-private partition:
// the working pattern plus the named-pattern library. fs/save stages and
// renames atomically, so a crash mid-write leaves the previous document
// intact.
const statePath = "/apps/data/com.gostalgia.musictoy/patterns.json"

const stateVersion = 1

// Library bounds keep the document small and the item list scannable.
const (
	maxSavedPatterns = 16
	maxPatternName   = 32
)

// stepAction is the shared action every grid cell declares; cell_id carries
// which step was pressed.
const stepAction = "step"

// soundCapability, soundRoute, and soundStatusRoute name the audio grant
// and the routes served by the host audio adapter. The manifest declares
// the grant, but the grant alone is not proof the host can render audio:
// the app probes session/whoami and sound/status once per launch and
// latches the result, so a Play press on a silent host never spends the
// doomed sound/play call. soundWhy keeps the reason for the status line.
const (
	soundCapability  = sdk.CapSound
	soundRoute       = "sound/play"
	soundStatusRoute = "sound/status"
)

// Tri-state for the sound probe: unknown until checked once, then latched.
const (
	soundUnknown = iota
	soundOff
	soundOn
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid compiled-in data is a programming error
	}
	return m
}

// ManifestJSON returns a copy of the raw embedded JSON.
func ManifestJSON() []byte { return append([]byte(nil), manifestJSON...) }

// savedPattern is one named library entry: a full snapshot of pattern,
// tempo, and scale.
type savedPattern struct {
	ID      string          `json:"id"`
	Name    string          `json:"name"`
	BPM     int             `json:"bpm"`
	Scale   string          `json:"scale"`
	Rows    [numRows]string `json:"rows"`
	SavedAt time.Time       `json:"saved_at"`
}

// diskState is the versioned persisted document: the working pattern and
// settings plus the library and the id counter that keeps future ids
// unique.
type diskState struct {
	Version  int             `json:"version"`
	BPM      int             `json:"bpm"`
	Scale    string          `json:"scale"`
	Rows     [numRows]string `json:"rows"`
	Patterns []savedPattern  `json:"patterns"`
	Seq      int             `json:"seq"`
}

// Toy is the running in-process instance. All state is guarded by mu.
type Toy struct {
	app *sdk.Context
	now func() time.Time

	mu         sync.Mutex
	loaded     bool
	pattern    Pattern
	bpm        int
	scaleName  string
	saved      []savedPattern
	seq        int
	dirty      bool // in-memory change has not reached disk
	sound      int
	soundWhy   string // why playback is unavailable, for the status line
	player     string // host player name reported by sound/status
	statusNote string // transient feedback line; cleared on each action
	lastError  string

	// playingUntil and playingFor describe the host-acknowledged pass, not
	// the pattern: pattern edits and tempo changes never touch them. Zero
	// playingUntil means no acknowledged pass; they are never persisted.
	playingUntil time.Time
	playingFor   time.Duration
}

// Factory constructs a fresh, uninitialized Toy instance.
func Factory() (sdk.Instance, error) {
	return &Toy{now: time.Now, bpm: defaultBPM, scaleName: scales[0].name}, nil
}

// setClock injects a deterministic clock for tests.
func setClock(t *Toy, now func() time.Time) { t.now = now }

// Init registers the presentation surface and the headless routes. State
// loads lazily on first use so Init stays prompt.
func (t *Toy) Init(app *sdk.Context) error {
	t.app = app
	if t.now == nil {
		t.now = time.Now
	}
	if t.bpm == 0 {
		t.bpm = defaultBPM
	}
	if t.scaleName == "" {
		t.scaleName = scales[0].name
	}
	for _, route := range []struct {
		name string
		h    sdk.Handler
	}{
		{"state", t.stateRoute},
		{"toggle", t.toggleRoute},
		{"set", t.setRoute},
		{"play", t.playRoute},
		{"save", t.saveRoute},
		{"load", t.loadRoute},
		{"delete", t.deleteRoute},
		{"list", t.listRoute},
	} {
		if err := app.Handle(route.name, route.h); err != nil {
			return err
		}
	}
	return app.Present(t.view, t.act)
}

// Run blocks until the instance is canceled. There is no playback cursor:
// each Play press composes one bounded clip and ships it whole.
func (t *Toy) Run(ctx context.Context) error {
	t.app.Log.Info("musictoy running")
	<-ctx.Done()
	return nil
}

// Stop flushes a pending save — the retry after an earlier save failure.
// A failed flush is logged, never fatal.
func (t *Toy) Stop(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.loaded && t.dirty {
		if err := t.persistLocked(ctx); err != nil && t.app.Log != nil {
			t.app.Log.Warn("musictoy: final save failed", "err", err)
		}
	}
	return nil
}

// ensureLoadedLocked reads and decodes patterns.json once. A missing file
// is simply a fresh blank pattern. A corrupt or wrong-version file is an
// honest error banner over a usable fresh state; the bad bytes are left
// alone until the next real change saves over them. A denied read is also
// reported, since nothing will persist without the grant.
func (t *Toy) ensureLoadedLocked(ctx context.Context) error {
	if t.loaded {
		return nil
	}
	t.loaded = true

	var out struct {
		DataBase64 string `json:"data_base64"`
	}
	err := t.app.Call(ctx, "fs/read", map[string]string{"path": statePath}, &out)
	if err != nil {
		if looksDenied(err) {
			t.lastError = fmt.Sprintf("storage access denied (%v) — patterns will not be saved", err)
		}
		return nil // no save yet: fresh pattern
	}
	data, err := base64.StdEncoding.DecodeString(out.DataBase64)
	if err != nil {
		t.lastError = "saved patterns are unreadable — starting fresh; the next change saves over them"
		return nil
	}
	var st diskState
	if err := json.Unmarshal(data, &st); err != nil || st.Version != stateVersion {
		t.lastError = "saved patterns are unreadable — starting fresh; the next change saves over them"
		return nil
	}
	t.bpm = st.BPM
	t.scaleName = st.Scale
	var healed bool
	t.pattern, healed = decode(st.Rows)
	t.saved = st.Patterns
	t.seq = st.Seq
	if t.sanitizeLocked() || healed {
		// The loaded file needed repair; write the healed copy back now so
		// a crash does not reload the same damage.
		if err := t.persistLocked(ctx); err != nil {
			t.lastError = fmt.Sprintf("save failed: %v", err)
		}
	}
	return nil
}

// looksDenied reports whether an fs error is a grant denial rather than a
// missing file.
func looksDenied(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "denied") || strings.Contains(msg, "permission")
}

// sanitizeLocked repairs loaded state in place and reports whether it
// changed anything: tempo and scale clamped to known values, malformed row
// strings rebuilt, seq pushed past every pat_N id, duplicate or reserved
// ids reassigned, names normalized, the library bounded, and timestamps
// pinned.
func (t *Toy) sanitizeLocked() bool {
	changed := false
	if t.bpm < minBPM || t.bpm > maxBPM {
		t.bpm = defaultBPM
		changed = true
	}
	if _, ok := findScale(t.scaleName); !ok {
		t.scaleName = scales[0].name
		changed = true
	}
	for _, sp := range t.saved {
		var n int
		if _, err := fmt.Sscanf(sp.ID, "pat_%d", &n); err == nil && n > t.seq {
			t.seq = n
		}
	}
	if len(t.saved) > maxSavedPatterns {
		t.saved = t.saved[:maxSavedPatterns]
		changed = true
	}
	now := t.currentTime()
	seen := map[string]bool{"lib_empty": true}
	for r := 0; r < numRows; r++ {
		seen[fmt.Sprintf("row_%d", r)] = true
	}
	kept := t.saved[:0]
	for _, sp := range t.saved {
		name := sanitizeName(sp.Name)
		if name == "" {
			name = "(untitled pattern)"
		}
		if name != sp.Name {
			changed = true
		}
		sp.Name = name
		if sp.ID == "" || seen[sp.ID] {
			t.seq++
			sp.ID = fmt.Sprintf("pat_%d", t.seq)
			changed = true
		}
		seen[sp.ID] = true
		if _, ok := findScale(sp.Scale); !ok {
			sp.Scale = scales[0].name
			changed = true
		}
		if sp.BPM < minBPM || sp.BPM > maxBPM {
			sp.BPM = defaultBPM
			changed = true
		}
		p, healed := decode(sp.Rows)
		if healed {
			sp.Rows = p.encode()
			changed = true
		}
		if sp.SavedAt.IsZero() || sp.SavedAt.After(now) {
			sp.SavedAt = now
			changed = true
		}
		kept = append(kept, sp)
	}
	t.saved = kept
	return changed
}

// persistLocked writes the whole document atomically. Callers hold mu.
func (t *Toy) persistLocked(ctx context.Context) error {
	saved := t.saved
	if saved == nil {
		saved = []savedPattern{}
	}
	data, err := json.Marshal(diskState{
		Version:  stateVersion,
		BPM:      t.bpm,
		Scale:    t.scaleName,
		Rows:     t.pattern.encode(),
		Patterns: saved,
		Seq:      t.seq,
	})
	if err != nil {
		return err
	}
	err = t.app.Call(ctx, "fs/save", map[string]any{
		"path":        statePath,
		"data_base64": base64.StdEncoding.EncodeToString(data),
	}, nil)
	if err == nil {
		t.dirty = false
	}
	return err
}

// saveErrorLocked persists and wraps a failure so the report says what
// failed, not just why the fs call did. Callers hold mu.
func (t *Toy) saveErrorLocked(ctx context.Context) error {
	if err := t.persistLocked(ctx); err != nil {
		return fmt.Errorf("save failed: %w", err)
	}
	return nil
}

// scaleLocked resolves the current scale table.
func (t *Toy) scaleLocked() scaleDef {
	s, ok := findScale(t.scaleName)
	if !ok {
		return scales[0]
	}
	return s
}

// probeLocked resolves the audio story once per launch: does this
// instance's grant carry the sound capability, and does the host audio
// adapter report itself supported? The result latches — a later grant or
// adapter is picked up on the next launch, never mid-composition.
func (t *Toy) probeLocked(ctx context.Context) {
	if t.sound != soundUnknown {
		return
	}
	t.sound = soundOff
	var who struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := t.app.Call(ctx, "session/whoami", nil, &who); err != nil {
		t.soundWhy = "couldn't check the audio grant"
		return
	}
	granted := false
	for _, cap := range who.Capabilities {
		if cap == soundCapability {
			granted = true
		}
	}
	if !granted {
		t.soundWhy = "the sound permission isn't granted"
		return
	}
	var st struct {
		Supported bool   `json:"supported"`
		Player    string `json:"player"`
	}
	if err := t.app.Call(ctx, soundStatusRoute, nil, &st); err != nil {
		t.soundWhy = "the audio service isn't answering"
		return
	}
	if !st.Supported {
		t.soundWhy = "this host has no audio player"
		return
	}
	t.sound = soundOn
	t.soundWhy = ""
	t.player = st.Player
}

// --- mutations: error-returning, shared by presentation and routes ---

// toggleStepLocked flips one cell by its grid id ("r0s3" .. "r3s15"). The
// reconstructed id must match the request exactly so "r0s4junk" cannot
// reach a real cell.
func (t *Toy) toggleStepLocked(ctx context.Context, cellID string) error {
	var r, s int
	n, err := fmt.Sscanf(cellID, "r%ds%d", &r, &s)
	if n != 2 || err != nil || r < 0 || r >= numRows || s < 0 || s >= numSteps ||
		fmt.Sprintf("r%ds%d", r, s) != cellID {
		return fmt.Errorf("no step %q on the grid", cellID)
	}
	on := t.pattern.Toggle(r, s)
	if on {
		t.statusNote = fmt.Sprintf("%s step %d on", rowNames[r], s+1)
	} else {
		t.statusNote = fmt.Sprintf("%s step %d off", rowNames[r], s+1)
	}
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// playLocked renders one pass of the pattern and ships it as a single
// sound/play call. Every failure — no grant, unsupported host, a saturated
// player, a vanished adapter — lands in the status line, never as an app
// error, and busy is reported once rather than retried.
func (t *Toy) playLocked(ctx context.Context) error {
	if t.sound != soundOn {
		return fmt.Errorf("playback unavailable — %s; composing and saving still work", t.soundWhy)
	}
	notes := t.pattern.synthesize(t.bpm, t.scaleLocked())
	if err := clipWithinSoundBounds(notes); err != nil {
		return fmt.Errorf("this pattern can't play honestly: %v", err)
	}
	var out struct {
		Played     bool `json:"played"`
		DurationMs int  `json:"duration_ms"`
	}
	err := t.app.Call(ctx, soundRoute, map[string]any{"notes": notes}, &out)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "playback busy"):
			return fmt.Errorf("audio busy — too many clips still playing; try again when one ends")
		case strings.Contains(err.Error(), "unsupported"):
			// The adapter vanished between probe and play: relatch so the
			// next view renders Play disabled instead of spending calls.
			t.sound = soundOff
			t.soundWhy = "this host has no audio player"
			return fmt.Errorf("playback unavailable — this host has no audio player")
		default:
			return fmt.Errorf("play failed: %v", err)
		}
	}
	if !out.Played {
		// A reply without the acknowledgement is not evidence that audio
		// started, so it never claims Playing.
		return fmt.Errorf("playback was not acknowledged — nothing was scheduled")
	}
	// The acknowledged duration owns the indicator. A missing duration
	// falls back to the pass this request described; either way a failed
	// attempt above leaves an existing pass's deadline untouched.
	duration := time.Duration(out.DurationMs) * time.Millisecond
	if duration <= 0 {
		duration = time.Duration(clipDurationMs(notes)) * time.Millisecond
	}
	t.playingUntil = t.currentTime().Add(duration)
	t.playingFor = duration

	seconds := duration.Seconds()
	if t.pattern.Active() == 0 {
		t.statusNote = fmt.Sprintf("%d rests (the pattern is empty)", len(notes))
	} else {
		t.statusNote = fmt.Sprintf("%d notes over %.1fs", len(notes), seconds)
	}
	return nil
}

// tempoLocked validates and applies a tempo request.
func (t *Toy) tempoLocked(ctx context.Context, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("enter a tempo first")
	}
	bpm, err := strconv.Atoi(raw)
	if err != nil || bpm < minBPM || bpm > maxBPM {
		return fmt.Errorf("tempo must be a number %d-%d bpm", minBPM, maxBPM)
	}
	t.bpm = bpm
	t.statusNote = fmt.Sprintf("tempo: %d bpm (one pass lasts %.1fs)", bpm, float64(numSteps*stepMs(bpm))/1000)
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// cycleScaleLocked advances to the next scale table.
func (t *Toy) cycleScaleLocked(ctx context.Context) error {
	next := 0
	for i, s := range scales {
		if s.name == t.scaleName {
			next = (i + 1) % len(scales)
		}
	}
	t.scaleName = scales[next].name
	t.statusNote = fmt.Sprintf("scale: %s", scales[next].label)
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// setScaleLocked validates and applies a scale by name.
func (t *Toy) setScaleLocked(ctx context.Context, name string) error {
	s, ok := findScale(strings.ToLower(strings.TrimSpace(name)))
	if !ok {
		names := make([]string, 0, len(scales))
		for _, sc := range scales {
			names = append(names, sc.name)
		}
		return fmt.Errorf("unknown scale %q (try %s)", name, strings.Join(names, ", "))
	}
	t.scaleName = s.name
	t.statusNote = fmt.Sprintf("scale: %s", s.label)
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// clearLocked blanks the working pattern.
func (t *Toy) clearLocked(ctx context.Context) error {
	t.pattern.Clear()
	t.statusNote = "pattern cleared"
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// applyRowLocked is the version-1 pattern editor: the row_edit field
// carries "<row> <16 glyphs>" ("lead x.x..x..") or "<row> clear".
func (t *Toy) applyRowLocked(ctx context.Context, raw string) error {
	fields := strings.Fields(raw)
	if len(fields) != 2 {
		return fmt.Errorf("row edit needs a row and 16 steps, e.g. \"lead x.x..x..x....x\"")
	}
	r, ok := findRow(fields[0])
	if !ok {
		return fmt.Errorf("no row %q (rows are %s)", fields[0], strings.Join(rowNames[:], ", "))
	}
	if fields[1] == "clear" || fields[1] == "off" {
		for s := 0; s < numSteps; s++ {
			t.pattern.cells[r][s] = false
		}
	} else {
		cells, _, err := parseRow(fields[1], true)
		if err != nil {
			return fmt.Errorf("%s row: %v", rowNames[r], err)
		}
		t.pattern.cells[r] = cells
	}
	t.statusNote = fmt.Sprintf("%s row is now %s", rowNames[r], t.pattern.rowString(r))
	t.dirty = true
	return t.saveErrorLocked(ctx)
}

// savePatternLocked stores the working pattern under a name, replacing a
// same-named entry rather than duplicating it.
func (t *Toy) savePatternLocked(ctx context.Context, name string) (savedPattern, bool, error) {
	name = sanitizeName(name)
	if name == "" {
		return savedPattern{}, false, fmt.Errorf("name the pattern first")
	}
	sp := savedPattern{
		Name:    name,
		BPM:     t.bpm,
		Scale:   t.scaleName,
		Rows:    t.pattern.encode(),
		SavedAt: t.currentTime(),
	}
	for i := range t.saved {
		if strings.EqualFold(t.saved[i].Name, name) {
			sp.ID = t.saved[i].ID
			t.saved[i] = sp
			t.statusNote = fmt.Sprintf("updated pattern %q", name)
			t.dirty = true
			return sp, true, t.saveErrorLocked(ctx)
		}
	}
	if len(t.saved) >= maxSavedPatterns {
		return savedPattern{}, false, fmt.Errorf("the pattern library is full (%d) — delete one first", maxSavedPatterns)
	}
	t.seq++
	sp.ID = fmt.Sprintf("pat_%d", t.seq)
	t.saved = append(t.saved, sp)
	t.statusNote = fmt.Sprintf("saved pattern %q", name)
	t.dirty = true
	return sp, false, t.saveErrorLocked(ctx)
}

// loadPatternLocked copies a library entry into the working pattern.
func (t *Toy) loadPatternLocked(ctx context.Context, itemID string) error {
	for i := range t.saved {
		if t.saved[i].ID == itemID {
			sp := t.saved[i]
			t.pattern, _ = decode(sp.Rows)
			t.bpm = sp.BPM
			t.scaleName = sp.Scale
			t.statusNote = fmt.Sprintf("loaded %q — %d bpm, %s", sp.Name, sp.BPM, sp.Scale)
			t.dirty = true
			return t.saveErrorLocked(ctx)
		}
	}
	return fmt.Errorf("select a saved pattern to load")
}

// deletePatternLocked removes a library entry.
func (t *Toy) deletePatternLocked(ctx context.Context, itemID string) error {
	for i := range t.saved {
		if t.saved[i].ID == itemID {
			name := t.saved[i].Name
			t.saved = append(t.saved[:i], t.saved[i+1:]...)
			t.statusNote = fmt.Sprintf("deleted %q", name)
			t.dirty = true
			return t.saveErrorLocked(ctx)
		}
	}
	return fmt.Errorf("select a saved pattern to delete")
}

// --- presentation ---

func (t *Toy) view(ctx context.Context) (sdk.View, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Musictoy", State: sdk.ViewError, Error: err.Error()}, nil
	}
	t.probeLocked(ctx)
	return t.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

func (t *Toy) act(ctx context.Context, req sdk.ActionRequest) (sdk.View, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return sdk.View{Title: "Musictoy", State: sdk.ViewError, Error: err.Error()}, nil
	}
	t.probeLocked(ctx)
	t.lastError = ""
	t.statusNote = ""

	var err error
	switch req.Action {
	case stepAction:
		err = t.toggleStepLocked(ctx, req.CellID)
	case "play":
		err = t.playLocked(ctx)
	case "tempo":
		err = t.tempoLocked(ctx, req.Values["tempo_bpm"])
	case "scale":
		err = t.cycleScaleLocked(ctx)
	case "clear":
		err = t.clearLocked(ctx)
	case "save":
		_, _, err = t.savePatternLocked(ctx, req.Values["save_name"])
	case "load":
		err = t.loadPatternLocked(ctx, req.ItemID)
	case "delete":
		err = t.deletePatternLocked(ctx, req.ItemID)
	case "apply_row":
		err = t.applyRowLocked(ctx, req.Values["row_edit"])
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", req.Action)
	}
	if err != nil {
		// Domain rejections land in the status line, not the error banner:
		// the pattern is still fully editable.
		t.statusNote = err.Error()
	}
	return t.viewLocked(sdk.PresentationRequestVersion(ctx)), nil
}

// viewLocked builds the snapshot at the negotiated presentation version.
func (t *Toy) viewLocked(version int) sdk.View {
	scale := t.scaleLocked()

	items := make([]sdk.Item, 0, numRows+len(t.saved)+1)
	if version < 2 {
		// Version-1 requests get no grid: the four rows render as items,
		// each carrying its step encoding, wave, and pitch.
		for r := 0; r < numRows; r++ {
			items = append(items, sdk.Item{
				ID:    fmt.Sprintf("row_%d", r),
				Label: fmt.Sprintf("%s  %s", rowNames[r], t.pattern.rowString(r)),
				Detail: fmt.Sprintf("%s · %.0f Hz",
					rowWaves[r], scale.rowFreqHz(r)),
			})
		}
	}
	for _, sp := range t.saved {
		pat, _ := decode(sp.Rows)
		items = append(items, sdk.Item{
			ID:    sp.ID,
			Label: sp.Name,
			Detail: fmt.Sprintf("%d bpm · %s · %d cells · saved %s",
				sp.BPM, sp.Scale, pat.Active(), ago(t.currentTime().Sub(sp.SavedAt))),
		})
	}
	if len(t.saved) == 0 {
		items = append(items, sdk.Item{
			ID:     "lib_empty",
			Label:  "No saved patterns",
			Detail: "name the working pattern below and press Save",
		})
	}

	fields := []sdk.Field{
		{ID: "tempo_bpm", Label: fmt.Sprintf("Tempo (%d-%d bpm)", minBPM, maxBPM), Value: strconv.Itoa(t.bpm)},
		{ID: "save_name", Label: "Pattern name"},
	}
	actions := []sdk.Action{
		{ID: "play", Label: "Play", Disabled: t.sound != soundOn},
		{ID: "tempo", Label: "Set tempo"},
		{ID: "scale", Label: "Scale: " + t.scaleName},
		{ID: "clear", Label: "Clear"},
		{ID: "save", Label: "Save"},
		{ID: "load", Label: "Load", Disabled: len(t.saved) == 0},
		{ID: "delete", Label: "Delete", Disabled: len(t.saved) == 0},
	}

	v := sdk.View{
		Title:   "Musictoy",
		State:   sdk.ViewReady,
		Items:   items,
		Fields:  fields,
		Actions: actions,
		Status:  t.statusLocked(scale),
		Error:   t.lastError,
	}
	if version >= 2 {
		v.Grid = t.gridLocked()
	} else {
		v.Fields = append(v.Fields, sdk.Field{
			ID:    "row_edit",
			Label: fmt.Sprintf("Row edit (%s + 16 x/. steps, or clear)", rowNames[0]),
		})
		v.Actions = append(v.Actions, sdk.Action{ID: "apply_row", Label: "Apply row"})
	}
	return v
}

// gridLocked builds the 16x4 step pad: every cell declares the shared step
// action, carries its coordinate as cell id, and shows its row voice as a
// letter — lower case for a rest, upper case for a set step. The label is
// the voice/pitch legend derived from the current scale.
func (t *Toy) gridLocked() *sdk.Grid {
	cells := make([]sdk.Cell, 0, numRows*numSteps)
	for r := 0; r < numRows; r++ {
		for s := 0; s < numSteps; s++ {
			on := 0
			if t.pattern.cells[r][s] {
				on = 1
			}
			cells = append(cells, sdk.Cell{
				ID:     fmt.Sprintf("r%ds%d", r, s),
				Label:  voiceCells[r][on],
				Action: stepAction,
			})
		}
	}
	return &sdk.Grid{Label: voiceLegend(t.scaleLocked()), Columns: numSteps, Cells: cells}
}

// playingLabelLocked reports the static acknowledged-pass indicator while the
// clock is still before the pass deadline. It never advances or animates on
// its own, and the deadline is set only by a successful acknowledgement.
func (t *Toy) playingLabelLocked() string {
	if t.playingUntil.IsZero() || !t.currentTime().Before(t.playingUntil) {
		return ""
	}
	return fmt.Sprintf("Playing one pass (%.1fs)", t.playingFor.Seconds())
}

// statusLocked picks the line under the grid. A durable acknowledged-pass
// indicator leads it when one is running; transient notes win over the
// baseline playback story, but neither clears nor retimes the indicator.
func (t *Toy) statusLocked(scale scaleDef) string {
	base := t.statusNote
	if base == "" {
		if t.sound != soundOn {
			base = fmt.Sprintf("playback unavailable — %s; composing and saving still work", t.soundWhy)
		} else {
			via := ""
			if t.player != "" {
				via = " · via " + t.player
			}
			base = fmt.Sprintf("%d cells · %d bpm · %s · play fires one %.1fs pass%s",
				t.pattern.Active(), t.bpm, scale.name, float64(numSteps*stepMs(t.bpm))/1000, via)
		}
	}
	if playing := t.playingLabelLocked(); playing != "" {
		if base == "" {
			return playing
		}
		return playing + " · " + base
	}
	return base
}

// now reads the injectable clock.
func (t *Toy) currentTime() time.Time {
	if t.now == nil {
		return time.Now()
	}
	return t.now()
}

// ago renders a duration compactly: "just now", "34m ago", "6h ago", "2d ago".
func ago(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// sanitizeName normalizes a pattern name: trimmed, single-line, bounded.
func sanitizeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	if len(s) > maxPatternName {
		s = strings.TrimSpace(s[:maxPatternName])
	}
	return s
}

// --- programmatic IPC routes ---

func (t *Toy) stateRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	t.probeLocked(ctx)
	scale := t.scaleLocked()
	sound := map[string]any{"state": "off", "reason": t.soundWhy}
	if t.sound == soundOn {
		sound = map[string]any{"state": "on", "player": t.player}
	}
	return map[string]any{
		"bpm":        t.bpm,
		"scale":      scale.name,
		"rows":       t.pattern.encode(),
		"active":     t.pattern.Active(),
		"step_ms":    stepMs(t.bpm),
		"patterns":   t.saved,
		"sound":      sound,
		"state_path": statePath,
	}, nil
}

func (t *Toy) toggleRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Row  int `json:"row"`
		Step int `json:"step"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.Row < 0 || params.Row >= numRows || params.Step < 0 || params.Step >= numSteps {
		return nil, fmt.Errorf("row must be 0-%d and step 0-%d", numRows-1, numSteps-1)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	on := t.pattern.Toggle(params.Row, params.Step)
	t.dirty = true
	if err := t.saveErrorLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"row": params.Row, "step": params.Step, "on": on}, nil
}

func (t *Toy) setRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		BPM   *int   `json:"bpm"`
		Scale string `json:"scale"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if params.BPM != nil {
		if err := t.tempoLocked(ctx, strconv.Itoa(*params.BPM)); err != nil {
			return nil, err
		}
	}
	if params.Scale != "" {
		if err := t.setScaleLocked(ctx, params.Scale); err != nil {
			return nil, err
		}
	}
	return map[string]any{"bpm": t.bpm, "scale": t.scaleName}, nil
}

func (t *Toy) playRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	t.probeLocked(ctx)
	notes := t.pattern.synthesize(t.bpm, t.scaleLocked())
	if err := t.playLocked(ctx); err != nil {
		// Playback failure is an expected state, not a route error: the
		// caller gets the honest reason the same way the status line does.
		return map[string]any{"played": false, "reason": err.Error(), "notes": len(notes)}, nil
	}
	return map[string]any{
		"played":      true,
		"notes":       len(notes),
		"duration_ms": int(t.playingFor / time.Millisecond),
	}, nil
}

func (t *Toy) saveRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		Name string `json:"name"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	sp, replaced, err := t.savePatternLocked(ctx, params.Name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": sp.ID, "name": sp.Name, "saved": true, "replaced": replaced}, nil
}

func (t *Toy) loadRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := t.loadPatternLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "loaded": true}, nil
}

func (t *Toy) deleteRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := sdk.DecodeParams(raw, &params); err != nil {
		return nil, err
	}
	if params.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	if err := t.deletePatternLocked(ctx, params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"id": params.ID, "deleted": true}, nil
}

func (t *Toy) listRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ensureLoadedLocked(ctx); err != nil {
		return nil, err
	}
	return map[string]any{
		"patterns": t.saved,
		"count":    len(t.saved),
		"max":      maxSavedPatterns,
	}, nil
}
