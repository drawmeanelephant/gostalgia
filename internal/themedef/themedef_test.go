package themedef

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

const minimalTheme = `{
	"name": "ocean",
	"palette": {
		"canvas": "#10202B", "surface": "#1A2E3C", "text": "#E8F1F2",
		"muted": "#8FA6AD", "accent": "#4FB3BF", "border": "#3E5C66",
		"header": "#14506B", "header_text": "#E8F1F2"
	}
}`

func TestParseMinimalThemeGetsDefaults(t *testing.T) {
	def, err := Parse([]byte(minimalTheme))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if def.Name != "ocean" {
		t.Fatalf("name = %q", def.Name)
	}
	if def.Palette.Accent != "#4FB3BF" {
		t.Fatalf("accent = %q", def.Palette.Accent)
	}
	// Omitted sections default the way the built-in colored themes do.
	if def.Spacing.PanelX != 1 || def.Spacing.Gap != 1 {
		t.Fatalf("spacing = %+v", def.Spacing)
	}
	if def.Borders.Panel.Preset != "thick" || def.Borders.Dialog.Preset != "double" {
		t.Fatalf("borders = %+v", def.Borders)
	}
	if !def.Typography.TitleBold || !def.Typography.HeadingBold || !def.Typography.KeyBold {
		t.Fatalf("typography = %+v", def.Typography)
	}
	if def.Focus.Text != def.Palette.Surface || def.Focus.Border != def.Palette.Accent || def.Focus.Marker != "›" {
		t.Fatalf("focus = %+v", def.Focus)
	}
	if def.States.Success.Text == "" || def.States.Busy.Label != "WORKING" || def.States.Empty.Symbol != "◇" {
		t.Fatalf("states = %+v", def.States)
	}
	if def.Progress.Filled != "━" || def.Progress.Track != "·" {
		t.Fatalf("progress = %+v", def.Progress)
	}
	if def.ReducedMotion {
		t.Fatal("reduced_motion must default to false")
	}
}

