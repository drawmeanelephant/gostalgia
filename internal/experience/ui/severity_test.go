package ui

import (
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
)

// Severity keeps a level's symbol and label in color modes and falls back to
// the literal bracketed label wherever chrome cannot carry color or weight.
func TestSeverityCue(t *testing.T) {
	type levelCue struct {
		name   string
		symbol string
		state  theme.State
	}
	levels := []levelCue{
		{"INFO", "◆", theme.Normal},
		{"WARN", "▲", theme.Busy},
		{"ERROR", "■", theme.Error},
	}
	for _, tc := range levels {
		t.Run(tc.name, func(t *testing.T) {
			// Plain uses the bracketed label and emits no escape sequences,
			// whatever the theme would otherwise color.
			plain := New(theme.Nostalgia(), Plain)
			got := plain.Severity(tc.symbol, tc.name, tc.state, 20)
			if got != "["+tc.name+"]" {
				t.Fatalf("Plain severity %q", got)
			}
			if strings.Contains(got, "\x1b") {
				t.Fatalf("Plain severity emitted escapes: %q", got)
			}

			// Monochrome chrome is ASCII even in a color profile, so it
			// keeps the bracket rather than the glyph.
			mono := New(theme.Monochrome(), ANSI256)
			if got := mono.Severity(tc.symbol, tc.name, tc.state, 20); !strings.Contains(got, "["+tc.name+"]") || strings.Contains(got, tc.symbol) {
				t.Fatalf("Monochrome severity %q", got)
			}

			// Color modes pair the glyph with the label in the state's own
			// tokens.
			for _, th := range []theme.Theme{theme.Nostalgia(), theme.Midnight(), theme.HighContrast(), theme.HighContrastLight()} {
				for _, mode := range []ColorMode{ANSI256, TrueColor} {
					k := New(th, mode)
					got := k.Severity(tc.symbol, tc.name, tc.state, 20)
					wantText := tc.symbol + " " + tc.name
					if !strings.Contains(got, wantText) || !strings.Contains(got, "\x1b[") {
						t.Fatalf("%s/%d severity %q, want %q with color", th.Name, mode, got, wantText)
					}
					status := th.Status(tc.state)
					want := k.paint(wantText, status.Text, status.Fill, true)
					if got != want {
						t.Fatalf("%s/%d severity %q, want the %s state's colors %q", th.Name, mode, got, tc.state, want)
					}
				}
			}
		})
	}
}
