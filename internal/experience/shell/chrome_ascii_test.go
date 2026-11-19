package shell

import (
	"fmt"
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

var monochromeOwnedUnicode = []string{"·", "↑", "↓", "←", "→", "…", "◇", "●", "─", "–"}

func requireMonochromeChromeASCII(t *testing.T, screen string) {
	t.Helper()
	for _, glyph := range monochromeOwnedUnicode {
		if strings.Contains(screen, glyph) {
			t.Fatalf("Monochrome shell-owned chrome contains %q:\n%s", glyph, screen)
		}
	}
}

func TestMonochromeChromeASCIIEveryNamedView(t *testing.T) {
	for _, size := range geometrySizes {
		for _, s := range geometryScreens() {
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Monochrome(), ui.Plain), size[0], size[1])
				s.setup(m)
				view := m.View()
				screen := strings.Join(checkRendered(t, view, size[0], size[1]), "\n")
				requireMonochromeChromeASCII(t, screen)
				if size == [2]int{200, 60} && s.name == "prompt" {
					for _, want := range []string{"e\u0301", "界", "👩‍💻"} {
						if !strings.Contains(screen, want) {
							t.Fatalf("prompt lost external Unicode %q:\n%s", want, screen)
						}
					}
				}
				if size == [2]int{80, 24} && s.name == "tasks-logs" && !strings.Contains(screen, "hello 界") {
					t.Fatalf("log content lost CJK while chrome became ASCII:\n%s", screen)
				}
			})
		}
	}
}

func TestMonochromeEmptyMarkersAndHintsAreASCII(t *testing.T) {
	for _, size := range [][2]int{{30, 10}, {80, 24}} {
		for _, screenName := range []string{"home", "launcher", "tasks", "notifications", "palette"} {
			t.Run(fmt.Sprintf("%s/%dx%d", screenName, size[0], size[1]), func(t *testing.T) {
				m := sized(NewWithTheme(t.Context(), noopCaller{}, nil, theme.Monochrome(), ui.Plain), size[0], size[1])
				switch screenName {
				case "home":
					m.setMode(modeHome)
				case "launcher":
					m.setMode(modeLauncher)
				case "tasks":
					m.taskView = true
					m.tasks.SetProcesses(nil)
				case "notifications":
					m.notifView = true
				case "palette":
					m.openPalette()
					m.paletteQuery = "no matching fixture"
				}
				screen := strings.Join(checkRendered(t, m.View(), size[0], size[1]), "\n")
				requireMonochromeChromeASCII(t, screen)
				if (size == [2]int{80, 24} || screenName == "home" || screenName == "launcher" || screenName == "palette") && !strings.Contains(screen, "[EMPTY]") {
					t.Fatalf("empty state has no ASCII empty marker:\n%s", screen)
				}
			})
		}
	}
}

func TestMonochromePresentationASCIIClippingPreservesAppUnicode(t *testing.T) {
	m := sized(NewWithTheme(t.Context(), noopCaller{}, nil, theme.Monochrome(), ui.Plain), 40, 24)
	data := padData()
	data.Blocks[0].Text = strings.Repeat("=", 60) + "\nVisible app content: 界 e\u0301 👩‍💻"
	m.presentation = &appView{data: data, instance: "launch", version: 2,
		values: map[string]string{"text": "draft"}, focus: 2, cell: 1}
	screen := strings.Join(checkRendered(t, m.View(), 40, 24), "\n")
	requireMonochromeChromeASCII(t, screen)
	for _, want := range []string{"Visible app content: 界 e\u0301 👩‍💻", "=====...", "Up/Dow..."} {
		if !strings.Contains(screen, want) {
			t.Fatalf("presentation lacks %q:\n%s", want, screen)
		}
	}
}

func TestPlainNonMonochromeShellChromeRemainsUnicode(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.setMode(modeHome)
	screen := strings.Join(screenText(m.View()), "\n")
	for _, want := range []string{"·", "↑↓"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("Nostalgia Plain unexpectedly lost existing Unicode chrome %q:\n%s", want, screen)
		}
	}
}
