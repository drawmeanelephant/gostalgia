package shell

import (
	"fmt"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

var (
	keyPgUp   = tea.KeyPressMsg{Code: tea.KeyPgUp}
	keyPgDown = tea.KeyPressMsg{Code: tea.KeyPgDown}
)

// scrollTranscript scrolls the transcript rows up from where the frame at
// the model's size shows it.
func scrollTranscript(m *Model, rows int) {
	if v, ok := m.solve().Viewport(viewTranscript); ok && rows > 0 {
		m.transcriptPos = v.Up(rows)
	}
}

// transcriptView is the transcript viewport as the current frame lays it out.
func transcriptView(t *testing.T, m *Model) bloom.ViewportView {
	t.Helper()
	v, ok := m.solve().Viewport(viewTranscript)
	if !ok {
		t.Fatal("the transcript is not on screen")
	}
	return v
}

// promptModel is an 80x24 prompt screen holding lines numbered from 0 after
// the welcome lines.
func promptModel(t *testing.T, mode ui.ColorMode, lines int) *Model {
	t.Helper()
	m := sized(fixtureModel(theme.Nostalgia(), mode), 80, 24)
	m.clearTranscript()
	m.setMode(modePrompt)
	for i := range lines {
		m.append(entry{fmt.Sprintf("numbered %05d", i), "output"})
	}
	return m
}

// bufferText is every line of the transcript buffer without its styles.
func bufferText(m *Model) []string {
	out := make([]string, m.transcriptBuffer().Len())
	for i := range out {
		out[i] = m.transcriptBuf.Text(i)
	}
	return out
}

// Results beyond the old 400-line cap fill the transcript to 10,000 lines;
// past that the oldest go, the newest stay in order, and the buffer and its
// semantic history hold the same lines.
func TestTranscriptKeepsTheNewestTenThousandLines(t *testing.T) {
	m := promptModel(t, ui.Plain, 0)
	const total = maxTranscript + 1234
	for start := 0; start < total; start += 500 {
		var lines []string
		for i := start; i < min(start+500, total); i++ {
			lines = append(lines, fmt.Sprintf("numbered %05d", i))
		}
		m.append(entry{strings.Join(lines, "\n"), "output"})
		if want := min(start+len(lines), maxTranscript); len(m.transcript) != want {
			t.Fatalf("after %d lines the transcript holds %d, want %d", start+len(lines), len(m.transcript), want)
		}
	}
	got := bufferText(m)
	if len(got) != maxTranscript || len(m.transcript) != maxTranscript {
		t.Fatalf("kept %d buffer lines and %d entries, want %d", len(got), len(m.transcript), maxTranscript)
	}
	for i, line := range got {
		if want := fmt.Sprintf("numbered %05d", total-maxTranscript+i); line != want || m.transcript[i].text != want {
			t.Fatalf("line %d is %q (entry %q), want %q", i, line, m.transcript[i].text, want)
		}
	}
	if body := bodyText(t, m); !strings.Contains(body, fmt.Sprintf("numbered %05d", total-1)) {
		t.Fatalf("the newest line is not at the bottom:\n%s", body)
	}
}

// A result keeps its first 12,000 runes, counted across lines and wide
// characters, and says it was cut; one of exactly 12,000 is kept whole.
func TestTranscriptTruncatesResultsAtTwelveThousandRunes(t *testing.T) {
	line := strings.Repeat("\u754C", 99) + "\n" // 100 runes
	for _, c := range []struct {
		runes int
		cut   bool
	}{{maxResultRunes, false}, {maxResultRunes + 1, true}, {maxResultRunes * 3, true}} {
		m := promptModel(t, ui.Plain, 0)
		text := []rune(strings.Repeat(line, c.runes/100+1))[:c.runes]
		m.append(entry{string(text), "output"})
		got := strings.Join(bufferText(m), "\n")
		want := string(text[:min(c.runes, maxResultRunes)])
		if c.cut {
			want += "\n[output truncated]"
		}
		if got != want {
			t.Fatalf("%d runes: kept %d runes, want %d (cut %v)", c.runes, len([]rune(got)), len([]rune(want)), c.cut)
		}
		if c.cut && !strings.Contains(bodyText(t, m), "[output truncated]") {
			t.Fatalf("%d runes: the truncation notice is not on screen", c.runes)
		}
	}
}

// Each kind is styled as it is appended, sanitized first, and its text kept.
func TestTranscriptStylesEachKindOnAppend(t *testing.T) {
	m := promptModel(t, ui.ANSI256, 0)
	kinds := []string{"output", "muted", "accent", "error", "command", ""}
	for _, kind := range kinds {
		m.append(entry{"kind " + kind + "\x1b[31m!", kind})
	}
	for i, kind := range kinds {
		want := entry{"kind " + kind + "!", kind}
		if got := m.transcriptBuf.Line(i); got != styleTranscriptLine(m.kit, want) {
			t.Fatalf("%q line is %q, want %q", kind, got, styleTranscriptLine(m.kit, want))
		}
		if m.transcriptBuf.Text(i) != want.text || m.transcript[i] != want {
			t.Fatalf("%q kept %q / %+v", kind, m.transcriptBuf.Text(i), m.transcript[i])
		}
	}
}

// A live theme or color change restyles the lines already there, without
// losing text, order or position; an unchanged kit, as a config poll
// re-applies it, and an ordinary frame leave the buffer as it is.
func TestTranscriptRestylesHistoryWithoutMoving(t *testing.T) {
	m := promptModel(t, ui.ANSI256, 0)
	m.SetThemeByName("nostalgia")
	for i := range 60 {
		m.append(entry{fmt.Sprintf("numbered %05d", i), []string{"output", "muted", "accent", "error", "command"}[i%5]})
	}
	press(m, keyPgUp)
	pos, top, text := m.transcriptPos, transcriptView(t, m).First, bufferText(m)

	rev := m.transcriptBuf.Rev()
	m.View()
	m.View()
	m.SetThemeByName("nostalgia")
	m.Update(configUpdateMsg{theme: "nostalgia", colorMode: "ansi256"})
	if m.transcriptBuf.Rev() != rev {
		t.Fatal("drawing, or re-applying the same theme, rebuilt the transcript")
	}

	for _, step := range []struct {
		name  string
		apply func()
		check func(view string)
	}{
		{"midnight", func() { m.SetThemeByName("midnight") }, func(view string) {
			if strings.Contains(view, "38;2;") || strings.Contains(view, "48;2;") {
				t.Fatal("ANSI256 transcript drew truecolor")
			}
		}},
		{"plain", func() { m.SetColorModeByName("plain") }, func(view string) {
			if strings.Contains(view, "\x1b") {
				t.Fatal("Plain transcript drew an escape sequence")
			}
		}},
		{"monochrome plain", func() { m.SetThemeByName("monochrome") }, func(view string) {
			if strings.Contains(view, "\x1b") {
				t.Fatal("Monochrome Plain transcript drew an escape sequence")
			}
		}},
		{"monochrome ansi", func() { m.SetColorModeByName("ansi16") }, func(view string) {
			if strings.Contains(view, "38;2;") || strings.Contains(view, "38;5;") {
				t.Fatal("Monochrome ANSI transcript drew more than basic colors")
			}
		}},
		{"high contrast truecolor", func() { m.SetThemeByName("high-contrast"); m.SetColorModeByName("truecolor") }, nil},
	} {
		step.apply()
		for i, e := range m.transcript {
			if got := m.transcriptBuf.Line(i); got != styleTranscriptLine(m.kit, e) {
				t.Fatalf("%s: line %d keeps the old style: %q", step.name, i, got)
			}
		}
		if got := bufferText(m); strings.Join(got, "\n") != strings.Join(text, "\n") {
			t.Fatalf("%s: restyling changed the text", step.name)
		}
		if m.transcriptPos != pos || transcriptView(t, m).First != top {
			t.Fatalf("%s: restyling moved the transcript from line %d to %d", step.name, top, transcriptView(t, m).First)
		}
		view := m.View().Content
		if !strings.Contains(ansi.Strip(view), m.transcriptBuf.Text(top)) {
			t.Fatalf("%s: the top line is not on screen", step.name)
		}
		if step.check != nil {
			step.check(view)
		}
	}
}

// cls and clear, typed or from the palette, empty the transcript and its
// history and return it to following new output, which arrives alone.
func TestTranscriptClearResetsBufferAndPosition(t *testing.T) {
	for _, way := range []string{"cls", "clear", "palette"} {
		t.Run(way, func(t *testing.T) {
			m := promptModel(t, ui.Plain, 80)
			press(m, keyPgUp)
			if m.transcriptPos.AtEnd() {
				t.Fatal("PgUp did not leave the end")
			}
			if way == "palette" {
				m.openPalette()
				press(m, typed("cls"))
				if items := m.filteredPaletteItems(); len(items) == 0 || items[0].Title != "cls" {
					t.Fatalf("palette offers %+v for cls", items)
				}
				press(m, keyEnter)
			} else {
				press(m, typed(way), keyEnter)
			}
			if len(m.transcript) != 0 || m.transcriptBuf.Len() != 0 || !m.transcriptPos.AtEnd() {
				t.Fatalf("after %s: %d entries, %d lines, position %+v", way, len(m.transcript), m.transcriptBuf.Len(), m.transcriptPos)
			}
			m.append(entry{"fresh line", "output"})
			if got := bufferText(m); len(got) != 1 || got[0] != "fresh line" {
				t.Fatalf("after %s the transcript holds %q", way, got)
			}
			body := bodyText(t, m)
			if !strings.Contains(body, "fresh line") || strings.Contains(body, "numbered") {
				t.Fatalf("after %s the screen shows stale lines:\n%s", way, body)
			}
		})
	}
}

// PgUp and PgDown page through the transcript within its lines; output
// while reading older lines leaves them in place, and paging back to the
// end follows the tail again.
func TestTranscriptPagingAndTailFollow(t *testing.T) {
	m := promptModel(t, ui.Plain, 200)
	v := transcriptView(t, m)
	rows := v.Rows
	if !m.transcriptPos.AtEnd() || !v.AtBottom() || !strings.Contains(bodyText(t, m), "numbered 00199") {
		t.Fatalf("a new transcript does not follow its tail: %+v", v)
	}
	press(m, keyPgUp)
	paged := transcriptView(t, m)
	if m.transcriptPos.AtEnd() || paged.Row != v.Row-rows {
		t.Fatalf("PgUp moved from row %d to %d, want %d", v.Row, paged.Row, v.Row-rows)
	}
	top := paged.First
	m.append(entry{"numbered 00200", "output"})
	if got := transcriptView(t, m).First; got != top || strings.Contains(bodyText(t, m), "numbered 00200") {
		t.Fatalf("output while reading jumped from line %d to %d", top, got)
	}
	for range 100 {
		press(m, keyPgUp)
	}
	if got := transcriptView(t, m); got.First != 0 || got.Row != 0 || !strings.Contains(bodyText(t, m), "numbered 00000") {
		t.Fatalf("PgUp past the start left %+v", got)
	}
	press(m, keyPgDown)
	if got := transcriptView(t, m).Row; got != rows {
		t.Fatalf("PgDown from the start reached row %d, want %d", got, rows)
	}
	for range 100 {
		press(m, keyPgDown)
	}
	if !m.transcriptPos.AtEnd() || !transcriptView(t, m).AtBottom() {
		t.Fatalf("PgDown to the end did not follow again: %+v", m.transcriptPos)
	}
	m.append(entry{"numbered 00201", "output"})
	if body := bodyText(t, m); !strings.Contains(body, "numbered 00201") {
		t.Fatalf("the followed tail did not show new output:\n%s", body)
	}
	// Submitting a command returns to the tail.
	press(m, keyPgUp)
	press(m, typed("echo hi"), keyEnter)
	if !m.transcriptPos.AtEnd() {
		t.Fatal("a command did not return the transcript to its tail")
	}
}

// Resizing keeps the same logical line at the top, long wrapped Unicode
// lines included, while the line is there.
func TestTranscriptResizeKeepsTheTopLine(t *testing.T) {
	m := promptModel(t, ui.Plain, 0)
	for i := range 120 {
		m.append(entry{fmt.Sprintf("wide %03d %s", i, strings.Repeat("\u754C\u9762 he\u0301llo \U0001f469\u200D\U0001f4bb ", 1+i%9)), "output"})
	}
	for range 6 {
		press(m, keyPgUp)
	}
	pos := m.transcriptPos
	top := transcriptView(t, m).First
	want := m.transcriptBuf.Text(top)
	for _, size := range [][2]int{{30, 10}, {200, 60}, {41, 13}, {80, 24}} {
		sized(m, size[0], size[1])
		v := transcriptView(t, m)
		if v.First != top {
			t.Fatalf("%dx%d: line %d is at the top, want %d", size[0], size[1], v.First, top)
		}
		head := strings.Fields(want)[0] + " " + strings.Fields(want)[1]
		if body := bodyText(t, m); !strings.Contains(body, head) {
			t.Fatalf("%dx%d: %q is not on screen:\n%s", size[0], size[1], head, body)
		}
	}
	if m.transcriptPos.Line != pos.Line {
		t.Fatalf("resizing moved the kept position from %+v to %+v", pos, m.transcriptPos)
	}
}

// Dropping the oldest lines keeps the line at the top there; dropping the
// line at the top leaves the oldest line kept there, never a blank screen.
func TestTranscriptEvictionClampsTheTopLine(t *testing.T) {
	m := promptModel(t, ui.Plain, maxTranscript)
	for range 100 {
		press(m, keyPgUp)
	}
	top := transcriptView(t, m).First
	want := m.transcriptBuf.Text(top)
	m.append(entry{"evict one\nevict two", "output"})
	if v := transcriptView(t, m); m.transcriptBuf.Text(v.First) != want {
		t.Fatalf("after eviction %q is at the top, want %q", m.transcriptBuf.Text(v.First), want)
	}
	for range 2 * maxTranscript {
		press(m, keyPgUp)
	}
	if m.transcriptPos.Line != 0 {
		t.Fatalf("paging to the start left %+v", m.transcriptPos)
	}
	m.append(entry{"evict three\nevict four\nevict five", "output"})
	v := transcriptView(t, m)
	if v.First != 0 || m.transcriptPos.Line != 0 {
		t.Fatalf("evicting the top line left %+v at line %d", m.transcriptPos, v.First)
	}
	body := bodyText(t, m)
	if !strings.Contains(body, m.transcriptBuf.Text(0)) || strings.TrimSpace(body) == "" {
		t.Fatalf("evicting the top line did not show the oldest kept line:\n%s", body)
	}
}
