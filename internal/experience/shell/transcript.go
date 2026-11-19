package shell

import (
	"strings"

	"bloomfx.dev/bloom"
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
		styled[i] = styleTranscriptLine(m.kit, m.transcript[len(m.transcript)-1])
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
	if e.kind == "error" {
		if m.presentation == nil {
			m.setMode(modePrompt)
		}
	}
}

// transcriptBuffer is the transcript's styled lines, made on first use.
func (m *Model) transcriptBuffer() *bloom.Buffer {
	if m.transcriptBuf == nil {
		m.transcriptBuf = bloom.NewBuffer("")
		m.transcriptStyle = transcriptStyleOf(m.kit)
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

// styleTranscriptLine is e's line in k's style for its kind.
func styleTranscriptLine(k ui.Kit, e entry) string {
	switch e.kind {
	case "accent", "command":
		return k.Heading(e.text)
	case "muted":
		return k.Muted(e.text)
	case "error":
		return k.StatusText(e.text, theme.Error)
	}
	return k.Text(e.text)
}

// transcriptStyleOf identifies how k styles every kind of transcript line:
// two kits with the same one style each line the same.
func transcriptStyleOf(k ui.Kit) string {
	var b strings.Builder
	for _, kind := range []string{"accent", "command", "muted", "error", "output"} {
		b.WriteString(styleTranscriptLine(k, entry{"x", kind}))
		b.WriteByte(0)
	}
	return b.String()
}

// setKit changes the shell's kit. Lines already in the transcript take the
// new kit's styles, from their kept text and kind, without moving.
func (m *Model) setKit(k ui.Kit) {
	m.kit = k
	style := transcriptStyleOf(k)
	if m.transcriptBuf == nil || style == m.transcriptStyle {
		m.transcriptStyle = style
		return
	}
	m.transcriptStyle = style
	lines := make([]string, len(m.transcript))
	for i, e := range m.transcript {
		lines[i] = styleTranscriptLine(k, e)
	}
	m.transcriptBuf.SetLines(lines...)
}

// transcriptNode is the transcript viewport. It follows new output while
// it is at its end, and wraps lines at the cell.
func (m *Model) transcriptNode() bloom.Child {
	return bloom.Viewport(m.transcriptBuf).At(m.transcriptPos).Follow().
		Wrap(bloom.WrapChars).Style(m.kit.TextStyle()).ID(viewTranscript)
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
