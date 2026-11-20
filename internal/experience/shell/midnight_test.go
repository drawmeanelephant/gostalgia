package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// TestMidnightStyledChromeSnapshots keeps actual styled shell output, not a
// stripped-text golden: the header row, an accent heading and the selected
// Home row carry their foreground/background escapes in both profiles.
func TestMidnightStyledChromeSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode ui.ColorMode
	}{
		{"ansi256", ui.ANSI256},
		{"truecolor", ui.TrueColor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(fixtureModel(theme.Midnight(), tc.mode), 80, 24)
			m.setMode(modeHome)
			rects := checkFrame(t, m)
			view := m.View()
			checkRendered(t, view, 80, 24)
			if tc.mode == ui.ANSI256 && truecolorRE.MatchString(view.Content) {
				t.Fatal("ANSI256 render leaked truecolor sequences")
			}
			lines := strings.Split(view.Content, "\n")
			header := lines[rects[slotTabs].Min.Y]
			var heading, selected string
			for _, line := range lines[rects[slotBody].Min.Y:rects[slotBody].Max.Y] {
				text := ansi.Strip(line)
				if heading == "" && strings.Contains(text, "SYSTEM ACTIVITY") {
					heading = line
				}
				if selected == "" && strings.Contains(text, "Echo") {
					selected = line
				}
			}
			if heading == "" || selected == "" {
				t.Fatalf("Home fixture lacks a heading or selected app:\n%s", ansi.Strip(view.Content))
			}
			for name, line := range map[string]string{"header": header, "heading": heading, "selected": selected} {
				if !strings.Contains(line, "\x1b[") {
					t.Fatalf("%s line has no styled colors: %q", name, line)
				}
			}
			got := strings.Join([]string{
				"HEADER", header, "HEADING", heading, "SELECTED", selected,
			}, "\n") + "\n"
			path := filepath.Join("testdata", "midnight-styled-"+tc.name+".golden")
			if *updateVisual {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Fatalf("styled Midnight chrome changed:\n%s", got)
			}
		})
	}
}
