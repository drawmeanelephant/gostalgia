package ui

import (
	"image/color"
	"math"
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
)

// relativeLuminance follows the WCAG definition for an 8-bit channel.
func relativeLuminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	linear := func(v uint32) float64 {
		f := float64(v) / 65535
		if f <= 0.04045 {
			return f / 12.92
		}
		return math.Pow((f+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
}

func contrastRatio(a, b color.Color) float64 {
	la, lb := relativeLuminance(a), relativeLuminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// coolTeal reports whether a color is unambiguously in the blue-green family
// rather than the warm amber used by Nostalgia. Slate's blue-gray and teal's
// green-cyan both satisfy it; the gray ANSI256 fallback and orange do not.
func coolTeal(c color.Color) bool {
	r, g, b, _ := c.RGBA()
	return g > r && b > r
}

func sameColor(a, b color.Color) bool {
	ar, ag, ab, _ := a.RGBA()
	br, bg, bb, _ := b.RGBA()
	return ar == br && ag == bg && ab == bb
}

// sgrPrefix is the escape sequence the kit writes before text painted with
// these colors.
func sgrPrefix(k Kit, fg, bg color.Color, bold bool) string {
	painted := k.paint("X", fg, bg, bold)
	if i := strings.Index(painted, "X"); i > 0 {
		return painted[:i]
	}
	return painted
}

// TestMidnightChromeProfilesAreLegibleAndTeal guards the #231 regression:
// the header fill, accent, and focus must be readable in both terminal
// profiles and stay inside Midnight's slate/teal identity. Styled output is
// inspected too, so a stripped-text golden cannot pass this test.
func TestMidnightChromeProfilesAreLegibleAndTeal(t *testing.T) {
	th := theme.Midnight()
	for _, tc := range []struct {
		name string
		mode ColorMode
	}{
		{"ansi256", ANSI256},
		{"truecolor", TrueColor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := New(th, tc.mode)
			headerFG := k.resolve(th.Palette.HeaderText)
			headerBG := k.resolve(th.Palette.Header)
			accent := k.resolve(th.Palette.Accent)
			surface := k.resolve(th.Palette.Surface)
			canvas := k.resolve(th.Palette.Canvas)
			text := k.resolve(th.Palette.Text)
			focusText := k.resolve(th.Focus.Text)
			focusFill := k.resolve(th.Focus.Fill)

			// The tabs row is the shell header: theme text on the header fill.
			row := k.Tabs([]Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Prompt"}}, 1, 40)
			if pair := sgrPrefix(k, headerFG, headerBG, false); !strings.Contains(row, pair) {
				t.Fatalf("header row did not render text on the header fill %q:\n%q", pair, row)
			}
			if got := contrastRatio(headerFG, headerBG); got < 4.5 {
				t.Errorf("header text contrast %.2f:1 is not clearly legible", got)
			}
			if !coolTeal(headerBG) {
				t.Errorf("header fill is not slate/teal: %v", headerBG)
			}

			// Accent text (headings and busy states) has to read on the
			// surface and be visibly unlike canvas and body text.
			if got := contrastRatio(accent, surface); got < 4.5 {
				t.Errorf("accent contrast on surface %.2f:1 is too weak", got)
			}
			if !coolTeal(accent) {
				t.Errorf("accent is not slate/teal: %v", accent)
			}
			for name, other := range map[string]color.Color{"canvas": canvas, "text": text} {
				if sameColor(accent, other) {
					t.Errorf("accent is indistinguishable from %s", name)
				}
			}

			// Focus keeps the inverse highlight the issue calls fine, with
			// the accent as its border.
			if got := contrastRatio(focusText, focusFill); got < 7 {
				t.Errorf("focused control contrast %.2f:1 is too weak", got)
			}
			focused := k.Selection("Home")
			if pair := sgrPrefix(k, focusText, focusFill, true); !strings.Contains(focused, pair) {
				t.Fatalf("focused control did not render the focus pair %q:\n%q", pair, focused)
			}
			if !sameColor(k.resolve(th.Focus.Border), accent) {
				t.Error("focus border does not use the accent token")
			}
			if sameColor(k.resolve(th.Focus.Border), canvas) {
				t.Error("focus border disappears into the canvas")
			}
		})
	}
}
