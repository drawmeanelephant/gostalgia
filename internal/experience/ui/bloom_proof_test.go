package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"gostalgia/internal/experience/theme"
)

// These proofs pin how the exact bloom release composes kit output. bloom is
// not wired into production yet; the shell keeps rendering kit strings itself.

var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;:]*m")

func builtinThemes() []theme.Theme {
	return []theme.Theme{theme.Nostalgia(), theme.Midnight(), theme.Monochrome(),
		theme.HighContrast(), theme.HighContrastLight()}
}

// hasTruecolor reports whether any SGR sequence in s selects a 24-bit
// foreground, background, or underline color, in either the semicolon
// (38;2;r;g;b) or colon (38:2::r:g:b) encoding.
func hasTruecolor(s string) bool {
	for _, seq := range sgrPattern.FindAllString(s, -1) {
		params := strings.FieldsFunc(seq[2:len(seq)-1], func(r rune) bool { return r == ';' || r == ':' })
		for i := 0; i < len(params)-1; i++ {
			switch params[i] {
			case "38", "48", "58":
				switch params[i+1] {
				case "2":
					return true
				case "5":
					i += 2
				}
			}
		}
	}
	return false
}

func TestTruecolorDetector(t *testing.T) {
	for in, want := range map[string]bool{
		"\x1b[38;2;1;2;3mx":    true,
		"\x1b[1;48;2;1;2;3mx":  true,
		"\x1b[38:2::1:2:3mx":   true,
		"\x1b[58:2::1:2:3mx":   true,
		"\x1b[38;5;2mx":        false,
		"\x1b[38;5;38;48;5;2m": false,
		"\x1b[2;38mx":          false,
		"38;2 outside an SGR":  false,
	} {
		if got := hasTruecolor(in); got != want {
			t.Errorf("hasTruecolor(%q) = %v, want %v", in, got, want)
		}
	}
}

// proofPanel renders an actual kit Panel whose body is external text passed
// through the Sanitize gate, as production callers must.
func proofPanel(k Kit) string {
	external := "log \x1b[31mred\x1b[0m \x1b]52;c;c2VjcmV0\a \x1b]8;;https://example.invalid\atext\x1b]8;;\a \x1bPq#0\x1b\\ \x1b_Gf=24\x1b\\done"
	return k.Panel(Panel{Title: "Proof", Body: Sanitize(external), Focused: true}, Bounds{Width: 30, Height: 6})
}

// proofRoot builds a bloom Root the way the shell must: explicit theme colors,
// no copy flash, and the shell's chosen profile delivered directly. Root.Init
// is never called, so the host terminal is never asked for its colors.
func proofRoot(t *testing.T, th theme.Theme, profile colorprofile.Profile) *bloom.Root {
	t.Helper()
	root := bloom.New(bloom.WithColors(th.Palette.Text, th.Palette.Surface), bloom.WithCopyFlash(false))
	if cmd := root.Update(tea.ColorProfileMsg{Profile: profile}); cmd != nil {
		t.Fatal("profile message scheduled a command on an idle root")
	}
	return root
}

func assertProofText(t *testing.T, label, out string) {
	t.Helper()
	plain := sgrPattern.ReplaceAllString(out, "")
	for _, want := range []string{"Proof", "log red", "text", "done"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("%s: rendered panel lost %q:\n%s", label, want, plain)
		}
	}
	for _, leaked := range []string{"c2VjcmV0", "example.invalid", "f=24", "\x1b]", "\x1bP", "\x1b_"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("%s: external escape payload %q survived sanitization:\n%q", label, leaked, out)
		}
	}
}

func TestBloomRenderKitPanelPlainEmitsNoColor(t *testing.T) {
	for _, th := range builtinThemes() {
		t.Run(th.Name, func(t *testing.T) {
			panel := proofPanel(New(th, Plain))
			root := proofRoot(t, th, colorprofile.NoTTY)
			for label, out := range map[string]string{
				"bloom.Render": bloom.Render(30, 6, bloom.Text(panel)),
				"Root.Render":  root.Render(30, 6, bloom.Text(panel)),
			} {
				if codes := sgrPattern.FindAllString(out, -1); len(codes) > 0 {
					t.Fatalf("%s: Plain emitted %d SGR sequences: %q", label, len(codes), codes)
				}
				if strings.Contains(out, "\x1b") {
					t.Fatalf("%s: Plain emitted an escape sequence: %q", label, out)
				}
				assertProofText(t, label, out)
			}
			if cmd := root.Update(tea.WindowSizeMsg{Width: 30, Height: 6}); cmd != nil {
				t.Fatal("idle root scheduled a frame command")
			}
		})
	}
}

func TestBloomRenderKitPanelANSI256EmitsNoTruecolor(t *testing.T) {
	for _, th := range builtinThemes() {
		t.Run(th.Name, func(t *testing.T) {
			k := New(th, ANSI256)
			panel := proofPanel(k)
			if hasTruecolor(panel) || len(sgrPattern.FindAllString(panel, -1)) == 0 {
				t.Fatalf("kit ANSI256 panel is not an indexed-color input: %q", panel)
			}
			root := proofRoot(t, th, colorprofile.ANSI256)
			for label, out := range map[string]string{
				"bloom.Render": bloom.Render(30, 6, bloom.Text(panel)),
				"Root.Render":  root.Render(30, 6, bloom.Text(panel)),
			} {
				if len(sgrPattern.FindAllString(out, -1)) == 0 {
					t.Fatalf("%s: ANSI256 output lost its colors: %q", label, out)
				}
				if hasTruecolor(out) {
					t.Fatalf("%s: ANSI256 output contains truecolor: %q", label, out)
				}
				assertProofText(t, label, out)
			}
		})
	}
}

// Blending effects mix colors in 24-bit even after the root learns the
// ANSI256 profile, so composed output that uses them must be converted
// explicitly with the shell's chosen profile. Plain and Monochrome attach no
// blending effects at all. If this test starts failing because the raw output
// no longer contains truecolor, bloom changed and the documented workaround
// in docs/shell.md should be revisited.
func TestBloomBlendingNeedsExplicitProfileConversion(t *testing.T) {
	th := theme.Nostalgia()
	panel := New(th, ANSI256).Panel(Panel{Title: "Base", Body: "under the dialog"}, Bounds{Width: 30, Height: 6})
	root := proofRoot(t, th, colorprofile.ANSI256)
	raw := root.Render(30, 6, bloom.Overlay(bloom.Text(panel),
		bloom.Layer(bloom.Text("overlay")).Backdrop(fx.Dim{Amount: 0.5})))
	if !hasTruecolor(raw) {
		t.Fatalf("expected fx.Dim to introduce truecolor at this bloom pin; got %q", raw)
	}

	var buf bytes.Buffer
	w := colorprofile.Writer{Forward: &buf, Profile: colorprofile.ANSI256}
	if _, err := w.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	converted := buf.String()
	if hasTruecolor(converted) {
		t.Fatalf("explicit ANSI256 conversion left truecolor: %q", converted)
	}
	if len(sgrPattern.FindAllString(converted, -1)) == 0 {
		t.Fatalf("explicit ANSI256 conversion dropped all color: %q", converted)
	}
	if plain := sgrPattern.ReplaceAllString(converted, ""); !strings.Contains(plain, "overlay") || !strings.Contains(plain, "Base") {
		t.Fatalf("conversion changed the composed text: %q", plain)
	}
}
