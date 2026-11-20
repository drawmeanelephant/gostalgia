package theme

import (
	"image/color"
	"reflect"
	"testing"
)

func TestNamedThemes(t *testing.T) {
	themes := []Theme{Nostalgia(), Midnight(), Monochrome(), HighContrast(), HighContrastLight()}
	for _, th := range themes {
		t.Run(th.Name, func(t *testing.T) {
			if th.Palette.Canvas == th.Palette.Text || th.Palette.Surface == th.Palette.Text {
				t.Fatal("text and background need distinct colors")
			}
			if th.Spacing.PanelX != 1 || th.Spacing.Gap != 1 || th.Focus.Marker == "" {
				t.Fatal("missing layout or focus tokens")
			}
			for _, state := range []State{Normal, Disabled, Busy, Success, Error, Empty} {
				s := th.Status(state)
				if s.Text == nil || s.Fill == nil || s.Symbol == "" || s.Label == "" || s.Message == "" {
					t.Errorf("incomplete %s status: %+v", state, s)
				}
			}
			if th.Status("unknown") != th.States.Normal {
				t.Fatal("unknown state must use normal tokens")
			}
		})
	}
	if Nostalgia().Palette == Midnight().Palette {
		t.Fatal("named palettes must differ")
	}
	if Monochrome().Palette == HighContrast().Palette {
		t.Fatal("monochrome and high-contrast palettes must differ")
	}
}

// termenv picks an ANSI256 cube level as int((v-35)/40) in float64, and Go
// may fuse that arithmetic into one rounding (arm64 does, amd64 does not). A
// channel of 115, 155, 195 or 235 makes the quotient exactly an integer, so
// it can truncate to different levels on different machines, and no styled
// ANSI256 golden could match on both.
func TestBuiltinColorsDownsampleTheSameOnEveryArch(t *testing.T) {
	boundary := map[uint32]bool{115: true, 155: true, 195: true, 235: true}
	for _, th := range []Theme{Nostalgia(), Midnight(), Monochrome(), HighContrast(), HighContrastLight()} {
		eachColor(reflect.ValueOf(th), th.Name, func(path string, c color.Color) {
			r, g, b, _ := c.RGBA()
			for _, v := range []uint32{r >> 8, g >> 8, b >> 8} {
				if boundary[v] {
					t.Errorf("%s %v has channel %d on an ANSI256 cube boundary; move it one step", path, c, v)
				}
			}
		})
	}
}

func eachColor(v reflect.Value, path string, visit func(string, color.Color)) {
	switch v.Kind() {
	case reflect.Struct:
		for i := range v.NumField() {
			eachColor(v.Field(i), path+"."+v.Type().Field(i).Name, visit)
		}
	case reflect.Interface:
		if c, ok := v.Interface().(color.Color); ok && c != nil {
			visit(path, c)
		}
	}
}

func TestAccessibleThemes(t *testing.T) {
	mono := Monochrome()
	if !mono.ReducedMotion {
		t.Fatal("monochrome should have reduced motion enabled by default")
	}
	if mono.Borders.Panel.Top != "-" || mono.Borders.Panel.TopLeft != "+" {
		t.Fatal("monochrome should use ASCII border tokens")
	}
	if mono.Focus.Marker != ">" {
		t.Fatal("monochrome focus marker should be plain ASCII '>'")
	}
	if mono.States.Success.Symbol != "[OK]" || mono.States.Error.Symbol != "[FAIL]" || mono.States.Busy.Symbol != "[BUSY]" {
		t.Fatalf("monochrome must use text badges [OK], [FAIL], [BUSY], got: %+v", mono.States)
	}

	hc := HighContrast()
	if !hc.ReducedMotion {
		t.Fatal("high-contrast should have reduced motion enabled")
	}
	if hc.States.Success.Symbol != "[OK]" || hc.States.Error.Symbol != "[FAIL]" {
		t.Fatalf("high-contrast must have distinct text badge symbols, got: %+v", hc.States)
	}

	withMotion := mono.WithReducedMotion(false)
	if withMotion.ReducedMotion {
		t.Fatal("WithReducedMotion(false) did not disable reduced motion")
	}
}

func TestThemeIsAnIndependentValue(t *testing.T) {
	custom := Nostalgia()
	custom.Focus.Marker = "*"
	custom.Spacing.PanelX = 0
	custom.States.Busy.Label = "PLEASE WAIT"
	if got := Nostalgia(); got.Focus.Marker != "›" || got.Spacing.PanelX != 1 || got.States.Busy.Label != "WORKING" {
		t.Fatal("customizing a copy changed the default")
	}
}
