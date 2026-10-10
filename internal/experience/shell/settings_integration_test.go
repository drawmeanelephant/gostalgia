package shell

import (
	"context"
	"encoding/json"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

type settingsMockCaller struct {
	calls        []string
	effectiveCfg map[string]any
	apps         []appStatus
}

func (c *settingsMockCaller) Call(ctx context.Context, method string, params, out any) error {
	c.calls = append(c.calls, method)
	switch method {
	case "config/list":
		if out != nil {
			data, _ := json.Marshal(map[string]any{
				"effective": c.effectiveCfg,
			})
			_ = json.Unmarshal(data, out)
		}
		return nil
	case "app/list":
		if out != nil {
			data, _ := json.Marshal(c.apps)
			_ = json.Unmarshal(data, out)
		}
		return nil
	case "app/launch":
		for i := range c.apps {
			c.apps[i].Running = true
			c.apps[i].PID = 42
		}
		return nil
	case "app/com.gostalgia.settings/view":
		if out != nil {
			v := sdk.View{
				Title:    "Settings - Appearance & Theme",
				State:    sdk.ViewReady,
				Instance: "inst_1",
				Items: []sdk.Item{
					{ID: "theme_midnight", Label: "Midnight", Detail: "Cool slate"},
				},
				Actions: []sdk.Action{
					{ID: "select", Label: "Select / Toggle"},
				},
			}
			data, _ := json.Marshal(v)
			_ = json.Unmarshal(data, out)
		}
		return nil
	default:
		return nil
	}
}

func TestShellSettingsCommandAndF7Toggle(t *testing.T) {
	caller := &settingsMockCaller{
		effectiveCfg: map[string]any{
			"theme": "nostalgia",
			"accessibility": map[string]any{
				"color_mode":     "ansi256",
				"reduced_motion": false,
			},
			"startup": map[string]any{
				"view": "prompt",
			},
			"shortcuts": map[string]any{
				"settings": "f7",
			},
		},
		apps: []appStatus{
			{
				Manifest: struct {
					ID          string   `json:"id"`
					Name        string   `json:"name"`
					Version     string   `json:"version"`
					Description string   `json:"description"`
					Permissions []string `json:"permissions"`
				}{
					ID:          "com.gostalgia.settings",
					Name:        "Settings",
					Version:     "0.1.0",
					Description: "Preferences",
				},
				Running: false,
			},
		},
	}

	m := NewWithTheme(context.Background(), caller, nil, theme.Nostalgia(), ui.ANSI256)
	m.apps = caller.apps

	// 1. Launch settings via command
	res := execute(context.Background(), caller, "/users/guest", "settings")
	if res.switchView != "settings" {
		t.Fatalf("expected switchView=settings, got %q", res.switchView)
	}

	// 2. Open Settings via openAppByID
	cmd := m.openAppByID("com.gostalgia.settings")
	if cmd == nil {
		t.Fatal("expected non-nil cmd from openAppByID")
	}
	msg := cmd()
	// openAppMsg should be returned
	openMsg, ok := msg.(openAppMsg)
	if !ok {
		t.Fatalf("expected openAppMsg, got %T: %+v", msg, msg)
	}
	m.Update(openMsg)
	if m.presentation == nil || m.presentation.id != "com.gostalgia.settings" {
		t.Fatalf("expected presentation view for settings, got %+v", m.presentation)
	}

	// 3. F7 toggles off active Settings view
	_, cancelCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF7})
	if m.presentation != nil {
		t.Fatal("expected presentation to be dismissed on F7")
	}
	_ = cancelCmd
}

func TestShellLiveConfigUpdates(t *testing.T) {
	caller := &settingsMockCaller{
		effectiveCfg: map[string]any{
			"theme": "nostalgia",
		},
	}
	m := NewWithTheme(context.Background(), caller, nil, theme.Nostalgia(), ui.ANSI256)

	// Verify initial theme is Nostalgia
	if m.kit.Theme().Name != "nostalgia" {
		t.Fatalf("initial theme = %s, want nostalgia", m.kit.Theme().Name)
	}

	// 1. Live update theme to midnight
	m.Update(configUpdateMsg{
		theme: "midnight",
	})
	if m.kit.Theme().Name != "midnight" {
		t.Fatalf("theme after live update = %s, want midnight", m.kit.Theme().Name)
	}

	// 2. Live update color mode to plain / monochrome
	m.Update(configUpdateMsg{
		colorMode: "plain",
	})
	if m.kit.Mode() != ui.Plain {
		t.Fatalf("color mode after live update = %v, want plain", m.kit.Mode())
	}

	// 3. Live update reduced motion to true
	rmTrue := true
	m.Update(configUpdateMsg{
		reducedMotion: &rmTrue,
	})
	if !m.kit.Theme().ReducedMotion {
		t.Fatal("expected reduced motion to be true")
	}

	// 4. Live update custom shortcut
	m.Update(configUpdateMsg{
		shortcuts: map[string]string{
			"settings": "f9",
		},
	})
	if !m.isShortcut("settings", "f9") {
		t.Fatal("expected f9 to trigger settings")
	}
	if m.isShortcut("settings", "f7") {
		t.Fatal("f7 should no longer trigger settings when rebound to f9")
	}
}

func TestShellStartupViewConfiguration(t *testing.T) {
	caller := &settingsMockCaller{
		effectiveCfg: map[string]any{},
	}

	// 1. Startup view: launcher
	m1 := NewWithTheme(context.Background(), caller, nil, theme.Nostalgia(), ui.ANSI256)
	m1.Update(configUpdateMsg{
		startupView: "launcher",
	})
	if m1.currentMode() != modeLauncher {
		t.Fatalf("startup view mode = %v, want modeLauncher", m1.currentMode())
	}

	// 2. Startup view: prompt
	m2 := NewWithTheme(context.Background(), caller, nil, theme.Nostalgia(), ui.ANSI256)
	m2.Update(configUpdateMsg{
		startupView: "prompt",
	})
	if m2.currentMode() != modePrompt {
		t.Fatalf("startup view mode = %v, want modePrompt", m2.currentMode())
	}
}
