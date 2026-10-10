package shell

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

var updateVisual = flag.Bool("update-visual", false, "update shell visual golden snapshots")

func TestShellVisualSnapshots(t *testing.T) {
	for _, view := range []string{"home", "shelf", "empty", "busy", "error", "tasks", "notifications", "app", "app-loading", "app-error", "app-disabled"} {
		t.Run(view, func(t *testing.T) {
			m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
			if view == "shelf" || view == "empty" {
				m.shelf = true
			}
			if view == "tasks" {
				m.taskView = true
			}
			if view == "notifications" {
				m.notifView = true
			}
			if view == "shelf" {
				var app appStatus
				app.Manifest.Name = "Echo"
				app.Manifest.ID = "com.gostalgia.echo"
				app.Manifest.Version = "0.1.0"
				app.Manifest.Description = "A friendly first app."
				app.Manifest.Permissions = []string{"ipc"}
				app.Running, app.PID = true, 1
				m.apps = []appStatus{app}
			}
			m.busy = view == "busy"
			if view == "error" {
				m.append(entry{"The app couldn't start. Try again.", "error"})
			}
			if strings.HasPrefix(view, "app") {
				data := screenData()
				m.presentation = &appView{
					data: data, instance: data.Instance, values: map[string]string{"text": "A thought worth keeping."},
				}
				switch view {
				case "app-loading":
					m.presentation.data.State = sdk.ViewLoading
					m.presentation.busy = true
				case "app-error":
					m.presentation.data.State = sdk.ViewError
					m.presentation.data.Error = "Unable to send. Try again."
				case "app-disabled":
					m.presentation.data.Actions[0].Disabled = true
					m.presentation.focus = 1
				}
			}
			rendered := strings.Split(m.View().Content, "\n")
			for i := range rendered {
				rendered[i] = strings.TrimRight(rendered[i], " ")
			}
			got := strings.Join(rendered, "\n") + "\n"
			path := filepath.Join("testdata", view+".golden")
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
				t.Fatalf("shell visual snapshot changed:\n%s", got)
			}
		})
	}
}

// TestPresentationV2Snapshots pins the version-2 elements across theme
// vocabularies and a small screen: ASCII-only monochrome, high-contrast
// markers, and honest clipping when the pad doesn't fit.
func TestPresentationV2Snapshots(t *testing.T) {
	for _, tc := range []struct {
		name          string
		theme         theme.Theme
		width, height int
		mutate        func(*sdk.View)
		setup         func(*appView)
		want          string
	}{
		{"app-v2", theme.Nostalgia(), 80, 24, nil, nil, ""},
		{"app-v2-monochrome", theme.Monochrome(), 80, 24, nil, nil, ""},
		{"app-v2-highcontrast", theme.HighContrast(), 80, 24, nil, nil, ""},
		{"app-v2-small", theme.Nostalgia(), 30, 10, nil, nil, ""},
		// An unfocused grid keeps a theme marker on the selected cell.
		{"app-v2-mono-unfocused", theme.Monochrome(), 80, 24, nil,
			func(v *appView) { v.focus = 1 }, "> [NE]"},
		// A focused cursor on a disabled cell stays visible: marker + dimmed.
		{"app-v2-mono-disabled-cell", theme.Monochrome(), 80, 24, nil,
			func(v *appView) { v.cell = 3 }, "> [SE] (disabled)"},
		// Art wider than the viewport clips honestly with an ellipsis.
		{"app-v2-clip", theme.Monochrome(), 40, 24, func(v *sdk.View) {
			v.Blocks[0].Text = strings.Repeat("=", 60) + "\n" + v.Blocks[0].Text
		}, nil, "=====…"},
		// A focused grid keeps its selection marker on a small screen.
		{"app-v2-grid-small", theme.Nostalgia(), 40, 18, nil, nil, "› [NE]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithTheme(context.Background(), noopCaller{}, nil, tc.theme, ui.Plain)
			m.Update(tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			data := padData()
			if tc.mutate != nil {
				tc.mutate(&data)
			}
			v := &appView{
				data: data, instance: "launch", version: 2,
				values: map[string]string{"text": "draft"}, focus: 2, cell: 1,
			}
			if tc.setup != nil {
				tc.setup(v)
			}
			m.presentation = v
			rendered := strings.Split(m.View().Content, "\n")
			for i := range rendered {
				rendered[i] = strings.TrimRight(rendered[i], " ")
			}
			got := strings.Join(rendered, "\n") + "\n"
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("%s lacks %q:\n%s", tc.name, tc.want, got)
			}
			path := filepath.Join("testdata", tc.name+".golden")
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
				t.Fatalf("version-2 visual snapshot changed:\n%s", got)
			}
		})
	}
	// Themes change color, not layout: ANSI-stripped output must be identical.
	var stripped []string
	for _, th := range []theme.Theme{theme.Nostalgia(), theme.Midnight()} {
		m := NewWithTheme(context.Background(), noopCaller{}, nil, th, ui.TrueColor)
		m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
		m.presentation = &appView{
			data: padData(), instance: "launch", version: 2, values: map[string]string{"text": "draft"},
		}
		stripped = append(stripped, ansi.Strip(m.View().Content))
	}
	if stripped[0] != stripped[1] {
		t.Fatal("theme selection changed version-2 layout")
	}
}

