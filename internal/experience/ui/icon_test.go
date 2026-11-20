package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

func TestNormalizeIconBoundsAndSanitizes(t *testing.T) {
	combining := strings.Repeat("e\u0301", 20)
	wide := strings.Repeat("界", 10)
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"two lines", "ab\ncd", []string{"ab", "cd"}},
		{"one line", ">_", []string{">_"}},
		{"excess lines", "a\nb\nc", []string{"a", "b"}},
		{"overlong art", "abcdefghijklmnop", []string{"abcdefghijkl"}},
		{"oversized input", strings.Repeat("x", 5000), []string{strings.Repeat("x", 12)}},
		{"ansi and osc controls", "\x1b[31mred\x1b[0m\n\x1b]52;c;clip\aY", []string{"red", "Y"}},
		{"nul and other controls", "a\x00b\x1b[2Jc\rd", []string{"abcd"}},
		{"blank lines", "\n \nAB\n\nCD\n", []string{"AB", "CD"}},
		{"wide text", wide, []string{strings.Repeat("界", 6)}},
		{"combining text", combining, []string{strings.Repeat("e\u0301", 12)}},
		{"wide plus ascii", "界abc", []string{"界abc"}},
		{"invalid utf-8", string([]byte{'a', 0xff, 'b'}), nil},
		{"only blanks", "  \n\t\n", nil},
		{"only zero width", "\u0301\u0301", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeIcon(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("NormalizeIcon(%q) = %q, want %q", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("NormalizeIcon(%q) = %q, want %q", tc.raw, got, tc.want)
				}
			}
			for _, line := range got {
				if w := ansi.StringWidth(line); w > IconMaxWidth {
					t.Fatalf("line %q is %d cells, limit %d", line, w, IconMaxWidth)
				}
				if strings.Contains(line, "\x1b") || strings.ContainsRune(line, '\a') {
					t.Fatalf("line %q kept a terminal sequence", line)
				}
				for _, r := range line {
					if unicode.IsControl(r) {
						t.Fatalf("line %q kept control %U", line, r)
					}
				}
			}
		})
	}
}

func TestAppCardShowsSuppliedAndFallbackIcons(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	supplied := k.AppCard(AppCard{
		Name: "Echo", ID: "com.gostalgia.echo", Version: "0.1.0", Icon: ">_\n>_",
		Status: "LIVE / PID 7", State: theme.Success, Focused: true,
	}, Bounds{80, 6})
	for _, want := range []string{"Echo", "LIVE / PID 7", ">_", "com.gostalgia.echo"} {
		if !strings.Contains(supplied, want) {
			t.Fatalf("supplied icon card lacks %q:\n%s", want, supplied)
		}
	}

	for _, tc := range []struct {
		name string
		icon string
	}{
		{"missing", ""},
		{"invalid", "\x1b[2J\x1b]52;c;clip\a\n\x00"},
		{"invalid utf-8", string([]byte{0xff, 0xfe})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := k.AppCard(AppCard{
				Name: "Echo", ID: "com.gostalgia.echo", Version: "0.1.0", Icon: tc.icon,
				Status: "READY", Focused: true,
			}, Bounds{80, 6})
			if !strings.Contains(card, "+-+") || !strings.Contains(card, "| |") {
				t.Fatalf("%s icon card lacks the generic ASCII fallback:\n%s", tc.name, card)
			}
			if !strings.Contains(card, "READY") || !strings.Contains(card, "Echo") {
				t.Fatalf("%s icon card lost identity/state:\n%s", tc.name, card)
			}
			if strings.ContainsAny(card, "\x1b\a\r\x00") {
				t.Fatalf("%s icon card rendered terminal controls: %q", tc.name, card)
			}
			assertRectangle(t, card, Bounds{80, 6})
		})
	}
}

// Compact cards keep the name and state, so icon art never takes their cells.
func TestCompactAppCardNeverReservesIconSpace(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	for _, height := range []int{1, 2, 3} {
		with := k.AppCard(AppCard{Name: "Echo", Icon: "XXXXXXXXXXXX\nXXXXXXXXXXXX", Status: "READY"}, Bounds{24, height})
		without := k.AppCard(AppCard{Name: "Echo", Status: "READY"}, Bounds{24, height})
		if with != without {
			t.Fatalf("%d-row card changed when icon art was supplied:\nwith:\n%s\nwithout:\n%s", height, with, without)
		}
		if strings.Contains(with, "XXXXXXXXXXXX") {
			t.Fatalf("%d-row compact card showed icon art:\n%s", height, with)
		}
		assertRectangle(t, with, Bounds{24, height})
	}
}

