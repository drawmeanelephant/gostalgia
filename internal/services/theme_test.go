package services

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/config"
	"gostalgia/internal/security"
)

const oceanTheme = `{
	"name": "ocean",
	"description": "Deep sea palette",
	"palette": {
		"canvas": "#10202B", "surface": "#1A2E3C", "text": "#E8F1F2",
		"muted": "#8FA6AD", "accent": "#4FB3BF", "border": "#3E5C66",
		"header": "#14506B", "header_text": "#E8F1F2"
	}
}`

func writeThemeFile(t *testing.T, env *testEnv, name, body string) {
	t.Helper()
	must(t, env.ctx.VFS.WriteFile("/config/themes/"+name, []byte(body), 0o644))
}

type themeListOut struct {
	Themes []struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	} `json:"themes"`
	Skipped []struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	} `json:"skipped"`
}

func (o themeListOut) names() []string {
	var out []string
	for _, th := range o.Themes {
		out = append(out, th.Name+":"+th.Source)
	}
	return out
}

func TestThemeListBuiltinsOnly(t *testing.T) {
	env := newTestEnv(t)
	resp := env.call(context.Background(), security.AdminCapabilities(), "theme/list", nil)
	if !resp.OK {
		t.Fatalf("theme/list failed: %s", resp.Error)
	}
	var out themeListOut
	must(t, json.Unmarshal(resp.Data, &out))
	want := []string{
		"high-contrast:builtin", "high-contrast-light:builtin", "midnight:builtin",
		"monochrome:builtin", "nostalgia:builtin",
	}
	got := out.names()
	if len(got) != len(want) {
		t.Fatalf("themes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("themes = %v, want %v", got, want)
		}
	}
	if len(out.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want empty", out.Skipped)
	}
}

func TestThemeListIncludesUserThemesAndSkips(t *testing.T) {
	env := newTestEnv(t)
	writeThemeFile(t, env, "ocean.json", oceanTheme)
	writeThemeFile(t, env, "broken.json", `{"name": "broken"`)
	writeThemeFile(t, env, "shadow.json", `{
		"name": "midnight",
		"palette": {
			"canvas": "#000000", "surface": "#000000", "text": "#FFFFFF",
			"muted": "#888888", "accent": "#FFFFFF", "border": "#444444",
			"header": "#111111", "header_text": "#FFFFFF"
		}
	}`)

	resp := env.call(context.Background(), security.AdminCapabilities(), "theme/list", nil)
	if !resp.OK {
		t.Fatalf("theme/list failed: %s", resp.Error)
	}
	var out themeListOut
	must(t, json.Unmarshal(resp.Data, &out))
	got := out.names()
	if len(got) != 6 || got[5] != "ocean:user" {
		t.Fatalf("themes = %v, want builtins + ocean:user", got)
	}
	if len(out.Skipped) != 2 {
		t.Fatalf("skipped = %+v, want broken.json and shadow.json", out.Skipped)
	}
}

func TestThemeGetUserTheme(t *testing.T) {
	env := newTestEnv(t)
	writeThemeFile(t, env, "ocean.json", oceanTheme)

	resp := env.call(context.Background(), security.AdminCapabilities(), "theme/get", map[string]string{"name": "Ocean"})
	if !resp.OK {
		t.Fatalf("theme/get failed: %s", resp.Error)
	}
	var out struct {
		Name       string `json:"name"`
		Source     string `json:"source"`
		File       string `json:"file"`
		Definition struct {
			Name    string `json:"name"`
			Palette struct {
				Accent string `json:"accent"`
			} `json:"palette"`
		} `json:"definition"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	if out.Name != "ocean" || out.Source != "user" || out.File != "ocean.json" {
		t.Fatalf("get = %+v", out)
	}
	if out.Definition.Palette.Accent != "#4FB3BF" {
		t.Fatalf("accent = %q", out.Definition.Palette.Accent)
	}
}

func TestThemeGetBuiltinAndUnknown(t *testing.T) {
	env := newTestEnv(t)
	admin := security.AdminCapabilities()
	if resp := env.call(context.Background(), admin, "theme/get", map[string]string{"name": "midnight"}); resp.OK {
		t.Fatal("theme/get on a built-in succeeded")
	}
	if resp := env.call(context.Background(), admin, "theme/get", map[string]string{"name": "ghost"}); resp.OK {
		t.Fatal("theme/get on an unknown theme succeeded")
	}
	if resp := env.call(context.Background(), admin, "theme/get", map[string]string{}); resp.OK {
		t.Fatal("theme/get without a name succeeded")
	}
}

func TestThemeRoutesRequireFileRead(t *testing.T) {
	env := newTestEnv(t)
	caps := security.NewCapabilities(security.CapIPC)
	for _, m := range []string{"theme/list", "theme/get"} {
		var params any
		if m == "theme/get" {
			params = map[string]string{"name": "x"}
		}
		if resp := env.call(context.Background(), caps, m, params); resp.OK {
			t.Fatalf("%s succeeded without fs.read", m)
		}
	}
}

func TestThemeValidatorAcceptsUserThemes(t *testing.T) {
	env := newTestEnv(t)
	writeThemeFile(t, env, "ocean.json", oceanTheme)

	if err := config.Validate("theme", "ocean"); err != nil {
		t.Fatalf("config rejected a valid user theme: %v", err)
	}
	if err := config.Validate("theme", "Ocean"); err != nil {
		t.Fatalf("config rejected a case-folded user theme: %v", err)
	}
	if err := config.Validate("theme", "not-a-theme"); err == nil {
		t.Fatal("config accepted an unknown theme")
	}
	if err := config.Validate("theme", "midnight"); err != nil {
		t.Fatalf("config rejected a built-in theme: %v", err)
	}
}

func TestThemeFilesNeverBlockServiceStart(t *testing.T) {
	env := newTestEnv(t)
	// Garbage in the directory must not break the already-running service
	// or any subsequent call.
	writeThemeFile(t, env, "trash.json", "this is not json at all {{{")
	writeThemeFile(t, env, "empty.json", "")
	resp := env.call(context.Background(), security.AdminCapabilities(), "theme/list", nil)
	if !resp.OK {
		t.Fatalf("theme/list failed with garbage files present: %s", resp.Error)
	}
}