func TestShellExplicitThemeAndViewport(t *testing.T) {
	for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256, ui.TrueColor} {
		warm := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), mode)
		dark := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Midnight(), mode)
		if ansi.Strip(warm.View().Content) != ansi.Strip(dark.View().Content) {
			t.Fatal("theme selection changed shell layout")
		}
		if mode == ui.Plain && strings.Contains(warm.View().Content, "\x1b") {
			t.Fatal("plain shell contains ANSI")
		}
		if mode != ui.Plain && warm.View().Content == dark.View().Content {
			t.Fatal("shell ignored explicit palette")
		}
		warm.cwd = "/users/guest/" + strings.Repeat("界e\u0301👩‍💻/", 50)
		warm.input = []rune(strings.Repeat("e\u0301👩‍💻界", 100))
		warm.cursor = len(warm.input)
		for _, shelf := range []bool{false, true} {
			warm.shelf = shelf
			for _, size := range []ui.Bounds{
				{Width: 0, Height: 0}, {Width: 1, Height: 1}, {Width: 20, Height: 8},
				{Width: 30, Height: 10}, {Width: 80, Height: 24}, {Width: 120, Height: 40}, {Width: 240, Height: 80},
			} {
				warm.Update(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
				view := warm.View().Content
				if size.Width == 0 || size.Height == 0 {
					if view != "" {
						t.Fatal("zero viewport was not empty")
					}
					continue
				}
				if lipgloss.Width(view) != size.Width || lipgloss.Height(view) != size.Height {
					t.Fatalf("shell dimensions = %dx%d, want %dx%d", lipgloss.Width(view), lipgloss.Height(view), size.Width, size.Height)
				}
			}
		}
	}
}

func TestShellAccessibleSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme theme.Theme
		mode  ui.ColorMode
		setup func(*Model)
	}{
		{
			name:  "monochrome-prompt",
			theme: theme.Monochrome(),
			mode:  ui.Plain,
			setup: func(m *Model) {
				m.setMode(modePrompt)
				m.append(entry{"Monochrome accessible mode active.", "accent"})
			},
		},
		{
			name:  "monochrome-home",
			theme: theme.Monochrome(),
			mode:  ui.Plain,
			setup: func(m *Model) {
				m.setMode(modeHome)
			},
		},
		{
			name:  "high-contrast-prompt",
			theme: theme.HighContrast(),
			mode:  ui.ANSI256,
			setup: func(m *Model) {
				m.setMode(modePrompt)
				m.append(entry{"High-contrast dark mode active.", "accent"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithTheme(context.Background(), noopCaller{}, nil, tc.theme, tc.mode)
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			if tc.setup != nil {
				tc.setup(m)
			}
			view := m.View().Content
			rendered := strings.Split(view, "\n")
			for i := range rendered {
				rendered[i] = strings.TrimRight(rendered[i], " ")
			}
			got := strings.Join(rendered, "\n") + "\n"
			path := filepath.Join("testdata", tc.name+".golden")
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
				t.Fatalf("accessible shell snapshot changed:\n%s", got)
			}
		})
	}
}

func TestShellAccessibleModesAndViewport(t *testing.T) {
	accessibleThemes := []theme.Theme{theme.Monochrome(), theme.HighContrast(), theme.HighContrastLight()}
	for _, th := range accessibleThemes {
		t.Run(th.Name, func(t *testing.T) {
			mode := ui.ANSI256
			if th.Name == "monochrome" {
				mode = ui.Plain
			}
			m := NewWithTheme(context.Background(), noopCaller{}, nil, th, mode)
			if !m.Theme().ReducedMotion {
				t.Fatalf("theme %s expected to have reduced motion enabled", th.Name)
			}
			for _, size := range []ui.Bounds{
				{Width: 0, Height: 0}, {Width: 1, Height: 1}, {Width: 20, Height: 8},
				{Width: 30, Height: 10}, {Width: 80, Height: 24}, {Width: 120, Height: 40}, {Width: 240, Height: 80},
			} {
				m.Update(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
				for _, viewMode := range []string{"home", "launcher", "prompt", "tasks", "notifications"} {
					m.taskView = viewMode == "tasks"
					m.notifView = viewMode == "notifications"
					switch viewMode {
					case "home":
						m.setMode(modeHome)
					case "launcher":
						m.setMode(modeLauncher)
					case "prompt":
						m.setMode(modePrompt)
					}
					view := m.View().Content
					if size.Width == 0 || size.Height == 0 {
						if view != "" {
							t.Fatalf("expected empty for %dx%d, got %q", size.Width, size.Height, view)
						}
						continue
					}
					if lipgloss.Width(view) != size.Width || lipgloss.Height(view) != size.Height {
						t.Fatalf("%s in %s at %dx%d: rendered %dx%d", th.Name, viewMode, size.Width, size.Height, lipgloss.Width(view), lipgloss.Height(view))
					}
					if mode == ui.Plain && strings.Contains(view, "\x1b") {
						t.Fatalf("plain mode contains ANSI sequences in %s", viewMode)
					}
				}
			}
		})
	}
}
