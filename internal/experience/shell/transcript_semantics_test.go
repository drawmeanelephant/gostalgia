package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// transcriptRows is the transcript viewport's rows as drawn, styles and all,
// trimmed of the padding to the right.
func transcriptRows(t *testing.T, m *Model) []string {
	t.Helper()
	view := m.View()
	// Keep the frame's width and UTF-8 checks, but cut the raw content: the
	// styled rows are the evidence, so stripping them would lose the point.
	_ = checkRendered(t, view, m.width, m.height)
	r := rectOf(t, m, viewTranscript)
	lines := strings.Split(view.Content, "\n")
	out := make([]string, r.Dy())
	for i := range out {
		out[i] = strings.TrimRight(ansi.Cut(lines[r.Min.Y+i], r.Min.X, r.Max.X), " ")
	}
	return out
}

// transcriptDrawn is the transcript viewport's rows joined, styled.
func transcriptDrawn(t *testing.T, m *Model) string {
	t.Helper()
	return strings.Join(transcriptRows(t, m), "\n") + "\n"
}

// plainRows is rows without their styles, as the cues are read.
func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = ansi.Strip(row)
	}
	return out
}

// semanticTranscriptModel is the fixture shell on Prompt with one record of
// each semantic kind the issue names, plus a wrapped result and hostile text.
func semanticTranscriptModel(t *testing.T, th theme.Theme, mode ui.ColorMode) *Model {
	t.Helper()
	m := sized(fixtureModel(th, mode), 80, 24)
	m.clearTranscript()
	m.setMode(modePrompt)
	m.append(entry{`C:\users\guest> echo "hello"`, "command"})
	m.append(entry{"hello", "output"})
	m.append(entry{`unknown command "ehco"; did you mean: echo?`, "error"})
	m.append(entry{"A result long enough to wrap over a row so the gutter marks only the first row, never the continuation, and the logical text stays whole.", "output"})
	m.append(entry{"\x1b[31mred\x1b[0m \x1b]52;c;steal\x07 hostile", "error"})
	return m
}

// goldenFile compares got with the golden at path, or writes it under
// -update-visual after the caller has inspected the change.
func goldenFile(t *testing.T, path, got string) {
	t.Helper()
	if *updateVisual {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s changed:\n%s", path, ansi.Strip(got))
	}
}

// Echo, output and error records carry a noncolor cue in every built-in
// theme and in Plain, where color is gone: the cues alone tell them apart.
func TestTranscriptKindsHaveNoncolorCues(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256, ui.TrueColor} {
			t.Run(th.Name+"/"+modeName(mode), func(t *testing.T) {
				m := semanticTranscriptModel(t, th, mode)
				rows := plainRows(transcriptRows(t, m))
				if len(rows) < 3 {
					t.Fatalf("the transcript drew %d rows", len(rows))
				}
				echo, output, errLine := rows[0], rows[1], rows[2]
				marker := m.kit.Theme().Focus.Marker + " "
				if !strings.HasPrefix(echo, marker) || !strings.Contains(echo, `echo "hello"`) {
					t.Fatalf("echo row %q does not start with the prompt cue %q", echo, marker)
				}
				if !strings.HasPrefix(errLine, "! ") || !strings.Contains(errLine, "unknown command") {
					t.Fatalf("error row %q does not start with the error cue", errLine)
				}
				if !strings.HasPrefix(output, "  ") || !strings.Contains(output, "hello") || strings.HasPrefix(output, marker) {
					t.Fatalf("output row %q does not read as ordinary output", output)
				}
				if echo == output || echo == errLine || output == errLine {
					t.Fatalf("the three kinds are not pairwise distinct: %q", rows[:3])
				}
				if mode == ui.Plain {
					if strings.Contains(m.View().Content, "\x1b") {
						t.Fatal("Plain drew an escape sequence")
					}
					if strings.Contains(m.View().Content, "\x1b[1m") {
						t.Fatal("Plain drew bold")
					}
				}
				// A wrapped line keeps its cue on its first row only.
				for _, row := range rows[3:] {
					if strings.HasPrefix(row, "! ") && strings.Contains(row, "logical text") {
						t.Fatalf("a continuation row was marked: %q", row)
					}
				}
				// Hostile text is sanitized: no injected styling or terminal
				// operation reaches the drawn rows.
				for _, row := range rows {
					if strings.Contains(row, "\x1b]52;") || strings.Contains(row, "steal") {
						t.Fatalf("hostile text reached the drawing: %q", row)
					}
				}
			})
		}
	}
}

