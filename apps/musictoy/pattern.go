package musictoy

import (
	"fmt"
	"math"
	"strings"
)

// Pattern geometry. The presentation grid caps a pad at 16 columns x 64
// cells, which is exactly a 16-step, 4-row sequencer: every step of every
// row is a cell.
const (
	numRows  = 4
	numSteps = 16
)

// Tempo bounds. They are also the playback bounds: one pass of the pattern
// is always 16 x stepMs, and the slowest allowed tempo keeps a full loop
// under the sound/play 10-second clip ceiling (16 x 250ms = 4000ms).
const (
	minBPM     = 60
	maxBPM     = 240
	defaultBPM = 120
)

// sound/play contract bounds. Synthesis stays inside them by construction —
// 64 cells can emit at most 64 notes and the tempo floor keeps one loop at
// or under 4 seconds — but clipWithinSoundBounds still checks the rendered
// sequence so a future geometry change fails loudly instead of shipping a
// rejected payload.
const (
	maxClipNotes      = 64
	minClipNoteMs     = 10
	maxClipNoteMs     = 2000
	maxClipDurationMs = 10000
	minClipFreqHz     = 20.0
	maxClipFreqHz     = 8000.0

	// restWave is the wave declared on rests: frequency_hz 0 renders
	// silence regardless of shape, but the contract still requires one.
	restWave = "sine"
)

// rootHz is the frequency of scale degree 0 — A2: deep enough for the bass
// row, far above the 20Hz floor.
const rootHz = 110.0

// rowWaves is the chiptune stack, top row to bottom: a sawtooth lead, two
// square middle voices, and a triangle bass.
var rowWaves = [numRows]string{"sawtooth", "square", "square", "triangle"}

// rowNames names rows top to bottom for the version-1 row editor and the
// item detail.
var rowNames = [numRows]string{"lead", "high", "low", "bass"}

// voiceCells is each row's single-character grid identity: the voice's
// initial in lower case for a rest and upper case for a set step. One glyph
// keeps the 16x4 pad inside the existing cell width, and the letter survives
// focus, toggles, theme changes, and Monochrome/Plain rendering without
// relying on row color.
var voiceCells = [numRows][2]string{
	{"l", "L"},
	{"h", "H"},
	{"o", "O"},
	{"b", "B"},
}

// voiceLegend labels the grid with the actual voices and their pitches in the
// current scale, so the cell letters always resolve to real sounding rows.
// It stays compact enough for an 80-column body.
func voiceLegend(scale scaleDef) string {
	parts := make([]string, 0, numRows)
	for r := 0; r < numRows; r++ {
		parts = append(parts, fmt.Sprintf("%s %s %.0f",
			voiceCells[r][1], rowNames[r], scale.rowFreqHz(r)))
	}
	return strings.Join(parts, " · ") + " Hz · uppercase = on"
}

// scaleDef is one named row-to-pitch table. offsets are semitones above the
// root, listed ascending; row r (0 = top) sounds offsets[numRows-1-r], so
// the top grid row is always the highest pitch.
type scaleDef struct {
	name    string
	label   string
	offsets [numRows]int
}

// scales is the data-driven scale table. pentatonic and major spread root,
// third, fifth, octave; chromatic is the diminished stack — the crunchy,
// tense end of the chiptune palette.
var scales = []scaleDef{
	{name: "pentatonic", label: "minor pentatonic", offsets: [numRows]int{0, 3, 7, 12}},
	{name: "major", label: "major triad + octave", offsets: [numRows]int{0, 4, 7, 12}},
	{name: "chromatic", label: "chromatic (diminished)", offsets: [numRows]int{0, 3, 6, 9}},
}

// findScale resolves a stored/requested scale name.
func findScale(name string) (scaleDef, bool) {
	for _, s := range scales {
		if s.name == name {
			return s, true
		}
	}
	return scaleDef{}, false
}

// findRow resolves a row token — a row name ("lead", "bass") or a 1-based
// position ("1".."4", top to bottom) — for the row editor.
func findRow(token string) (int, bool) {
	token = strings.ToLower(strings.TrimSpace(token))
	for i, name := range rowNames {
		if token == name {
			return i, true
		}
	}
	for i := range rowNames {
		if token == fmt.Sprintf("%d", i+1) {
			return i, true
		}
	}
	return 0, false
}

// rowFreqHz maps row r (0 = top) to its frequency in this scale.
func (s scaleDef) rowFreqHz(r int) float64 {
	return rootHz * math.Pow(2, float64(s.offsets[numRows-1-r])/12)
}

// stepMs is the duration of one sixteenth-note step in milliseconds.
func stepMs(bpm int) int { return 60000 / (bpm * 4) }

// Pattern is the step grid: cells[row][step], row 0 on top.
type Pattern struct {
	cells [numRows][numSteps]bool
}

// On reports whether the cell is set.
func (p *Pattern) On(r, s int) bool { return p.cells[r][s] }

// Toggle flips one cell and reports the new state.
func (p *Pattern) Toggle(r, s int) bool {
	p.cells[r][s] = !p.cells[r][s]
	return p.cells[r][s]
}

// Active counts the set cells.
func (p *Pattern) Active() int {
	n := 0
	for r := 0; r < numRows; r++ {
		for s := 0; s < numSteps; s++ {
			if p.cells[r][s] {
				n++
			}
		}
	}
	return n
}

