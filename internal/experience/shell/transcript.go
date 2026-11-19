package shell

import (
	"strings"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// maxTranscript is how many logical lines the transcript keeps. Past it
// the oldest lines give way to new output.
const maxTranscript = 10000

// maxResultRunes bounds a single result before it reaches the transcript.
const maxResultRunes = 12000

// viewportID names the viewports of the shell frame for Frame.Viewport.
type viewportID int

const viewTranscript viewportID = iota + 1

// append adds e to the transcript, one logical line per line of its text,
// styled for its kind as it arrives. Wrapping is the viewport's.
func (m *Model) append(e entry) {
	// Never render terminal controls from app/file output (escape sequences,
	// OSC clipboard commands, carriage return spoofing, etc.).
	e.text = safe(e.text)
	// Keep memory bounded even when a service returns an enormous file.
	if r := []rune(e.text); len(r) > maxResultRunes {
		e.text = string(r[:maxResultRunes]) + "\n[output truncated]"
	}
	buf := m.transcriptBuffer()
	lines := strings.Split(e.text, "\n")
	styled := make([]string, len(lines))
	for i, line := range lines {
		m.transcript = append(m.transcript, entry{line, e.kind})
		styled[i] = m.transcriptLook.style(m.transcript[len(m.transcript)-1])
	}
	buf.Append(styled...)
	if over := len(m.transcript) - maxTranscript; over > 0 {
		buf.Delete(bloom.Pos{}, bloom.Pos{Line: over})
		m.transcript = m.transcript[over:]
		if p := m.transcriptPos; !p.AtEnd() {
			// The same line stays at the top; one that was dropped leaves
			// the oldest line kept there instead.
			if p.Line -= over; p.Line < 0 {
				p.Line, p.Row = 0, 0
			}
			m.transcriptPos = p
		}
	}
}

// transcriptBuffer is the transcript's styled lines, made on first use.
func (m *Model) transcriptBuffer() *bloom.Buffer {
	if m.transcriptBuf == nil {
		m.transcriptBuf = bloom.NewBuffer("")
	}
	return m.transcriptBuf
}

// clearTranscript empties the transcript and its history and returns it
// to following new output.
func (m *Model) clearTranscript() {
	m.transcript = nil
	m.transcriptBuffer().Clear()
	m.transcriptPos = bloom.ViewportEnd
}

// resetScroll returns shell output to its tail and an app view's rows to
// their focus.
func (m *Model) resetScroll() {
	m.scroll = 0
	m.transcriptPos = bloom.ViewportEnd
}

// transcriptLook is how a kit styles each kind of transcript line, the
// way its Heading, Muted, StatusText and Text do, as the SGR codes around
// a line of each kind. They are worked out once per kit: restyling
// thousands of lines with the kit itself resolves the same colors and
// measures each line again for every one of them.
type transcriptLook struct {
	heading, muted, fail, text wrap
	// id is the look's lines of every kind: two looks with the same id
	// style each line the same.
	id string
}

// wrap is what a style writes before and after a line.
type wrap struct{ before, after string }

func transcriptLookOf(k ui.Kit) transcriptLook {
	// A sanitized logical line holds no newline, tab, carriage return or
	// escape sequence, and these styles set only colors and bold, so the
	// kit paints any such line as it paints "x": the same codes around it.
	wrapOf := func(paint func(string) string) wrap {
		before, after, _ := strings.Cut(paint("x"), "x")
		return wrap{before, after}
	}
	l := transcriptLook{
		heading: wrapOf(k.Heading),
		muted:   wrapOf(k.Muted),
		fail:    wrapOf(func(s string) string { return k.StatusText(s, theme.Error) }),
		text:    wrapOf(k.Text),
	}
	var b strings.Builder
	for _, kind := range []string{"accent", "command", "muted", "error", "output"} {
		b.WriteString(l.style(entry{"x", kind}))
		b.WriteByte(0)
	}
	l.id = b.String()
	return l
}

// style is e's line, sanitized, in the look's style for its kind.
func (l transcriptLook) style(e entry) string {
	if e.text == "" {
		return ""
	}
	w := l.text
	switch e.kind {
	case "accent", "command":
		w = l.heading
	case "muted":
		w = l.muted
	case "error":
		w = l.fail
	}
	return w.before + e.text + w.after
}

// setKit changes the shell's kit. Lines already in the transcript take the
// new kit's styles, from their kept text and kind, without moving.
func (m *Model) setKit(k ui.Kit) {
	m.kit = k
	m.syncMotion()
	look := transcriptLookOf(k)
	same := look.id == m.transcriptLook.id
	m.transcriptLook = look
	if m.transcriptBuf == nil || same {
		return
	}
	lines := make([]string, len(m.transcript))
	for i, e := range m.transcript {
		lines[i] = look.style(e)
	}
	m.transcriptBuf.SetLines(lines...)
}

// transcriptCueWidth is the column of semantic cues kept left of every
// transcript line. The cues are a gutter: the viewport draws them beside
// its text, and selecting, copying and wrapping read the logical lines
// without them.
const transcriptCueWidth = 2

// transcriptCue is the cue for a transcript line, drawn in its gutter. An
// echo carries the theme's prompt marker, an error a bang, and ordinary
// output a blank column: the glyphs, not the colors alone, tell the kinds
// apart, so Monochrome and Plain keep the distinction. A line that wraps
// is cued once, on its first row.
func (m *Model) transcriptCue(row bloom.GutterRow) string {
	if row.Continued || row.Line < 0 || row.Line >= len(m.transcript) {
		return m.kit.Text("  ")
	}
	switch m.transcript[row.Line].kind {
	case "command":
		return m.kit.Heading(m.kit.Theme().Focus.Marker + " ")
	case "error":
		return m.kit.StatusText("! ", theme.Error)
	}
	return m.kit.Text("  ")
}

// transcriptNode is the transcript viewport. It follows new output while
// it is at its end, and wraps lines at the cell. The wheel scrolls it as
// PgUp and PgDn do, following again once it reaches the end, and the mouse
// selects text in it to copy.
func (m *Model) transcriptNode() bloom.Child {
	return bloom.Viewport(m.transcriptBuf).At(m.transcriptPos).Follow().
		Wrap(bloom.WrapChars).Style(m.kit.TextStyle()).ID(viewTranscript).Selectable().
		Gutter(transcriptCueWidth, m.transcriptCue).
		OnScroll(func(pos bloom.ViewportPos) tea.Cmd {
			return m.baseAct(func() tea.Cmd {
				m.transcriptPos = pos
				return nil
			})
		})
}

// pageTranscript scrolls the transcript a page up or down, when it is on
// screen. The page is measured on a frame solved for the transcript as it
// is now: output since the last drawn frame may have dropped lines from
// its top, which that frame's offsets would not account for.
func (m *Model) pageTranscript(down bool) {
	v, ok := m.solve().Viewport(viewTranscript)
	if !ok {
		return
	}
	if down {
		m.transcriptPos = v.PageDown()
	} else {
		m.transcriptPos = v.PageUp()
	}
}
