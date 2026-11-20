package musictoy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

var t0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// harness models the app-private VFS partition plus the session and sound
// routes the app probes, with an injectable clock. No test sleeps.
type harness struct {
	now            time.Time
	files          map[string][]byte
	caps           []string // capabilities session/whoami reports
	denyFS         bool     // fs/read + fs/save denied: simulates a missing grant
	soundSupported bool     // sound/status response
	player         string
	whoamiErr      error // injectable probe failures
	statusErr      error
	playErr        error
	playCalls      int
	playAck        bool       // sound/play reports played; default true
	playDurationMs int        // acknowledged duration override; 0 = computed
	lastNotes      []clipNote // payload of the most recent sound/play call
	seq            atomic.Int64

	app      *Toy
	handlers map[string]sdk.Handler
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		now:            t0,
		files:          map[string][]byte{},
		caps:           []string{"ipc", "fs.read", "fs.write", "sound"},
		soundSupported: true,
		player:         "fake",
		playAck:        true,
		handlers:       map[string]sdk.Handler{},
	}
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	app := inst.(*Toy)
	setClock(app, func() time.Time { return h.now })
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), h.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := app.Init(ctx); err != nil {
		t.Fatal(err)
	}
	h.app = app
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
	switch method {
	case "fs/read":
		if h.denyFS {
			return errors.New("permission denied: missing capability \"fs.read\"")
		}
		data, ok := h.files[p.Path]
		if !ok {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		return reply(map[string]any{"path": p.Path, "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save":
		if h.denyFS {
			return errors.New("permission denied: missing capability \"fs.write\"")
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		h.files[p.Path] = data
		return reply(map[string]any{"path": p.Path, "saved": true})
	case "session/whoami":
		if h.whoamiErr != nil {
			return h.whoamiErr
		}
		return reply(map[string]any{"capabilities": h.caps})
	case soundStatusRoute:
		if h.statusErr != nil {
			return h.statusErr
		}
		return reply(map[string]any{"supported": h.soundSupported, "player": h.player})
	case soundRoute:
		h.playCalls++
		if h.playErr != nil {
			return h.playErr
		}
		var body struct {
			Notes []clipNote `json:"notes"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return err
		}
		h.lastNotes = body.Notes
		duration := clipDurationMs(body.Notes)
		if h.playDurationMs != 0 {
			duration = h.playDurationMs
		}
		return reply(map[string]any{"played": h.playAck, "notes": len(body.Notes), "duration_ms": duration})
	}
	return fmt.Errorf("unknown method %s", method)
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

func (h *harness) act(t *testing.T, version int, action, item, cell string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryAct(version, action, item, cell, values)
	if err != nil {
		t.Fatalf("action %s (item %q cell %q): %v", action, item, cell, err)
	}
	return v
}

func (h *harness) tryAct(version int, action, item, cell string, values map[string]string) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req := sdk.ActionRequest{
		Version:   version,
		Instance:  cur.(sdk.View).Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)),
		Action:    action,
		ItemID:    item,
		CellID:    cell,
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

func (h *harness) route(t *testing.T, name string, params, out any) {
	t.Helper()
	hd, ok := h.handlers[name]
	if !ok {
		t.Fatalf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	res, err := hd(context.Background(), raw)
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

func (h *harness) routeErr(name string, params any) error {
	hd, ok := h.handlers[name]
	if !ok {
		return fmt.Errorf("route %q not registered", name)
	}
	raw, _ := json.Marshal(params)
	_, err := hd(context.Background(), raw)
	return err
}

func findAction(v sdk.View, id string) *sdk.Action {
	for i := range v.Actions {
		if v.Actions[i].ID == id {
			return &v.Actions[i]
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

func cellByID(t *testing.T, g *sdk.Grid, id string) sdk.Cell {
	t.Helper()
	for _, c := range g.Cells {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("grid has no cell %q", id)
	return sdk.Cell{}
}

// --- manifest ---

func TestManifestValidity(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
	if m.ID != ID {
		t.Errorf("id = %s, want %s", m.ID, ID)
	}
	if m.Entrypoint != "musictoy" {
		t.Errorf("entrypoint = %s, want musictoy", m.Entrypoint)
	}
	if !json.Valid(ManifestJSON()) {
		t.Error("ManifestJSON is not valid JSON")
	}
	want := []string{"ipc", "fs.read", "fs.write", "sound"}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v, want %v", m.Permissions, want)
	}
	for i := range want {
		if m.Permissions[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", m.Permissions, want)
		}
	}
}

// --- pattern encoding ---

func TestRowEncodingRoundTrip(t *testing.T) {
	var p Pattern
	p.Toggle(0, 0)
	p.Toggle(0, 4)
	p.Toggle(0, 8)
	p.Toggle(0, 12)
	p.Toggle(3, 15)
	if got := p.rowString(0); got != "x...x...x...x..." {
		t.Fatalf("row 0 = %q, want x...x...x...x...", got)
	}
	rows := p.encode()
	back, healed := decode(rows)
	if healed {
		t.Fatal("decode of a clean encoding reported healing")
	}
	for r := 0; r < numRows; r++ {
		for s := 0; s < numSteps; s++ {
			if back.On(r, s) != p.On(r, s) {
				t.Fatalf("cell r%ds%d round-tripped to %v, want %v", r, s, back.On(r, s), p.On(r, s))
			}
		}
	}
}

func TestRowDecodeHeals(t *testing.T) {
	// A short row blanks entirely; a bad glyph heals to an off cell while
	// its neighbors survive.
	p, healed := decode([numRows]string{"x?x", "x..............x", "x!x.............", "................"})
	if !healed {
		t.Fatal("decode of damaged rows did not report healing")
	}
	if p.Active() != 4 {
		t.Fatalf("healed pattern has %d active cells, want 4", p.Active())
	}
	if !p.On(1, 0) || !p.On(1, 15) {
		t.Error("the intact row lost its cells during healing")
	}
	if !p.On(2, 0) || p.On(2, 1) || !p.On(2, 2) {
		t.Error("the bad-glyph row did not heal to x.x.............")
	}
}

func TestParseRowStrict(t *testing.T) {
	if _, _, err := parseRow("x.x", true); err == nil {
		t.Error("short row accepted strictly")
	}
	if _, _, err := parseRow("x.x?............", true); err == nil {
		t.Error("bad glyph accepted strictly")
	}
	cells, healed, err := parseRow("xX#*1...........", true)
	if err != nil || healed {
		t.Fatalf("strict parse of on-glyphs: err=%v healed=%v", err, healed)
	}
	for s := 0; s < 5; s++ {
		if !cells[s] {
			t.Errorf("on-glyph at step %d parsed off", s)
		}
	}
}

// --- synthesis ---

func TestSynthesizeArpeggiatesWithinStep(t *testing.T) {
	var p Pattern
	p.Toggle(0, 0) // lead
	p.Toggle(2, 0) // low
	p.Toggle(3, 0) // bass
	p.Toggle(0, 1) // lead alone

	scale, _ := findScale("pentatonic")
	notes := p.synthesize(120, scale) // stepMs = 125

	if len(notes) != 18 {
		t.Fatalf("notes = %d, want 18 (3 arp + 1 hit + 14 rests)", len(notes))
	}
	// Step 0: three voices arpeggiate bottom-up inside the step.
	wantWaves := []string{"triangle", "square", "sawtooth"}
	sum := 0
	for i := 0; i < 3; i++ {
		if notes[i].Wave != wantWaves[i] {
			t.Errorf("arp note %d wave = %q, want %q (bass first)", i, notes[i].Wave, wantWaves[i])
		}
		if notes[i].FrequencyHz == 0 {
			t.Errorf("arp note %d is a rest, want pitched", i)
		}
		sum += notes[i].DurationMs
	}
	if sum != 125 {
		t.Errorf("arpeggio spans %dms, want the full 125ms step", sum)
	}
	if notes[0].FrequencyHz >= notes[2].FrequencyHz {
		t.Error("arpeggio did not rise from bass to lead")
	}
	// Step 1: one voice, the whole step.
	if notes[3].Wave != "sawtooth" || notes[3].DurationMs != 125 {
		t.Errorf("solo note = %+v, want sawtooth 125ms", notes[3])
	}
	// Steps 2-15: rests.
	for i := 4; i < len(notes); i++ {
		if notes[i].FrequencyHz != 0 || notes[i].Wave != restWave {
			t.Errorf("empty step %d = %+v, want a rest", i, notes[i])
		}
	}
	if total := clipDurationMs(notes); total != 16*125 {
		t.Errorf("clip duration = %dms, want %dms", total, 16*125)
	}
}

// TestSynthesizeStaysInsideSoundBounds pins the worst case: every cell set
// at both tempo extremes must still render a payload sound/play accepts —
// <=64 notes, 10-2000ms each, <=10s total, frequencies in range, waves set.
func TestSynthesizeStaysInsideSoundBounds(t *testing.T) {
	var p Pattern
	for r := 0; r < numRows; r++ {
		for s := 0; s < numSteps; s++ {
			p.Toggle(r, s)
		}
	}
	for _, bpm := range []int{minBPM, maxBPM, defaultBPM} {
		for _, scale := range scales {
			notes := p.synthesize(bpm, scale)
			if err := clipWithinSoundBounds(notes); err != nil {
				t.Fatalf("full grid at %d bpm %s: %v", bpm, scale.name, err)
			}
			if len(notes) != maxClipNotes {
				t.Fatalf("full grid = %d notes, want %d", len(notes), maxClipNotes)
			}
			if total := clipDurationMs(notes); total != 16*stepMs(bpm) {
				t.Fatalf("clip at %d bpm lasts %dms, want %dms", bpm, total, 16*stepMs(bpm))
			}
		}
	}
	// An empty pattern is all rests and still a legal clip.
	notes := (&Pattern{}).synthesize(maxBPM, scales[0])
	if err := clipWithinSoundBounds(notes); err != nil {
		t.Fatalf("empty pattern at %d bpm: %v", maxBPM, err)
	}
	for _, n := range notes {
		if n.FrequencyHz != 0 {
			t.Errorf("empty pattern note = %+v, want a rest", n)
		}
	}
}

func TestFindRow(t *testing.T) {
	for i, name := range rowNames {
		if r, ok := findRow(name); !ok || r != i {
			t.Errorf("findRow(%q) = %d,%v want %d,true", name, r, ok, i)
		}
		if r, ok := findRow(fmt.Sprintf("%d", i+1)); !ok || r != i {
			t.Errorf("findRow(%d) = %d,%v want %d,true", i+1, r, ok, i)
		}
	}
	if _, ok := findRow("snare"); ok {
		t.Error("findRow accepted an unknown row")
	}
	if _, ok := findRow("0"); ok {
		t.Error("findRow accepted row 0; rows are named or 1-based")
	}
}

// --- presentation: grid ---

func TestGridV2(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)
	g := v.Grid
	if g == nil {
		t.Fatal("version-2 view has no grid")
	}
	if g.Columns != numSteps {
		t.Errorf("columns = %d, want %d", g.Columns, numSteps)
	}
	if len(g.Cells) != numRows*numSteps {
		t.Fatalf("cells = %d, want %d", len(g.Cells), numRows*numSteps)
	}
	for i, c := range g.Cells {
		want := fmt.Sprintf("r%ds%d", i/numSteps, i%numSteps)
		if c.ID != want {
			t.Fatalf("cell %d id = %q, want %q", i, c.ID, want)
		}
		if c.Action != stepAction {
			t.Errorf("cell %q action = %q, want %q", c.ID, c.Action, stepAction)
		}
		if wantLabel := voiceCells[i/numSteps][0]; c.Label != wantLabel {
			t.Errorf("empty cell %q label = %q, want its voice's rest glyph %q", c.ID, c.Label, wantLabel)
		}
	}
	// The step action is cell-declared only, like dogcalc's press.
	if findAction(v, stepAction) != nil {
		t.Error("step appears as a top-level action; it belongs to cells only")
	}
	if findAction(v, "apply_row") != nil {
		t.Error("v2 view exposes the v1 row editor")
	}
}

func TestGridVoiceIdentity(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)

	// Every row carries its own rest glyph, so empty rows are distinguishable.
	seen := map[string]int{}
	for r := 0; r < numRows; r++ {
		got := cellByID(t, v.Grid, fmt.Sprintf("r%ds%d", r, 0)).Label
		if got != voiceCells[r][0] {
			t.Fatalf("row %d rest glyph = %q, want %q", r, got, voiceCells[r][0])
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("rows %d and %d share the rest glyph %q", prev, r, got)
		}
		seen[got] = r
	}

	// The legend names the actual voices in top-to-bottom order with the
	// pitches the current scale will sound.
	last := -1
	for r := 0; r < numRows; r++ {
		want := fmt.Sprintf("%s %s %.0f", voiceCells[r][1], rowNames[r], scales[0].rowFreqHz(r))
		at := strings.Index(v.Grid.Label, want)
		if at < 0 {
			t.Fatalf("legend %q is missing %q", v.Grid.Label, want)
		}
		if at <= last {
			t.Fatalf("legend %q is not in lead/high/low/bass order at %q", v.Grid.Label, want)
		}
		last = at
	}
	if !strings.Contains(v.Grid.Label, "uppercase = on") {
		t.Errorf("legend %q does not explain the case cue", v.Grid.Label)
	}

	// A toggle changes only that cell's case; the other rows keep their
	// identity and the whole pad stays one glyph per cell.
	v = h.act(t, 2, stepAction, "", "r2s5", nil)
	for r := 0; r < numRows; r++ {
		want := voiceCells[r][0]
		if r == 2 {
			want = voiceCells[r][1]
		}
		if got := cellByID(t, v.Grid, fmt.Sprintf("r%ds%d", r, 5)).Label; got != want {
			t.Fatalf("row %d step 5 glyph = %q, want %q", r, got, want)
		}
	}

	// The scale changes the pitches in the legend, never the row letters.
	v = h.act(t, 2, "scale", "", "", nil) // major
	for r := 0; r < numRows; r++ {
		want := fmt.Sprintf("%s %s %.0f", voiceCells[r][1], rowNames[r], scales[1].rowFreqHz(r))
		if !strings.Contains(v.Grid.Label, want) {
			t.Errorf("after scale change legend %q is missing %q", v.Grid.Label, want)
		}
		if got := cellByID(t, v.Grid, fmt.Sprintf("r0s%d", r)).Label; got != voiceCells[0][0] {
			t.Errorf("scale change altered row 0 identity: %q", got)
		}
	}
}

func TestToggleViaGrid(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, stepAction, "", "r0s4", nil)
	if got := cellByID(t, v.Grid, "r0s4").Label; got != "L" {
		t.Fatalf("after press r0s4 label = %q, want L", got)
	}
	if !h.app.pattern.On(0, 4) {
		t.Fatal("cell press did not set the pattern")
	}
	v = h.act(t, 2, stepAction, "", "r0s4", nil)
	if got := cellByID(t, v.Grid, "r0s4").Label; got != "l" {
		t.Fatalf("second press r0s4 label = %q, want l", got)
	}
	if h.app.pattern.On(0, 4) {
		t.Fatal("second press did not clear the pattern cell")
	}
}

func TestStepRequiresCellID(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tryAct(2, stepAction, "", "", nil); err == nil {
		t.Error("step without a cell_id dispatched, want rejection")
	}
	if _, err := h.tryAct(2, stepAction, "", "r9s9", nil); err == nil {
		t.Error("step with an unknown cell_id dispatched, want rejection")
	}
	// Even if a forged id reached the app, canonical-form checking rejects it.
	if err := h.app.toggleStepLocked(context.Background(), "r0s4junk"); err == nil {
		t.Error("step id with a trailing junk dispatched, want rejection")
	}
}

// --- presentation: v1 fallback ---

func TestV1FallbackRowsAsItems(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 1)
	if v.Grid != nil {
		t.Fatal("version-1 snapshot leaked a grid")
	}
	for r, name := range rowNames {
		item := findItem(v, fmt.Sprintf("row_%d", r))
		if item == nil {
			t.Fatalf("v1 view is missing row item %q", name)
		}
		if !strings.Contains(item.Label, name) || !strings.Contains(item.Label, "................") {
			t.Errorf("row item %q label = %q, want name plus empty encoding", name, item.Label)
		}
		if !strings.Contains(item.Detail, rowWaves[r]) {
			t.Errorf("row item %q detail = %q, want its wave", name, item.Detail)
		}
	}
	if findAction(v, "apply_row") == nil {
		t.Fatal("v1 view is missing the apply_row action")
	}
}

func TestV1RowEdit(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 1, "apply_row", "", "", map[string]string{"row_edit": "lead x.x.x.x.x.x.x.x."})
	item := findItem(v, "row_0")
	if item == nil || !strings.Contains(item.Label, "x.x.x.x.x.x.x.x.") {
		t.Fatalf("after row edit, lead item = %+v", item)
	}
	for s := 0; s < numSteps; s += 2 {
		if !h.app.pattern.On(0, s) {
			t.Errorf("row edit missed step %d", s)
		}
	}
	// Clearing via the row editor.
	v = h.act(t, 1, "apply_row", "", "", map[string]string{"row_edit": "lead clear"})
	if h.app.pattern.Active() != 0 {
		t.Errorf("row clear left %d cells on", h.app.pattern.Active())
	}
	// Bad input is a domain rejection on the status line, not an IPC error.
	v = h.act(t, 1, "apply_row", "", "", map[string]string{"row_edit": "snare xxxxxxxxxxxxxxxx"})
	if !strings.Contains(v.Status, "no row") {
		t.Errorf("bad row edit status = %q, want a no-row note", v.Status)
	}
	v = h.act(t, 1, "apply_row", "", "", map[string]string{"row_edit": "lead xx"})
	if !strings.Contains(v.Status, "16 steps") {
		t.Errorf("short row edit status = %q, want a length note", v.Status)
	}
	// Grid presses do not exist on v1.
	if _, err := h.tryAct(1, stepAction, "", "r0s0", nil); err == nil {
		t.Error("v1 accepted a grid press")
	}
}

// --- tempo and scale ---

func TestTempoField(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": "180"})
	if h.app.bpm != 180 {
		t.Fatalf("bpm = %d, want 180", h.app.bpm)
	}
	if !strings.Contains(v.Status, "180 bpm") {
		t.Errorf("status = %q, want a tempo note", v.Status)
	}
	for _, bad := range []string{"30", "300", "fast", ""} {
		v = h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": bad})
		if h.app.bpm != 180 {
			t.Fatalf("tempo %q changed bpm to %d", bad, h.app.bpm)
		}
		if v.State == sdk.ViewError {
			t.Errorf("tempo %q raised the error banner", bad)
		}
	}
}

func TestScaleCycleAndSet(t *testing.T) {
	h := newHarness(t)
	v := h.view(t, 2)
	if a := findAction(v, "scale"); a == nil || !strings.Contains(a.Label, "pentatonic") {
		t.Fatalf("initial scale action = %+v", a)
	}
	v = h.act(t, 2, "scale", "", "", nil)
	if h.app.scaleName != "major" {
		t.Fatalf("after cycle scale = %q, want major", h.app.scaleName)
	}
	v = h.act(t, 2, "scale", "", "", nil)
	v = h.act(t, 2, "scale", "", "", nil)
	if h.app.scaleName != "pentatonic" {
		t.Fatalf("after wrap scale = %q, want pentatonic", h.app.scaleName)
	}
	// The set route takes a name and rejects unknown ones honestly.
	var out struct {
		Scale string `json:"scale"`
	}
	h.route(t, "set", map[string]any{"scale": "chromatic"}, &out)
	if out.Scale != "chromatic" {
		t.Fatalf("set scale = %q, want chromatic", out.Scale)
	}
	if err := h.routeErr("set", map[string]any{"scale": "locrian"}); err == nil {
		t.Error("set accepted an unknown scale")
	}
	if err := h.routeErr("set", map[string]any{"bpm": 9999}); err == nil {
		t.Error("set accepted an out-of-range tempo")
	}
}

// --- playback ---

// TestPlaySilentWithoutGrant: no sound capability means the Play action is
// disabled, the status line says why, and composition still works — with
// zero sound/play calls spent.
func TestPlaySilentWithoutGrant(t *testing.T) {
	h := newHarness(t)
	h.caps = []string{"ipc", "fs.read", "fs.write"}

	v := h.view(t, 2)
	if a := findAction(v, "play"); a == nil || !a.Disabled {
		t.Fatalf("play action = %+v, want disabled", a)
	}
	if !strings.Contains(v.Status, "playback unavailable") || !strings.Contains(v.Status, "sound permission") {
		t.Errorf("status = %q, want an honest unavailable reason", v.Status)
	}
	if _, err := h.tryAct(2, "play", "", "", nil); err == nil {
		t.Error("disabled play action dispatched")
	}
	// Composition is unaffected.
	v = h.act(t, 2, stepAction, "", "r3s0", nil)
	if cellByID(t, v.Grid, "r3s0").Label != "B" {
		t.Error("composing broke on a silent host")
	}
	if h.playCalls != 0 {
		t.Errorf("sound/play called %d times without a grant, want 0", h.playCalls)
	}
	if h.app.sound != soundOff {
		t.Errorf("sound probe = %d, want latched off", h.app.sound)
	}
}

// TestPlayHostUnsupported: the grant names sound but the host reports no
// audio adapter — same silent degradation, different stated reason.
func TestPlayHostUnsupported(t *testing.T) {
	h := newHarness(t)
	h.soundSupported = false

	v := h.view(t, 2)
	if a := findAction(v, "play"); a == nil || !a.Disabled {
		t.Fatalf("play action = %+v, want disabled", a)
	}
	if !strings.Contains(v.Status, "no audio player") {
		t.Errorf("status = %q, want the unsupported reason", v.Status)
	}
	v = h.act(t, 2, stepAction, "", "r0s0", nil)
	if h.playCalls != 0 {
		t.Errorf("sound/play called %d times on an unsupported host, want 0", h.playCalls)
	}
}

// TestPlayServiceUnreachable: an unreachable status route also latches off
// honestly rather than spending a doomed play call.
func TestPlayServiceUnreachable(t *testing.T) {
	h := newHarness(t)
	h.statusErr = errors.New("no such route")

	v := h.view(t, 2)
	if a := findAction(v, "play"); a == nil || !a.Disabled {
		t.Fatalf("play action = %+v, want disabled", a)
	}
	if !strings.Contains(v.Status, "isn't answering") {
		t.Errorf("status = %q, want the unreachable reason", v.Status)
	}
	if h.playCalls != 0 {
		t.Errorf("sound/play called %d times, want 0", h.playCalls)
	}
}

// TestPlayFiresOneBoundedPass: on a supported host a press ships the whole
// rendered pass in a single sound/play call and reports it plainly.
func TestPlayFiresOneBoundedPass(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, stepAction, "", "r3s0", nil)
	h.act(t, 2, stepAction, "", "r0s4", nil)

	v := h.act(t, 2, "play", "", "", nil)
	if h.playCalls != 1 {
		t.Fatalf("sound/play calls = %d, want 1", h.playCalls)
	}
	if len(h.lastNotes) != 16 {
		t.Fatalf("played %d notes, want 16 (2 hits + 14 rests)", len(h.lastNotes))
	}
	if err := clipWithinSoundBounds(h.lastNotes); err != nil {
		t.Fatalf("played clip violates sound bounds: %v", err)
	}
	if h.lastNotes[0].FrequencyHz != scales[0].rowFreqHz(3) || h.lastNotes[0].Wave != "triangle" {
		t.Errorf("first played note = %+v, want the bass row's triangle", h.lastNotes[0])
	}
	if !strings.Contains(v.Status, "one pass") {
		t.Errorf("status = %q, want an honest one-pass report", v.Status)
	}
	if a := findAction(v, "play"); a == nil || a.Disabled {
		t.Error("play stayed disabled on a supported host")
	}
}

// TestPlayBusySurfacesHonestly: a saturated audio adapter returns busy once;
// the status line repeats it plainly and the app never retries.
func TestPlayBusySurfacesHonestly(t *testing.T) {
	h := newHarness(t)
	h.playErr = errors.New("sound/play: platform: audio playback busy")

	v := h.act(t, 2, "play", "", "", nil)
	if h.playCalls != 1 {
		t.Fatalf("sound/play calls = %d, want exactly 1 (no retry loop)", h.playCalls)
	}
	if !strings.Contains(v.Status, "audio busy") {
		t.Errorf("status = %q, want the busy reason", v.Status)
	}
	if v.State == sdk.ViewError {
		t.Error("busy raised the error banner; it is a status-line note")
	}
}

// TestPlayUnsupportedAtPlayTime: the adapter can disappear between probe and
// press — the failure relatches the probe so later views disable Play.
func TestPlayUnsupportedAtPlayTime(t *testing.T) {
	h := newHarness(t)
	h.view(t, 2) // probe latches on
	h.playErr = errors.New("sound/play: platform: host audio playback is unsupported")

	v := h.act(t, 2, "play", "", "", nil)
	if !strings.Contains(v.Status, "playback unavailable") {
		t.Errorf("status = %q, want the unavailable reason", v.Status)
	}
	if h.app.sound != soundOff {
		t.Errorf("probe = %d, want relatched off after unsupported", h.app.sound)
	}
	if a := findAction(v, "play"); a == nil || !a.Disabled {
		t.Error("play stayed enabled after the adapter vanished")
	}
}

// --- acknowledged playback indicator ---

// startPass sets a cell and plays one pass, returning the action view. The
// harness clock starts at t0, so the deadline is t0 plus the acknowledged
// duration unless a test overrides it.
func startPass(t *testing.T, h *harness) sdk.View {
	t.Helper()
	h.act(t, 2, stepAction, "", "r0s0", nil)
	return h.act(t, 2, "play", "", "", nil)
}

// TestPlayingIndicatorFollowsAcknowledgedDuration: nothing claims Playing
// before the host acknowledges a pass, the static indicator appears for the
// acknowledged duration, and the presentation immediately after the deadline
// is clean. The deadline is in-memory only.
func TestPlayingIndicatorFollowsAcknowledgedDuration(t *testing.T) {
	h := newHarness(t)
	if strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("view claimed Playing before any acknowledged playback")
	}

	v := startPass(t, h)
	if !strings.Contains(v.Status, "Playing one pass (2.0s)") {
		t.Fatalf("status after acknowledged play = %q, want a 2.0s Playing indicator", v.Status)
	}
	if !strings.Contains(v.Status, "16 notes over 2.0s") {
		t.Errorf("status after acknowledged play = %q, want the acknowledged pass note", v.Status)
	}

	h.now = t0.Add(2*time.Second - time.Millisecond)
	if !strings.Contains(h.view(t, 2).Status, "Playing one pass (2.0s)") {
		t.Fatal("indicator vanished before the acknowledged deadline")
	}
	h.now = t0.Add(2 * time.Second)
	if strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("indicator survived the acknowledged deadline")
	}

	// A relaunch over the same files starts with no pass in flight.
	h2 := newHarness(t)
	h2.files = h.files
	if strings.Contains(h2.view(t, 2).Status, "Playing") {
		t.Fatal("playback deadline was persisted across launches")
	}
}

// TestPlayingIndicatorUsesAcknowledgedDuration: the host's acknowledged
// duration owns the deadline, not the requested pass length.
func TestPlayingIndicatorUsesAcknowledgedDuration(t *testing.T) {
	h := newHarness(t)
	h.playDurationMs = 400

	v := startPass(t, h)
	if !strings.Contains(v.Status, "Playing one pass (0.4s)") {
		t.Fatalf("status = %q, want the acknowledged 0.4s pass", v.Status)
	}
	var out struct {
		Played     bool `json:"played"`
		DurationMs int  `json:"duration_ms"`
	}
	h.route(t, "play", nil, &out)
	if !out.Played || out.DurationMs != 400 {
		t.Fatalf("play route = %+v, want the acknowledged 400ms", out)
	}

	h.now = t0.Add(400*time.Millisecond - time.Millisecond)
	if !strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("indicator vanished before the acknowledged deadline")
	}
	h.now = t0.Add(400 * time.Millisecond)
	if strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("indicator outlived the acknowledged duration")
	}
}

// TestPlayingIndicatorIsStatic: the indicator is fixed text for the whole
// acknowledged pass — no countdown, moving step, or other time-driven
// decoration — so two presentations at different clocks inside the pass are
// identical.
func TestPlayingIndicatorIsStatic(t *testing.T) {
	h := newHarness(t)
	startPass(t, h)

	h.now = t0.Add(250 * time.Millisecond)
	first := h.view(t, 2)
	h.now = t0.Add(1250 * time.Millisecond)
	second := h.view(t, 2)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("presentation changed inside the pass:\n%+v\n%+v", first, second)
	}
	if !strings.Contains(first.Status, "Playing one pass (2.0s)") {
		t.Fatalf("static pass status = %q, want Playing", first.Status)
	}
}

// TestUnrelatedActionsDoNotRetimePlaying: editing the pattern, changing
// tempo, changing scale, saving, or clearing never clears or extends the
// pass already acknowledged.
func TestUnrelatedActionsDoNotRetimePlaying(t *testing.T) {
	h := newHarness(t)
	if v := startPass(t, h); !strings.Contains(v.Status, "Playing one pass (2.0s)") {
		t.Fatalf("status = %q, want Playing", v.Status)
	}
	h.now = t0.Add(500 * time.Millisecond)

	h.act(t, 2, stepAction, "", "r0s1", nil)
	v := h.view(t, 2)
	if !strings.Contains(v.Status, "Playing one pass (2.0s)") || !strings.Contains(v.Status, "lead step 2 on") {
		t.Fatalf("step edit status = %q, want the pass plus the edit note", v.Status)
	}

	// A faster tempo would make a new pass 1.3s; it must not retime this one.
	h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": "180"})
	v = h.view(t, 2)
	if !strings.Contains(v.Status, "Playing one pass (2.0s)") {
		t.Fatalf("tempo change retimed the acknowledged pass: %q", v.Status)
	}
	h.act(t, 2, "scale", "", "", nil)
	h.act(t, 2, "save", "", "", map[string]string{"save_name": "during pass"})
	h.act(t, 2, "clear", "", "", nil)
	v = h.view(t, 2)
	if !strings.Contains(v.Status, "Playing one pass (2.0s)") || !strings.Contains(v.Status, "pattern cleared") {
		t.Fatalf("unrelated actions changed the pass status: %q", v.Status)
	}

	h.now = t0.Add(1999 * time.Millisecond)
	if !strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("indicator vanished before its original deadline")
	}
	h.now = t0.Add(2 * time.Second)
	if strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("indicator was extended past its original deadline")
	}
}

// TestPlayFailuresNeverClaimOrExtendPlaying: unavailable hosts, busies, and
// acknowledgements that report failure never start an indicator, and a
// failure during an acknowledged pass leaves its original deadline alone.
func TestPlayFailuresNeverClaimOrExtendPlaying(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*harness)
	}{
		{"no grant", func(h *harness) { h.caps = []string{"ipc", "fs.read", "fs.write"} }},
		{"unsupported host", func(h *harness) { h.soundSupported = false }},
		{"status unreachable", func(h *harness) { h.statusErr = errors.New("no such route") }},
		{"busy", func(h *harness) { h.playErr = errors.New("sound/play: platform: audio playback busy") }},
		{"unacknowledged", func(h *harness) { h.playAck = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			h.act(t, 2, stepAction, "", "r0s0", nil)
			var out struct {
				Played bool   `json:"played"`
				Reason string `json:"reason"`
			}
			h.route(t, "play", nil, &out)
			if out.Played {
				t.Fatalf("play route = %+v, want failure", out)
			}
			if out.Reason == "" {
				t.Error("failed play had no readable reason")
			}
			if v := h.view(t, 2); strings.Contains(v.Status, "Playing") {
				t.Fatalf("failed play claimed Playing: %q", v.Status)
			}
			// Composition stays usable after the failure.
			if got := cellByID(t, h.act(t, 2, stepAction, "", "r1s1", nil).Grid, "r1s1").Label; got != "H" {
				t.Fatalf("composition after failure set %q, want H", got)
			}
		})
	}

	// A failure during an acknowledged pass neither clears nor extends it.
	h := newHarness(t)
	startPass(t, h)
	h.now = t0.Add(500 * time.Millisecond)
	h.playErr = errors.New("sound/play: platform: audio playback busy")
	v := h.act(t, 2, "play", "", "", nil)
	if !strings.Contains(v.Status, "Playing one pass (2.0s)") || !strings.Contains(v.Status, "audio busy") {
		t.Fatalf("busy during a pass status = %q, want the pass plus the busy note", v.Status)
	}
	h.playErr = nil
	h.playAck = false
	h.act(t, 2, "play", "", "", nil)
	h.now = t0.Add(1999 * time.Millisecond)
	if !strings.Contains(h.view(t, 2).Status, "Playing one pass (2.0s)") {
		t.Fatal("failed retry cleared or retimed the acknowledged pass")
	}
	h.now = t0.Add(2 * time.Second)
	if strings.Contains(h.view(t, 2).Status, "Playing") {
		t.Fatal("failed retries extended the acknowledged pass")
	}
}

// --- persistence ---

func TestSaveLoadDeletePattern(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, stepAction, "", "r0s0", nil)
	h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": "150"})
	h.act(t, 2, "scale", "", "", nil) // major

	v := h.act(t, 2, "save", "", "", map[string]string{"save_name": "riff one"})
	if findItem(v, "pat_1") == nil {
		t.Fatalf("library item missing after save: %+v", v.Items)
	}

	// Mutate the working pattern, then load restores the saved snapshot.
	h.act(t, 2, "clear", "", "", nil)
	h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": "60"})
	v = h.act(t, 2, "load", "pat_1", "", nil)
	if !h.app.pattern.On(0, 0) || h.app.pattern.Active() != 1 {
		t.Errorf("load restored %d cells, want the saved one", h.app.pattern.Active())
	}
	if h.app.bpm != 150 || h.app.scaleName != "major" {
		t.Errorf("load restored %d bpm %s, want 150 major", h.app.bpm, h.app.scaleName)
	}

	// Saving the same name replaces, not duplicates.
	h.act(t, 2, stepAction, "", "r1s1", nil)
	v = h.act(t, 2, "save", "", "", map[string]string{"save_name": "Riff One"})
	if len(h.app.saved) != 1 {
		t.Fatalf("same-name save duplicated the library to %d", len(h.app.saved))
	}
	if h.app.saved[0].ID != "pat_1" {
		t.Errorf("replaced pattern id = %q, want pat_1", h.app.saved[0].ID)
	}

	v = h.act(t, 2, "delete", "pat_1", "", nil)
	if len(h.app.saved) != 0 || findItem(v, "lib_empty") == nil {
		t.Fatalf("delete left %d patterns, empty notice %v", len(h.app.saved), findItem(v, "lib_empty"))
	}
	if a := findAction(v, "load"); a == nil || !a.Disabled {
		t.Error("load stayed enabled with an empty library")
	}
}

func TestSaveRequiresNameAndBounds(t *testing.T) {
	h := newHarness(t)
	v := h.act(t, 2, "save", "", "", map[string]string{"save_name": "   "})
	if !strings.Contains(v.Status, "name the pattern") {
		t.Errorf("empty-name save status = %q, want the rejection", v.Status)
	}
	for i := 0; i < maxSavedPatterns; i++ {
		h.act(t, 2, "save", "", "", map[string]string{"save_name": fmt.Sprintf("p%d", i)})
	}
	v = h.act(t, 2, "save", "", "", map[string]string{"save_name": "one too many"})
	if !strings.Contains(v.Status, "library is full") {
		t.Errorf("over-cap save status = %q, want the full-library rejection", v.Status)
	}
	if len(h.app.saved) != maxSavedPatterns {
		t.Fatalf("library = %d, want capped at %d", len(h.app.saved), maxSavedPatterns)
	}
}

// TestPersistenceReload: the working pattern, settings, and library survive
// a relaunch through the same document.
func TestPersistenceReload(t *testing.T) {
	h := newHarness(t)
	h.act(t, 2, stepAction, "", "r2s7", nil)
	h.act(t, 2, "tempo", "", "", map[string]string{"tempo_bpm": "90"})
	h.act(t, 2, "save", "", "", map[string]string{"save_name": "keep me"})

	// A fresh instance over the same files loads everything back.
	h2 := newHarness(t)
	h2.files = h.files
	v := h2.view(t, 2)
	if cellByID(t, v.Grid, "r2s7").Label != "O" {
		t.Error("working pattern did not survive relaunch")
	}
	if h2.app.bpm != 90 {
		t.Errorf("bpm after relaunch = %d, want 90", h2.app.bpm)
	}
	if findItem(v, "pat_1") == nil {
		t.Error("saved pattern did not survive relaunch")
	}
	if v.Error != "" {
		t.Errorf("clean reload raised the banner: %q", v.Error)
	}
}

// TestCorruptSaveDegradesHonestly: a damaged document becomes a fresh
// pattern plus an honest banner, and the next change saves over it.
func TestCorruptSaveDegradesHonestly(t *testing.T) {
	h := newHarness(t)
	h.files[statePath] = []byte("{not json")

	v := h.view(t, 2)
	if v.State != sdk.ViewReady {
		t.Fatalf("corrupt save state = %q, want ready over a fresh pattern", v.State)
	}
	if !strings.Contains(v.Error, "unreadable") {
		t.Errorf("corrupt save banner = %q, want the honest note", v.Error)
	}
	if h.app.pattern.Active() != 0 {
		t.Error("corrupt save leaked cells into the fresh pattern")
	}
	h.act(t, 2, stepAction, "", "r0s0", nil)
	var st diskState
	if err := json.Unmarshal(h.files[statePath], &st); err != nil {
		t.Fatalf("post-change save is not valid json: %v", err)
	}
	if st.Rows[0] != "x..............." {
		t.Errorf("saved row = %q, want x...............", st.Rows[0])
	}
}

// TestDeniedStorageNotesIt: a denied fs read is a banner, and composing
// still works in memory — each mutation reports its save failure.
func TestDeniedStorageNotesIt(t *testing.T) {
	h := newHarness(t)
	h.denyFS = true

	v := h.view(t, 2)
	if !strings.Contains(v.Error, "denied") {
		t.Errorf("denied storage banner = %q, want the denial", v.Error)
	}
	v = h.act(t, 2, stepAction, "", "r0s0", nil)
	if cellByID(t, v.Grid, "r0s0").Label != "L" {
		t.Error("composing broke without storage")
	}
	if !strings.Contains(v.Status, "save failed") {
		t.Errorf("post-toggle status = %q, want a save failure note", v.Status)
	}
}

// --- routes ---

func TestRoutes(t *testing.T) {
	h := newHarness(t)

	var st struct {
		BPM    int             `json:"bpm"`
		Scale  string          `json:"scale"`
		Rows   [numRows]string `json:"rows"`
		Active int             `json:"active"`
		Sound  map[string]any  `json:"sound"`
	}
	h.route(t, "state", nil, &st)
	if st.BPM != defaultBPM || st.Scale != "pentatonic" || st.Active != 0 {
		t.Fatalf("state = %+v", st)
	}
	if st.Sound["state"] != "on" || st.Sound["player"] != "fake" {
		t.Errorf("state sound = %+v, want on/fake", st.Sound)
	}

	var tog struct {
		On bool `json:"on"`
	}
	h.route(t, "toggle", map[string]any{"row": 1, "step": 2}, &tog)
	if !tog.On || !h.app.pattern.On(1, 2) {
		t.Fatalf("toggle = %+v", tog)
	}
	if err := h.routeErr("toggle", map[string]any{"row": 9, "step": 0}); err == nil {
		t.Error("toggle accepted an out-of-range row")
	}

	var play struct {
		Played     bool   `json:"played"`
		Notes      int    `json:"notes"`
		DurationMs int    `json:"duration_ms"`
		Reason     string `json:"reason"`
	}
	h.route(t, "play", nil, &play)
	if !play.Played || play.Notes != 16 {
		t.Fatalf("play = %+v, want played with 16 notes", play)
	}
	if play.DurationMs != 16*stepMs(defaultBPM) {
		t.Errorf("play duration = %dms, want %dms", play.DurationMs, 16*stepMs(defaultBPM))
	}

	var saved struct {
		ID       string `json:"id"`
		Replaced bool   `json:"replaced"`
	}
	h.route(t, "save", map[string]any{"name": "route riff"}, &saved)
	if saved.ID == "" || saved.Replaced {
		t.Fatalf("save = %+v", saved)
	}
	var lst struct {
		Count int `json:"count"`
	}
	h.route(t, "list", nil, &lst)
	if lst.Count != 1 {
		t.Fatalf("list count = %d, want 1", lst.Count)
	}
	var loaded struct {
		Loaded bool `json:"loaded"`
	}
	h.route(t, "load", map[string]any{"id": saved.ID}, &loaded)
	if !loaded.Loaded {
		t.Fatal("load route failed")
	}
	if err := h.routeErr("load", map[string]any{"id": "pat_999"}); err == nil {
		t.Error("load accepted an unknown pattern id")
	}
	var del struct {
		Deleted bool `json:"deleted"`
	}
	h.route(t, "delete", map[string]any{"id": saved.ID}, &del)
	if !del.Deleted {
		t.Fatal("delete route failed")
	}
}

// TestPlayRouteSilentHost: the play route reports {played:false, reason}
// instead of erroring — the silent path is a state, not a failure.
func TestPlayRouteSilentHost(t *testing.T) {
	h := newHarness(t)
	h.soundSupported = false

	var play struct {
		Played bool   `json:"played"`
		Reason string `json:"reason"`
	}
	h.route(t, "play", nil, &play)
	if play.Played {
		t.Fatal("play reported success on a silent host")
	}
	if !strings.Contains(play.Reason, "playback unavailable") {
		t.Errorf("play reason = %q, want the unavailable story", play.Reason)
	}
	if h.playCalls != 0 {
		t.Errorf("sound/play called %d times, want 0", h.playCalls)
	}
}