// A full card drops the icon column instead of squeezing state that still
// fits, and it never leaves blank cells reserved for absent art.
func TestNarrowAppCardDropsIconRatherThanSqueezingState(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	card := AppCard{
		Name: "Echo", ID: "com.gostalgia.echo", Status: "LIVE / PID 1234",
		Icon: strings.Repeat("W", IconMaxWidth),
	}
	view := k.AppCard(card, Bounds{24, 6})
	if strings.Contains(view, "WWWW") {
		t.Fatalf("narrow card squeezed icon art into the identity:\n%s", view)
	}
	if !strings.Contains(view, "LIVE / PID 1234") {
		t.Fatalf("narrow card lost its status:\n%s", view)
	}
	card.Icon = ""
	if without := k.AppCard(card, Bounds{24, 6}); without != view {
		t.Fatalf("narrow card silently reserved icon space:\nwith art:\n%s\nwithout:\n%s", view, without)
	}
}

// Monochrome keeps its bounded ASCII vocabulary: non-ASCII external art falls
// back to the generic glyph instead of introducing wcwidth-dependent art.
func TestAppCardMonochromeUsesASCIISafeIcon(t *testing.T) {
	view := New(theme.Monochrome(), Plain).AppCard(AppCard{
		Name: "Echo", ID: "com.gostalgia.echo", Icon: "界面\n界面",
		Status: "READY", Focused: true,
	}, Bounds{40, 6})
	if strings.Contains(view, "界") {
		t.Fatalf("Monochrome rendered non-ASCII icon art:\n%s", view)
	}
	if !strings.Contains(view, "+-+") {
		t.Fatalf("Monochrome lacks its ASCII fallback:\n%s", view)
	}
	for _, r := range view {
		if r > unicode.MaxASCII {
			t.Fatalf("Monochrome icon card contains non-ASCII %q:\n%s", r, view)
		}
	}
}

// TestAppIconSnapshots pins supplied, missing, and hostile icon art beside
// identity and state in every built-in theme, with Plain separate. Supplied
// art is ASCII so Monochrome's own fallback rule does not obscure the layout.
func TestAppIconSnapshots(t *testing.T) {
	cases := []struct {
		name  string
		theme theme.Theme
		mode  ColorMode
	}{
		{"nostalgia", theme.Nostalgia(), ANSI256},
		{"midnight", theme.Midnight(), TrueColor},
		{"monochrome", theme.Monochrome(), Plain},
		{"high-contrast", theme.HighContrast(), ANSI256},
		{"high-contrast-light", theme.HighContrastLight(), ANSI256},
		{"plain", theme.Midnight(), Plain},
	}
	art := []struct {
		name string
		icon string
	}{
		{"supplied", ">_\n>_"},
		{"missing", ""},
		{"invalid", "\x1b[31m\x00\n\x1b]52;c;clip\a"},
	}
	var views []string
	for _, tc := range cases {
		k := New(tc.theme, tc.mode)
		for _, a := range art {
			card := k.AppCard(AppCard{
				Name: "Echo", ID: "com.gostalgia.echo", Version: "0.1.0", Icon: a.icon,
				Description: "A friendly first app.", Status: "LIVE / PID 7",
				State: theme.Success, Focused: true,
			}, Bounds{40, 6})
			views = append(views, tc.name+" "+a.name+"\n"+card)
		}
	}
	snapshotFile(t, "app-icons-normal-and-fallback", strings.ReplaceAll(strings.Join(views, "\n\n"), "\x1b", `\x1b`))
}

// snapshotFile writes a named .snapshot beside component goldens, keeping the
// contract's evidence names while using the same inspect-and-update flow.
func snapshotFile(t *testing.T, name, view string) {
	t.Helper()
	lines := strings.Split(view, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", name+".snapshot")
	if *update {
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
	if string(want) != got {
		t.Fatalf("%s changed; inspect the rendering and rerun with -update:\n%s", path, got)
	}
}