// Clear blanks every cell.
func (p *Pattern) Clear() { p.cells = [numRows][numSteps]bool{} }

// rowString renders one row as 16 characters: 'x' on, '.' off.
func (p *Pattern) rowString(r int) string {
	var b [numSteps]byte
	for s := 0; s < numSteps; s++ {
		b[s] = '.'
		if p.cells[r][s] {
			b[s] = 'x'
		}
	}
	return string(b[:])
}

// encode renders the pattern as its four row strings, top to bottom.
func (p *Pattern) encode() [numRows]string {
	var rows [numRows]string
	for r := 0; r < numRows; r++ {
		rows[r] = p.rowString(r)
	}
	return rows
}

// onChars are the accepted "step on" glyphs for row strings — forgiving on
// load and in the row editor.
const onChars = "xX#*1"

// parseRow decodes a 16-character row string. strict rejects unknown
// glyphs (user input errors); lenient maps them to off and reports healed
// (stored-file repair).
func parseRow(text string, strict bool) (cells [numSteps]bool, healed bool, err error) {
	text = strings.TrimSpace(text)
	if len(text) != numSteps {
		return cells, false, fmt.Errorf("row needs exactly %d steps, got %d", numSteps, len(text))
	}
	for s := 0; s < numSteps; s++ {
		c := text[s]
		switch {
		case strings.IndexByte(onChars, c) >= 0:
			cells[s] = true
		case c == '.' || c == '-' || c == '_' || c == '0':
		default:
			if strict {
				return cells, false, fmt.Errorf("step %d: %q is not a step glyph (use x or .)", s+1, string(c))
			}
			healed = true
		}
	}
	return cells, healed, nil
}

// decode restores a pattern from its row strings, leniently: an over-long
// row keeps its first 16 steps, a short row or a bad glyph heals to off
// cells, and all repairs report healed.
func decode(rows [numRows]string) (p Pattern, healed bool) {
	for r := 0; r < numRows; r++ {
		text := rows[r]
		if len(text) > numSteps {
			text, healed = text[:numSteps], true
		}
		cells, rowHealed, err := parseRow(text, false)
		if err != nil || rowHealed {
			healed = true
		}
		p.cells[r] = cells
	}
	return p, healed
}

// clipNote is one entry of a sound/play notes payload: frequency_hz 0 is a
// rest and wave is required on every note.
type clipNote struct {
	FrequencyHz float64 `json:"frequency_hz"`
	DurationMs  int     `json:"duration_ms"`
	Wave        string  `json:"wave"`
}

// synthesize renders one pass through the pattern as a sound/play note
// sequence. The route is monophonic, so a step with several rows set
// arpeggiates them — bottom-up, bass first — inside the step's own
// timeslice, the classic chiptune chord trick. An empty step emits one
// rest. Note boundaries are cumulative split points so each step totals
// exactly stepMs and the whole clip totals exactly 16*stepMs.
func (p *Pattern) synthesize(bpm int, scale scaleDef) []clipNote {
	ms := stepMs(bpm)
	notes := make([]clipNote, 0, numSteps*numRows)
	for s := 0; s < numSteps; s++ {
		var rows []int
		for r := numRows - 1; r >= 0; r-- {
			if p.cells[r][s] {
				rows = append(rows, r)
			}
		}
		if len(rows) == 0 {
			notes = append(notes, clipNote{DurationMs: ms, Wave: restWave})
			continue
		}
		for i, r := range rows {
			notes = append(notes, clipNote{
				FrequencyHz: scale.rowFreqHz(r),
				DurationMs:  (i+1)*ms/len(rows) - i*ms/len(rows),
				Wave:        rowWaves[r],
			})
		}
	}
	return notes
}

// clipDurationMs totals the rendered sequence.
func clipDurationMs(notes []clipNote) int {
	total := 0
	for _, n := range notes {
		total += n.DurationMs
	}
	return total
}

// clipWithinSoundBounds re-verifies a rendered sequence against the
// sound/play contract. Bounded inputs make violations unreachable; the
// check exists so the failure would be an honest status line rather than a
// rejected IPC call the view could not explain.
func clipWithinSoundBounds(notes []clipNote) error {
	if len(notes) == 0 {
		return fmt.Errorf("nothing to play")
	}
	if len(notes) > maxClipNotes {
		return fmt.Errorf("%d notes exceeds the %d-note clip limit", len(notes), maxClipNotes)
	}
	total := 0
	for i, n := range notes {
		if n.DurationMs < minClipNoteMs || n.DurationMs > maxClipNoteMs {
			return fmt.Errorf("note %d lasts %dms, outside %d-%dms", i, n.DurationMs, minClipNoteMs, maxClipNoteMs)
		}
		if n.FrequencyHz != 0 && (n.FrequencyHz < minClipFreqHz || n.FrequencyHz > maxClipFreqHz) {
			return fmt.Errorf("note %d at %gHz, outside %g-%gHz", i, n.FrequencyHz, minClipFreqHz, maxClipFreqHz)
		}
		total += n.DurationMs
	}
	if total > maxClipDurationMs {
		return fmt.Errorf("clip lasts %dms, over the %dms limit", total, maxClipDurationMs)
	}
	return nil
}
