package ui

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const (
	// IconMaxLines and IconMaxWidth bound optional app icon art at the
	// rendering boundary: at most two lines of at most twelve display cells.
	IconMaxLines = 2
	IconMaxWidth = 12

	// iconScanBytes caps how much untrusted metadata an icon normalizes. The
	// SDK already limits a registered manifest's icon; this keeps a render
	// cheap even if a caller hands the component an oversized value.
	iconScanBytes = 4 << 10
)

// iconFallback is the generic ASCII app glyph. It stands in for missing or
// unusable art, so a card that shows an icon column never shows a blank one.
var iconFallback = []string{"+-+", "| |"}

// NormalizeIcon turns untrusted icon metadata into at most two display lines
// of at most twelve cells. Terminal sequences and controls are removed, blank
// lines dropped, and overlong or wide text clipped at a grapheme boundary. It
// returns nil when no usable art remains, so the card can show its fallback
// glyph without reserving empty cells.
func NormalizeIcon(raw string) []string {
	if raw == "" {
		return nil
	}
	if len(raw) > iconScanBytes {
		// Cut before validating so a hostile value cannot make every frame
		// sanitize an unbounded string.
		raw = strings.ToValidUTF8(raw[:iconScanBytes], "")
	}
	if !utf8.ValidString(raw) {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(Sanitize(raw), "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" || ansi.StringWidth(line) == 0 {
			continue
		}
		lines = append(lines, ansi.Truncate(line, IconMaxWidth, ""))
		if len(lines) == IconMaxLines {
			break
		}
	}
	return lines
}

// appIcon is the art AppCard draws: normalized metadata, or the generic ASCII
// glyph when the metadata is missing or unusable. Monochrome chrome keeps its
// bounded ASCII vocabulary, so non-ASCII external art falls back there.
func (k Kit) appIcon(raw string) []string {
	lines := NormalizeIcon(raw)
	if len(lines) == 0 || (k.ASCIIChrome() && !asciiOnly(lines)) {
		return iconFallback
	}
	return lines
}

func asciiOnly(lines []string) bool {
	for _, line := range lines {
		for _, r := range line {
			if r < 0x20 || r > 0x7e {
				return false
			}
		}
	}
	return true
}
