package ui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

// ASCIIChrome reports whether shell-owned chrome needs its bounded ASCII
// vocabulary. Color mode is independent: Plain with another theme keeps its
// ordinary Unicode chrome.
func (k Kit) ASCIIChrome() bool {
	return k.theme.Name == theme.Monochrome().Name
}

// Separator separates shell-owned annotations without touching adjacent
// application or user content.
func (k Kit) Separator() string {
	if k.ASCIIChrome() {
		return " | "
	}
	return " · "
}

// EmptyMarker is the shell-owned empty-state marker.
func (k Kit) EmptyMarker() string {
	if k.ASCIIChrome() {
		return k.theme.Status(theme.Empty).Symbol
	}
	return "◇"
}

// Hint maps only the known glyphs in a shell-owned hint or annotation. It is
// not a filter for external text: external text still goes through Sanitize
// and retains its Unicode verbatim.
func (k Kit) Hint(text string) string {
	if !k.ASCIIChrome() {
		return text
	}
	return strings.NewReplacer(
		"←→↑↓", "LRUD",
		"↑↓", "Up/Down",
		"←→", "Left/Right",
		"↑", "Up",
		"↓", "Down",
		"←", "Left",
		"→", "Right",
		"·", "|",
		"…", "...",
		"●", "*",
		"─", "-",
		"–", "-",
	).Replace(text)
}

// KeyLabel renders a shell-owned key label in the theme's chrome vocabulary.
func (k Kit) KeyLabel(key string) string {
	return k.Hint(key)
}

// Truncate clips one trusted, possibly ANSI-styled line by grapheme cell
// width, using a bounded ASCII suffix when Monochrome chrome requires it.
func (k Kit) Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s, _, _ = strings.Cut(s, "\n")
	if k.ASCIIChrome() {
		return ansi.Truncate(s, width, strings.Repeat(".", min(width, 3)))
	}
	return ansi.Truncate(s, width, "…")
}

// Rule draws a shell-owned horizontal rule.
func (k Kit) Rule(width int) string {
	if width <= 0 {
		return ""
	}
	if k.ASCIIChrome() {
		return strings.Repeat("-", width)
	}
	return strings.Repeat("─", width)
}