// Styled transcript goldens pin the semantic treatment of the same records in
// each built-in theme; the styled rows prove color, and the cues prove that
// the kinds stay apart without it.
func TestTranscriptStyledSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name string
		th   theme.Theme
		mode ui.ColorMode
	}{
		{"transcript-nostalgia", theme.Nostalgia(), ui.ANSI256},
		{"transcript-midnight-ansi256", theme.Midnight(), ui.ANSI256},
		{"transcript-midnight-truecolor", theme.Midnight(), ui.TrueColor},
		{"transcript-monochrome", theme.Monochrome(), ui.Plain},
		{"transcript-high-contrast", theme.HighContrast(), ui.ANSI256},
		{"transcript-high-contrast-light", theme.HighContrastLight(), ui.ANSI256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := semanticTranscriptModel(t, tc.th, tc.mode)
			got := transcriptDrawn(t, m)
			rows := plainRows(transcriptRows(t, m))
			marker := m.kit.Theme().Focus.Marker + " "
			if !strings.HasPrefix(rows[0], marker) || !strings.HasPrefix(rows[2], "! ") {
				t.Fatalf("%s lacks the echo/error cues: %q", tc.name, rows[:3])
			}
			if strings.HasPrefix(rows[1], marker) || strings.HasPrefix(rows[1], "! ") {
				t.Fatalf("%s marks ordinary output", tc.name)
			}
			switch {
			case tc.mode == ui.Plain:
				if strings.Contains(got, "\x1b") {
					t.Fatalf("%s drew escapes in Plain", tc.name)
				}
			case tc.mode == ui.ANSI256:
				if strings.Contains(got, "38;2;") || strings.Contains(got, "48;2;") {
					t.Fatalf("%s drew truecolor in ANSI256", tc.name)
				}
			case tc.mode == ui.TrueColor:
				if !truecolorRE.MatchString(got) {
					t.Fatalf("%s did not draw truecolor", tc.name)
				}
			}
			goldenFile(t, filepath.Join("testdata", tc.name+".golden"), got)
		})
	}
}

// Plain is a color mode of its own, not a theme: the same records in every
// built-in theme's Plain rendering keep the cues and draw no color or bold.
func TestTranscriptPlainSnapshot(t *testing.T) {
	var b strings.Builder
	for _, th := range builtinThemes() {
		m := semanticTranscriptModel(t, th, ui.Plain)
		got := transcriptDrawn(t, m)
		if strings.Contains(got, "\x1b") {
			t.Fatalf("%s Plain drew an escape sequence", th.Name)
		}
		if strings.Contains(got, "\x1b[1m") {
			t.Fatalf("%s Plain drew bold", th.Name)
		}
		fmt.Fprintf(&b, "THEME: %s\n%s\n", th.Name, got)
	}
	goldenFile(t, filepath.Join("testdata", "transcript-plain.snapshot"), b.String())
}

// A theme or color-profile change restyles the records already kept without
// changing them: their logical text stays, and their cues follow the new kit.
func TestTranscriptThemeSwitchSnapshot(t *testing.T) {
	m := semanticTranscriptModel(t, theme.Nostalgia(), ui.ANSI256)
	logical := strings.Join(bufferText(m), "\n")
	var b strings.Builder
	record := func(label string) {
		rows := transcriptRows(t, m)
		fmt.Fprintf(&b, "%s\nkit: %s/%s\nescapes: %t\n", label, m.kit.Theme().Name, modeName(m.kit.Mode()), strings.Contains(strings.Join(rows, ""), "\x1b"))
		for i, row := range rows {
			fmt.Fprintf(&b, "row %02d: %q\n", i, row)
		}
		b.WriteByte('\n')
		if got := strings.Join(bufferText(m), "\n"); got != logical {
			t.Fatalf("%s changed the logical records", label)
		}
		plain := plainRows(rows)
		marker := m.kit.Theme().Focus.Marker + " "
		if !strings.HasPrefix(plain[0], marker) || !strings.HasPrefix(plain[2], "! ") {
			t.Fatalf("%s did not restyle the cues: %q", label, plain[:3])
		}
	}
	record("BEFORE: nostalgia/ansi256")
	m.SetThemeByName("midnight")
	m.SetColorModeByName("truecolor")
	record("AFTER: midnight/truecolor")
	m.SetColorModeByName("plain")
	record("AFTER: midnight/plain")
	if strings.Contains(transcriptDrawn(t, m), "\x1b") {
		t.Fatal("Plain drew escapes after a profile change")
	}
	m.SetThemeByName("monochrome")
	record("AFTER: monochrome/plain")
	m.SetThemeByName("high-contrast")
	m.SetColorModeByName("ansi256")
	record("AFTER: high-contrast/ansi256")
	goldenFile(t, filepath.Join("testdata", "transcript-theme-switch.snapshot"), b.String())
}

