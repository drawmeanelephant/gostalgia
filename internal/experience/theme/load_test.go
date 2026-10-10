package theme

import (
	"testing"

	"gostalgia/internal/themedef"
)

func TestFromDefinitionCompleteTokens(t *testing.T) {
	def, err := themedef.Parse([]byte(`{
		"name": "ocean",
		"palette": {
			"canvas": "#10202B", "surface": "#1A2E3C", "text": "#E8F1F2",
			"muted": "#8FA6AD", "accent": "#4FB3BF", "border": "#3E5C66",
			"header": "#14506B", "header_text": "#E8F1F2"
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	th := FromDefinition(def)
	if th.Name != "ocean" {
		t.Fatalf("name = %q", th.Name)
	}
	if th.Palette.Canvas == "" || th.Palette.Text == "" || th.Palette.Accent == "" {
		t.Fatal("incomplete palette")
	}
	if th.Palette.Canvas == th.Palette.Text || th.Palette.Surface == th.Palette.Text {
		t.Fatal("text and background need distinct colors")
	}
	if th.Spacing.PanelX != 1 || th.Spacing.Gap != 1 {
		t.Fatalf("spacing = %+v", th.Spacing)
	}
	if th.Borders.Panel.Top == "" || th.Borders.Dialog.Top == "" {
		t.Fatal("border presets were not resolved to glyphs")
	}
	for _, state := range []State{Normal, Disabled, Busy, Success, Error, Empty} {
		s := th.Status(state)
		if s.Text == "" || s.Fill == "" || s.Symbol == "" || s.Label == "" || s.Message == "" {
			t.Errorf("incomplete %s status: %+v", state, s)
		}
	}
	if th.Progress.Filled == "" || th.Progress.Track == "" {
		t.Fatal("incomplete progress tokens")
	}
	if th.ReducedMotion {
		t.Fatal("reduced motion must default to false")
	}
}

func TestFromDefinitionCustomTokens(t *testing.T) {
	def, err := themedef.Parse([]byte(`{
		"name": "dots",
		"palette": {
			"canvas": "#FFFFFF", "surface": "#F5F5F5", "text": "#111111",
			"muted": "#666666", "accent": "#0055AA", "border": "#999999",
			"header": "#DDDDDD", "header_text": "#111111"
		},
		"spacing": {"panel_x": 2, "gap": 0},
		"borders": {
			"panel": "ascii",
			"dialog": {"top": "=", "bottom": "=", "left": "!", "right": "!",
				"top_left": "#", "top_right": "#", "bottom_left": "#", "bottom_right": "#"}
		},
		"typography": {"title_bold": false},
		"focus": {"marker": ">"},
		"progress": {"filled": "#", "track": "-"},
		"reduced_motion": true
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	th := FromDefinition(def)
	if th.Spacing.PanelX != 2 || th.Spacing.Gap != 0 {
		t.Fatalf("spacing = %+v", th.Spacing)
	}
	if th.Borders.Panel.Top != "-" || th.Borders.Panel.TopLeft != "+" {
		t.Fatalf("ascii preset did not produce ASCII glyphs: %+v", th.Borders.Panel)
	}
	if th.Borders.Dialog.Top != "=" || th.Borders.Dialog.Left != "!" || th.Borders.Dialog.BottomRight != "#" {
		t.Fatalf("explicit glyphs not honored: %+v", th.Borders.Dialog)
	}
	if th.Typography.TitleBold || !th.Typography.HeadingBold {
		t.Fatalf("typography = %+v", th.Typography)
	}
	if th.Focus.Marker != ">" {
		t.Fatalf("focus marker = %q", th.Focus.Marker)
	}
	if th.Progress.Filled != "#" || th.Progress.Track != "-" {
		t.Fatalf("progress = %+v", th.Progress)
	}
	if !th.ReducedMotion {
		t.Fatal("reduced_motion not honored")
	}
}

func TestFromDefinitionIsAnIndependentValue(t *testing.T) {
	def, err := themedef.Parse([]byte(`{
		"name": "ocean",
		"palette": {
			"canvas": "#10202B", "surface": "#1A2E3C", "text": "#E8F1F2",
			"muted": "#8FA6AD", "accent": "#4FB3BF", "border": "#3E5C66",
			"header": "#14506B", "header_text": "#E8F1F2"
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := FromDefinition(def)
	a.Focus.Marker = "*"
	b := FromDefinition(def)
	if b.Focus.Marker == "*" {
		t.Fatal("themes converted from one definition share mutable state")
	}
}