func TestParseFullTheme(t *testing.T) {
	def, err := Parse([]byte(`{
		"name": "paper",
		"description": "Light print theme",
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
		"typography": {"title_bold": false, "heading_bold": true, "key_bold": true},
		"focus": {"text": "#FFFFFF", "fill": "#0055AA", "border": "#0055AA", "marker": ">"},
		"states": {
			"error": {"text": "#AA0000", "fill": "#FFFFFF", "symbol": "[FAIL]", "label": "FAIL", "message": "It broke."}
		},
		"progress": {"filled": "#", "track": "-"},
		"reduced_motion": true
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if def.Description != "Light print theme" {
		t.Fatalf("description = %q", def.Description)
	}
	if def.Spacing.PanelX != 2 || def.Spacing.Gap != 0 {
		t.Fatalf("spacing = %+v", def.Spacing)
	}
	if def.Borders.Panel.Preset != "ascii" {
		t.Fatalf("panel border = %+v", def.Borders.Panel)
	}
	if def.Borders.Dialog.Glyphs == nil || def.Borders.Dialog.Glyphs.Top != "=" || def.Borders.Dialog.Glyphs.BottomRight != "#" {
		t.Fatalf("dialog glyphs = %+v", def.Borders.Dialog.Glyphs)
	}
	if def.Typography.TitleBold {
		t.Fatal("explicit title_bold false was overridden")
	}
	if def.Focus.Marker != ">" {
		t.Fatalf("focus marker = %q", def.Focus.Marker)
	}
	if def.States.Error.Symbol != "[FAIL]" || def.States.Error.Text != "#AA0000" {
		t.Fatalf("error state = %+v", def.States.Error)
	}
	// Untouched states still get palette-derived defaults.
	if def.States.Normal.Label != "READY" || def.States.Normal.Text != "#111111" {
		t.Fatalf("normal state = %+v", def.States.Normal)
	}
	if !def.ReducedMotion {
		t.Fatal("reduced_motion not honored")
	}
}

func TestParseRejectsInvalidDocuments(t *testing.T) {
	cases := map[string]string{
		"not json":           `{`,
		"unknown field":      `{"name":"x","palette":{},"bogus":1}`,
		"missing name":       `{"palette":{"canvas":"#000000","surface":"#111111","text":"#FFFFFF","muted":"#777777","accent":"#FFAA00","border":"#333333","header":"#222222","header_text":"#FFFFFF"}}`,
		"bad name":           strings.Replace(minimalTheme, `"ocean"`, `"Ocean!"`, 1),
		"empty name":         strings.Replace(minimalTheme, `"ocean"`, `""`, 1),
		"missing palette":    `{"name":"nope"}`,
		"incomplete palette": strings.Replace(minimalTheme, `"accent": "#4FB3BF", `, ``, 1),
		"bad color":          strings.Replace(minimalTheme, `"#4FB3BF"`, `"bluish"`, 1),
		"hex out of range":   strings.Replace(minimalTheme, `"#4FB3BF"`, `"#GGG"`, 1),
		"ansi out of range":  strings.Replace(minimalTheme, `"#4FB3BF"`, `"999"`, 1),
		"negative spacing":   strings.Replace(minimalTheme, "\"header_text\": \"#E8F1F2\"\n\t}", "\"header_text\": \"#E8F1F2\"\n\t}, \"spacing\": {\"panel_x\": -1}", 1),
		"huge spacing":       strings.Replace(minimalTheme, "\"header_text\": \"#E8F1F2\"\n\t}", "\"header_text\": \"#E8F1F2\"\n\t}, \"spacing\": {\"gap\": 99}", 1),
		"bad border preset":  strings.Replace(minimalTheme, "\"header_text\": \"#E8F1F2\"\n\t}", "\"header_text\": \"#E8F1F2\"\n\t}, \"borders\": {\"panel\": \"wiggly\"}", 1),
		"glyph too long":     strings.Replace(minimalTheme, "\"header_text\": \"#E8F1F2\"\n\t}", "\"header_text\": \"#E8F1F2\"\n\t}, \"focus\": {\"marker\": \"\u203a\u203a\u203a\u203a\u203a\u203a\u203a\u203a\u203a\"}", 1),
		"label too long":     strings.Replace(minimalTheme, "\"header_text\": \"#E8F1F2\"\n\t}", "\"header_text\": \"#E8F1F2\"\n\t}, \"states\": {\"busy\": {\"label\": \"TOOLONGTOOLONGTOOLONGTOOLONGTOOLONG\"}}", 1),
		"long name":          strings.Replace(minimalTheme, `"ocean"`, `"`+strings.Repeat("a", maxNameLen+1)+`"`, 1),
		"not utf8":           string([]byte{0xff, 0xfe, 0x00}),
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err == nil {
				t.Fatalf("expected %s to fail validation", name)
			}
		})
	}
}

func TestParseColorForms(t *testing.T) {
	for _, c := range []string{"#FFF", "#FFFF", "#4FB3BF", "#4FB3BFAA", "0", "15", "255"} {
		doc := strings.Replace(minimalTheme, `"#4FB3BF"`, `"`+c+`"`, 1)
		if _, err := Parse([]byte(doc)); err != nil {
			t.Errorf("color %q rejected: %v", c, err)
		}
	}
}

func TestLoadDir(t *testing.T) {
	fsys := fstest.MapFS{
		"config/themes/ocean.json":  {Data: []byte(minimalTheme)},
		"config/themes/paper.json":  {Data: []byte(strings.Replace(minimalTheme, `"ocean"`, `"paper"`, 1))},
		"config/themes/broken.json": {Data: []byte(`{"name": "broken"`)},
		"config/themes/dup.json":    {Data: []byte(strings.Replace(minimalTheme, `"ocean"`, `"paper"`, 1))},
		"config/themes/notes.txt":   {Data: []byte("not a theme")},
		"config/themes/subdir":      {Mode: fs.ModeDir},
	}
	defs, skips, err := LoadDir(fsys, Dir, nil)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("defs = %+v", defs)
	}
	if defs[0].Name != "ocean" || defs[1].Name != "paper" {
		t.Fatalf("defs not sorted by name: %+v", defs)
	}
	if defs[0].Source != "ocean.json" {
		t.Fatalf("ocean source = %q", defs[0].Source)
	}
	// dup.json sorts before paper.json, so it wins the "paper" name.
	if defs[1].Source != "dup.json" {
		t.Fatalf("paper source = %q", defs[1].Source)
	}
	if len(skips) != 2 {
		t.Fatalf("skips = %+v", skips)
	}
	files := map[string]string{}
	for _, sk := range skips {
		files[sk.File] = sk.Reason
	}
	if !strings.Contains(files["broken.json"], "JSON") {
		t.Errorf("broken.json reason = %q", files["broken.json"])
	}
	if !strings.Contains(files["paper.json"], "already defined") {
		t.Errorf("paper.json reason = %q", files["paper.json"])
	}
}

func TestLoadDirMissingDirIsEmpty(t *testing.T) {
	defs, skips, err := LoadDir(fstest.MapFS{}, Dir, nil)
	if err != nil || len(defs) != 0 || len(skips) != 0 {
		t.Fatalf("missing dir = %v, %v, %v", defs, skips, err)
	}
}

func TestLoadDirReservedNames(t *testing.T) {
	fsys := fstest.MapFS{
		"config/themes/nostalgia.json": {Data: []byte(strings.Replace(minimalTheme, `"ocean"`, `"nostalgia"`, 1))},
		"config/themes/ocean.json":     {Data: []byte(minimalTheme)},
	}
	reserved := map[string]bool{"nostalgia": true}
	defs, skips, err := LoadDir(fsys, Dir, reserved)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "ocean" {
		t.Fatalf("defs = %+v", defs)
	}
	if len(skips) != 1 || !strings.Contains(skips[0].Reason, "reserved") {
		t.Fatalf("skips = %+v", skips)
	}
}

func TestLoadDirBoundsFiles(t *testing.T) {
	fsys := fstest.MapFS{}
	for i := 0; i < MaxFiles+10; i++ {
		base := fmt.Sprintf("t%02d", i)
		doc := strings.Replace(minimalTheme, `"ocean"`, `"`+base+`"`, 1)
		fsys["config/themes/"+base+".json"] = &fstest.MapFile{Data: []byte(doc)}
	}
	defs, skips, err := LoadDir(fsys, Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != MaxFiles {
		t.Fatalf("loaded %d defs, want bounded to %d", len(defs), MaxFiles)
	}
	if len(skips) != 10 {
		t.Fatalf("skips = %+v", skips)
	}
}

func TestLoadDirSkipsOversizedFile(t *testing.T) {
	big := make([]byte, MaxFileSize+1)
	copy(big, []byte(minimalTheme))
	fsys := fstest.MapFS{
		"config/themes/big.json":   {Data: big},
		"config/themes/ocean.json": {Data: []byte(minimalTheme)},
	}
	defs, skips, err := LoadDir(fsys, Dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(defs) != 1 || defs[0].Name != "ocean" {
		t.Fatalf("defs = %+v", defs)
	}
	if len(skips) != 1 || skips[0].File != "big.json" || !strings.Contains(skips[0].Reason, "exceeds") {
		t.Fatalf("skips = %+v", skips)
	}
}