// Paging away from the tail keeps the inspected lines while output arrives;
// paging back follows again. A selection reads the logical lines, so the
// gutter cues never reach a copy, and a resize keeps the same selected text.
func TestTranscriptPagingTailSelectionSnapshot(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	m.clearTranscript()
	for i := range 80 {
		m.append(entry{fmt.Sprintf("line %03d", i), "output"})
	}
	var b strings.Builder
	record := func(label string) {
		v := transcriptView(t, m)
		fmt.Fprintf(&b, "%s: first=%d row=%d rows=%d total=%d atEnd=%t atBottom=%t\n",
			label, v.First, v.Row, v.Rows, v.TotalRows, m.transcriptPos.AtEnd(), v.AtBottom())
	}
	record("START")
	press(m, keyPgUp)
	record("AFTER PAGE UP")
	top := transcriptView(t, m).First
	m.append(entry{"new line 080", "output"})
	record("AFTER NEW OUTPUT")
	if got := transcriptView(t, m).First; got != top || strings.Contains(bodyText(t, m), "new line 080") {
		t.Fatalf("paged output jumped from %d to %d", top, got)
	}
	for range 100 {
		press(m, keyPgDown)
	}
	record("AFTER PAGE DOWN")
	if !m.transcriptPos.AtEnd() || !strings.Contains(bodyText(t, m), "new line 080") {
		t.Fatal("paging back did not follow the tail")
	}

	// Ctrl-Y with no selection copies the lines in view from the logical
	// records, so the cue gutter is not part of the copied text.
	act(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if got := copied(t, rec); len(got) != 1 || strings.ContainsAny(got[0], "›»") || strings.Contains(got[0], "! ") {
		t.Fatalf("Ctrl-Y copied the cues: %q", got)
	} else if want := ui.ViewportText(transcriptView(t, m), m.transcriptBuffer()); got[0] != want {
		t.Fatalf("Ctrl-Y copied %q, want the logical lines %q", got[0], want)
	}

	v := transcriptView(t, m)
	fromLine := min(max(v.First, 0), m.transcriptBuf.Len()-2)
	from := bloom.Pos{Line: fromLine, Col: 2}
	to := bloom.Pos{Line: fromLine + 1, Col: 5}
	fromCell, ok := v.CellOf(from)
	if !ok {
		t.Fatalf("selection start %+v is out of sight", from)
	}
	toCell, ok := v.CellOf(to)
	if !ok {
		t.Fatalf("selection end %+v is out of sight", to)
	}
	r := rectOf(t, m, viewTranscript)
	dragBetween(t, m, r.Min.Add(fromCell), r.Min.Add(toCell))
	// A drag includes the cell it ends on, as a terminal's selection does.
	want := m.transcriptBuf.Text(fromLine)[2:] + "\n" + m.transcriptBuf.Text(fromLine + 1)[:6]
	if got := copied(t, rec); len(got) != 2 || got[1] != want {
		t.Fatalf("copy = %q, want the logical text %q", got, want)
	}
	selected, ok := m.root().Selection()
	if !ok || selected != want {
		t.Fatalf("selection = %q (%t), want %q", selected, ok, want)
	}
	fmt.Fprintf(&b, "SELECTION: %q\n", selected)
	record("AFTER SELECTION")

	sized(m, 40, 12)
	m.View()
	resized, ok := m.root().Selection()
	if !ok || resized != selected {
		t.Fatalf("resize changed the selection from %q to %q", selected, resized)
	}
	fmt.Fprintf(&b, "AFTER RESIZE: %dx%d selection=%q\n", m.width, m.height, resized)
	record("AFTER RESIZE")

	act(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if got := copied(t, rec); len(got) != 3 || got[2] != selected {
		t.Fatalf("Ctrl-Y copied %q, want the logical selection %q", got, selected)
	}
	fmt.Fprintf(&b, "COPIED: %q\n", selected)
	goldenFile(t, filepath.Join("testdata", "transcript-paging-tail-selection.snapshot"), b.String())
}

// The 10,000-line cap counts logical lines, not wrapped rows. Adding line
// 10,001 drops the oldest record, keeps navigation valid, and keeps a
// selection on the logical text it was made on.
func TestTranscriptLogicalCapFixture(t *testing.T) {
	m := promptModel(t, ui.Plain, 0)
	long := strings.Repeat("wrapped output ", 6)
	for start := 0; start < maxTranscript; start += 250 {
		var lines []string
		for i := start; i < min(start+250, maxTranscript); i++ {
			if i%100 == 99 {
				lines = append(lines, long)
			} else {
				lines = append(lines, fmt.Sprintf("line %05d", i))
			}
		}
		m.append(entry{strings.Join(lines, "\n"), "output"})
	}
	if len(m.transcript) != maxTranscript || m.transcriptBuf.Len() != maxTranscript {
		t.Fatalf("fixture holds %d entries and %d lines", len(m.transcript), m.transcriptBuf.Len())
	}

	// Inspect away from the tail and select a line near the kept top.
	for range 5 {
		press(m, keyPgUp)
	}
	v := transcriptView(t, m)
	fromLine := max(v.First+1, 0)
	from := bloom.Pos{Line: fromLine, Col: 2}
	to := bloom.Pos{Line: fromLine, Col: 8}
	r := rectOf(t, m, viewTranscript)
	fromCell, ok := v.CellOf(from)
	if !ok {
		t.Fatalf("selection start %+v is out of sight", from)
	}
	toCell, ok := v.CellOf(to)
	if !ok {
		t.Fatalf("selection end %+v is out of sight", to)
	}
	rec := &recordingCaller{}
	m.client = rec
	dragBetween(t, m, r.Min.Add(fromCell), r.Min.Add(toCell))
	want := m.transcriptBuf.Text(fromLine)[2:9]
	if got := copied(t, rec); len(got) != 1 || got[0] != want {
		t.Fatalf("fixture selection copied %q, want %q", got, want)
	}
	pos, top := m.transcriptPos, transcriptView(t, m).First
	topText := m.transcriptBuf.Text(top)

	m.append(entry{"line 10000", "output"})
	if len(m.transcript) != maxTranscript || m.transcriptBuf.Len() != maxTranscript {
		t.Fatalf("after 10,001 lines the transcript holds %d entries and %d lines", len(m.transcript), m.transcriptBuf.Len())
	}
	if got, want := m.transcriptBuf.Text(0), "line 00001"; got != want {
		t.Fatalf("the oldest kept line is %q, want %q", got, want)
	}
	if got, want := m.transcriptBuf.Text(maxTranscript-1), "line 10000"; got != want {
		t.Fatalf("the newest line is %q, want %q", got, want)
	}
	if got := m.transcript[0].text; got != "line 00001" {
		t.Fatalf("the oldest kept record is %q", got)
	}
	// The inspected text stays at the top: dropping one older line moves its
	// index down by one, not the line on screen.
	if got := m.transcriptBuf.Text(transcriptView(t, m).First); got != topText {
		t.Fatalf("eviction moved the top line from %q to %q", topText, got)
	}
	if wantPos := max(pos.Line-1, 0); m.transcriptPos.Line != wantPos {
		t.Fatalf("eviction moved the kept position from %+v to %+v, want line %d", pos, m.transcriptPos, wantPos)
	}
	selected, ok := m.root().Selection()
	if !ok || selected != want {
		t.Fatalf("eviction changed the selection to %q (%t), want %q", selected, ok, want)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "lines: %d\nwrapped rows: %d\ntop line: %q\nfirst: %q\nlast: %q\nselection: %q\n",
		m.transcriptBuf.Len(), transcriptView(t, m).TotalRows, topText,
		m.transcriptBuf.Text(0), m.transcriptBuf.Text(maxTranscript-1), selected)
	goldenFile(t, filepath.Join("testdata", "transcript-cap.snapshot"), b.String())
}

func modeName(mode ui.ColorMode) string {
	switch mode {
	case ui.Plain:
		return "plain"
	case ui.ANSI16:
		return "ansi16"
	case ui.ANSI256:
		return "ansi256"
	default:
		return "truecolor"
	}
}
