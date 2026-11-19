package shell

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func TestHomeWordmarkFallbackThresholds(t *testing.T) {
	big := selectHomeWordmark(1000)
	if big.font != "big" {
		t.Fatalf("large font = %q, want big", big.font)
	}
	mini := selectHomeWordmark(big.width - 1)
	if mini.font != "mini" {
		t.Fatalf("font below big threshold = %q, want mini", mini.font)
	}
	if mini.width >= big.width || mini.height >= big.height {
		t.Fatalf("mini dimensions %dx%d do not shrink big %dx%d", mini.width, mini.height, big.width, big.height)
	}
	for _, tc := range []struct {
		width int
		want  string
	}{{big.width, "big"}, {big.width - 1, "mini"}, {mini.width, "mini"}, {mini.width - 1, "spaced-title"}, {0, "spaced-title"}} {
		mark := selectHomeWordmark(tc.width)
		if mark.font != tc.want {
			t.Errorf("selectHomeWordmark(%d).font = %q, want %q", tc.width, mark.font, tc.want)
		}
		if mark.font != "spaced-title" && mark.width > tc.width {
			t.Errorf("selectHomeWordmark(%d) chose %s at %d cells", tc.width, mark.font, mark.width)
		}
	}
	if got := selectHomeWordmark(mini.width - 1).text; got != homeSpacedTitle {
		t.Fatalf("narrow fallback = %q, want %q", got, homeSpacedTitle)
	}
}

func TestHomeWordmarkFontsAreASCII(t *testing.T) {
	for _, width := range []int{0, 30, 80, 200} {
		mark := selectHomeWordmark(width)
		if !utf8.ValidString(mark.text) {
			t.Fatalf("%s wordmark is invalid UTF-8", mark.font)
		}
		for _, r := range mark.text {
			if r != '\n' && r > 127 {
				t.Fatalf("%s wordmark contains non-ASCII %q", mark.font, r)
			}
		}
	}
}

func TestHomeWordmarkThemeAndPlainProfiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme theme.Theme
		mode  ui.ColorMode
		plain bool
	}{
		{"monochrome-plain", theme.Monochrome(), ui.Plain, true},
		{"nostalgia-plain", theme.Nostalgia(), ui.Plain, true},
		{"monochrome-ansi256", theme.Monochrome(), ui.ANSI256, false},
		{"nostalgia-ansi256", theme.Nostalgia(), ui.ANSI256, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(fixtureModel(tc.theme, tc.mode), 200, 60)
			m.setMode(modeHome)
			view := m.View().Content
			if tc.plain && strings.Contains(view, "\x1b") {
				t.Fatalf("Plain Home contains terminal escapes: %q", view)
			}
			if !tc.plain && !strings.Contains(view, "\x1b[") {
				t.Fatal("themed Home did not use its theme palette")
			}
			frame := m.solve()
			mark, ok := frame.Rect(homeWordmarkID)
			if !ok {
				t.Fatal("Home wordmark is missing")
			}
			body, _ := frame.Rect(slotBody)
			if !mark.In(body) {
				t.Fatalf("wordmark %v overflows Home body %v", mark, body)
			}
		})
	}
}

func TestShellHomeWordmarkSnapshots(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, size := range geometrySizes {
			t.Run(fmt.Sprintf("%s/%dx%d", th.Name, size[0], size[1]), func(t *testing.T) {
				mode := ui.ANSI256
				if th.Name == theme.Monochrome().Name {
					mode = ui.Plain
				}
				m := sized(fixtureModel(th, mode), size[0], size[1])
				m.setMode(modeHome)
				rects := checkFrame(t, m)
				frame := m.solve()
				mark, ok := frame.Rect(homeWordmarkID)
				if !ok || !mark.In(rects[slotBody]) {
					t.Fatalf("wordmark %v (%v) outside Home body %v", mark, ok, rects[slotBody])
				}
				view := m.View()
				checkRendered(t, view, size[0], size[1])
				lines := screenText(view)
				for i := range lines {
					lines[i] = strings.TrimRight(lines[i], " ")
				}
				got := strings.Join(lines, "\n") + "\n"
				path := filepath.Join("testdata", "home-wordmark", fmt.Sprintf("%s-%dx%d.golden", th.Name, size[0], size[1]))
				if *updateVisual {
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
				if got != string(want) {
					t.Fatalf("Home wordmark snapshot changed:\n%s", got)
				}
			})
		}
	}
}
