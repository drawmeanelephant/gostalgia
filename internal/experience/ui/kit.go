// Package ui is Gostalgia's stateless Charm component kit. Views pass their theme,
// color mode, state, and outer bounds explicitly. Rendering starts no timers,
// performs no IO, and never inspects the host terminal.
package ui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
	"gostalgia/internal/experience/theme"
)

type ColorMode uint8

const (
	Plain ColorMode = iota
	ANSI256
	TrueColor
	ANSI16
)

const ANSI = ANSI16

// Kit owns an explicit color profile and a value copy of its theme. It can be
// reused concurrently; its rendering methods do not mutate either. Lip Gloss
// v2 styles are deterministic value types, so profile downsampling happens
// here, at the point a color is applied.
type Kit struct {
	theme   theme.Theme
	mode    ColorMode
	profile termenv.Profile
}

func New(t theme.Theme, mode ColorMode) Kit {
	profile := termenv.Ascii
	switch mode {
	case ANSI16:
		profile = termenv.ANSI
	case ANSI256:
		profile = termenv.ANSI256
	case TrueColor:
		profile = termenv.TrueColor
	default:
		mode = Plain
	}
	return Kit{theme: t, mode: mode, profile: profile}
}

// Theme returns the kit's theme tokens.
func (k Kit) Theme() theme.Theme {
	return k.theme
}

// Mode returns the kit's explicit color capability mode.
func (k Kit) Mode() ColorMode {
	return k.mode
}

// resolve downsamples a theme color to the kit's explicit profile, yielding the
// x/ansi concrete type that encodes as the right SGR form: 3/4-bit for ANSI16,
// 38;5;n for ANSI256, and 38;2;r;g;b for TrueColor. Indexed palette colors pass
// through where the profile can express them, matching the v1 renderer.
func (k Kit) resolve(c color.Color) color.Color {
	if c == nil {
		return lipgloss.NoColor{}
	}
	if _, ok := c.(lipgloss.NoColor); ok {
		return lipgloss.NoColor{}
	}
	if k.profile == termenv.Ascii {
		return lipgloss.NoColor{}
	}
	switch v := c.(type) {
	case ansi.BasicColor:
		return v
	case ansi.IndexedColor:
		if k.profile != termenv.ANSI {
			return v
		}
		if ac, ok := k.profile.Convert(termenv.ANSI256Color(v)).(termenv.ANSIColor); ok {
			return ansi.BasicColor(ac)
		}
		return lipgloss.NoColor{}
	}
	switch v := k.profile.Convert(k.profile.FromColor(c)).(type) {
	case termenv.ANSIColor:
		return ansi.BasicColor(v)
	case termenv.ANSI256Color:
		return ansi.IndexedColor(v)
	case termenv.RGBColor:
		return lipgloss.Color(string(v))
	default:
		return lipgloss.NoColor{}
	}
}

func (k Kit) paint(text string, fg, bg color.Color, bold bool) string {
	if text == "" || k.mode == Plain {
		return text
	}
	style := lipgloss.NewStyle().Foreground(k.resolve(fg)).Background(k.resolve(bg)).Bold(bold)
	// Lip Gloss doesn't restore an outer style after a nested full SGR reset.
	// Reapply it so composed badges/tabs/help don't expose the host background
	// in separators or padding. Derive the prefix from this explicit style.
	if strings.Contains(text, "\x1b[0m") || strings.Contains(text, "\x1b[m") {
		prefix := strings.TrimSuffix(strings.TrimSuffix(style.Render(""), "\x1b[0m"), "\x1b[m")
		text = strings.NewReplacer(
			"\x1b[0m", "\x1b[0m"+prefix,
			"\x1b[m", "\x1b[m"+prefix,
		).Replace(text)
	}
	return style.Render(text)
}

// Text styles trusted text. Use Sanitize first for file, app, or IPC data.
func (k Kit) Text(text string) string {
	p := k.theme.Palette
	return k.paint(text, p.Text, p.Surface, false)
}

func (k Kit) Heading(text string) string {
	p := k.theme.Palette
	return k.paint(text, p.Accent, p.Surface, k.theme.Typography.HeadingBold)
}

func (k Kit) Muted(text string) string {
	p := k.theme.Palette
	return k.paint(text, p.Muted, p.Surface, false)
}

func (k Kit) StatusText(text string, state theme.State) string {
	s := k.theme.Status(state)
	return k.paint(text, s.Text, s.Fill, false)
}

func (k Kit) Selection(text string) string {
	f := k.theme.Focus
	return k.paint(text, f.Text, f.Fill, k.theme.Typography.HeadingBold)
}

func (k Kit) FocusText(text string) string {
	return k.Selection(k.theme.Focus.Marker + " " + text)
}
