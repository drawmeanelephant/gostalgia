package e2e

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"gostalgia/internal/runtime"
)

const e2eOceanTheme = `{
	"name": "ocean",
	"description": "Deep sea palette",
	"palette": {
		"canvas": "#10202B", "surface": "#1A2E3C", "text": "#E8F1F2",
		"muted": "#8FA6AD", "accent": "#4FB3BF", "border": "#3E5C66",
		"header": "#14506B", "header_text": "#E8F1F2"
	}
}`

// A theme file dropped into /config/themes is validated, listed next to
// the built-ins, and persistable through the normal config layer — the
// same path the Settings app and the `theme` shell command take.
func TestUserLoadableThemesE2E(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	t.Cleanup(func() { rt.Shutdown("test finished") })

	client := dialRunning(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	writeFile := func(path, contents string) {
		t.Helper()
		must(t, client.Call(ctx, "fs/write", map[string]string{
			"path":        path,
			"data_base64": base64.StdEncoding.EncodeToString([]byte(contents)),
		}, nil))
	}
	writeFile("/config/themes/ocean.json", e2eOceanTheme)
	writeFile("/config/themes/broken.json", `{"name": "broken"`)

	var list struct {
		Themes []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"themes"`
		Skipped []struct {
			File string `json:"file"`
		} `json:"skipped"`
	}
	must(t, client.Call(ctx, "theme/list", nil, &list))
	foundUser := false
	foundBuiltins := 0
	for _, th := range list.Themes {
		if th.Source == "builtin" {
			foundBuiltins++
		}
		if th.Name == "ocean" && th.Source == "user" {
			foundUser = true
		}
	}
	if foundBuiltins != 5 {
		t.Fatalf("builtins listed = %d, want 5: %+v", foundBuiltins, list.Themes)
	}
	if !foundUser {
		t.Fatalf("user theme not listed: %+v", list.Themes)
	}
	if len(list.Skipped) != 1 || list.Skipped[0].File != "broken.json" {
		t.Fatalf("skipped = %+v, want broken.json", list.Skipped)
	}

	var get struct {
		Name       string `json:"name"`
		Source     string `json:"source"`
		Definition struct {
			Name    string `json:"name"`
			Palette struct {
				Accent string `json:"accent"`
			} `json:"palette"`
		} `json:"definition"`
	}
	must(t, client.Call(ctx, "theme/get", map[string]string{"name": "ocean"}, &get))
	if get.Definition.Name != "ocean" || get.Definition.Palette.Accent != "#4FB3BF" {
		t.Fatalf("theme/get = %+v", get)
	}

	// The user theme persists through the standard config layer, so it
	// survives a reboot and scopes to the active profile like a built-in.
	var setResp map[string]any
	must(t, client.Call(ctx, "config/set", map[string]any{
		"path":  "theme",
		"value": "ocean",
		"layer": "user",
	}, &setResp))

	var getResp struct {
		Value any `json:"value"`
	}
	must(t, client.Call(ctx, "config/get", map[string]string{"path": "theme"}, &getResp))
	if getResp.Value != "ocean" {
		t.Fatalf("theme = %v, want ocean", getResp.Value)
	}

	// And an invalid name is still rejected.
	if err := client.Call(ctx, "config/set", map[string]any{
		"path":  "theme",
		"value": "not-a-real-theme",
		"layer": "user",
	}, &setResp); err == nil {
		t.Fatal("config/set accepted an unknown theme")
	}
}
