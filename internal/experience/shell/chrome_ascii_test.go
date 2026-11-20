package shell

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

var monochromeOwnedUnicode = []string{"·", "↑", "↓", "←", "→", "…", "◇", "●", "─", "–", "—"}

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
	m.showWindow(&appView{data: data, instance: "launch", version: 2,
		values: map[string]string{"text": "draft"}, focus: 2, cell: 1})
	screen := strings.Join(checkRendered(t, m.View(), 40, 24), "\n")
	requireMonochromeChromeASCII(t, screen)
	for _, want := range []string{"Visible app content: 界 e\u0301 👩‍💻", "=====...", "Up/Dow..."} {
		if !strings.Contains(screen, want) {
			t.Fatalf("presentation lacks %q:\n%s", want, screen)
		}
	}
}

func TestMonochromePromptCommandChrome(t *testing.T) {
	caller := callerFunc(func(_ context.Context, method string, _, out any) error {
		var value any
		switch method {
		case "session/get":
			value = map[string]any{
				"id": "session-1", "active": true, "started_at": "2026-01-02T03:04:05Z",
				"user": map[string]any{"id": "u-1", "name": "用户 · →"},
			}
		case "app/list":
			value = []any{map[string]any{
				"manifest": map[string]any{"id": "com.test.unicode", "name": "界面 · →", "version": "1", "permissions": []string{"ipc"}},
				"running":  true, "pid": 7,
			}}
		case "proc/logs":
			value = map[string]any{
				"id": 7, "name": "进程 · →", "kind": "external", "state": "running",
				"stdout": map[string]any{"content": "输出 · → …\n", "total_bytes": 20},
				"stderr": map[string]any{},
			}
		default:
			return nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, out)
	})
	for _, tc := range []struct {
		line      string
		want      []string
		asciiOnly bool
	}{
		{line: "help", want: []string{"recents | favorites", "Up/Down history", "F1 home | F2 apps"}, asciiOnly: true},
		{line: "session", want: []string{"SESSION session-1 | User: 用户 · → (u-1)", "Started: 2026-01-02 03:04:05 | Active: true"}},
		{line: "apps", want: []string{"com.test.unicode  界面 · → v1", "LIVE | PID 7 | caps: ipc"}},
		{line: "logs 7", want: []string{"PROCESS 7 (进程 · →) | external | running", "stdout: 20B (dropped 0B) | stderr: 0B", "输出 · → …"}},
	} {
		t.Run(tc.line, func(t *testing.T) {
			m := NewWithTheme(t.Context(), caller, nil, theme.Monochrome(), ui.Plain)
			result, ok := m.submit(tc.line)().(resultMsg)
			if !ok || result.err != nil {
				t.Fatalf("command result = %#v, error %v", result, result.err)
			}
			for _, want := range tc.want {
				if !strings.Contains(result.text, want) {
					t.Errorf("command output lacks %q:\n%s", want, result.text)
				}
			}
			if tc.asciiOnly {
				requireMonochromeChromeASCII(t, result.text)
			}
			m.Update(result)
			lineCount := strings.Count(result.text, "\n") + 1
			appended := m.transcript[len(m.transcript)-lineCount:]
			lines := make([]string, 0, lineCount)
			for _, entry := range appended {
				lines = append(lines, entry.text)
			}
			if got := strings.Join(lines, "\n"); got != result.text {
				t.Fatalf("transcript changed command output:\n%q\nwant:\n%q", got, result.text)
			}
		})
	}

	m := NewWithTheme(t.Context(), caller, nil, theme.Nostalgia(), ui.Plain)
	result := m.submit("session")().(resultMsg)
	if !strings.Contains(result.text, "SESSION session-1 · User: 用户 · →") {
		t.Fatalf("non-Monochrome command lost Unicode chrome or content:\n%s", result.text)
	}
}

func TestMonochromeUnknownCommandChromePreservesInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme theme.Theme
		want  string
	}{
		{name: "Monochrome", theme: theme.Monochrome(), want: `unknown command "界面" - type help`},
		{name: "Nostalgia", theme: theme.Nostalgia(), want: `unknown command "界面" — type help`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithTheme(t.Context(), noopCaller{}, nil, tc.theme, ui.Plain)
			result, ok := m.submit("界面")().(resultMsg)
			if !ok || result.err == nil {
				t.Fatalf("unknown command result = %#v, error %v", result, result.err)
			}
			if got := result.err.Error(); got != tc.want {
				t.Fatalf("unknown command error = %q, want %q", got, tc.want)
			}
			m.Update(result)
			if got := m.transcript[len(m.transcript)-1].text; got != tc.want {
				t.Fatalf("transcript error = %q, want %q", got, tc.want)
			}
			if tc.name == "Monochrome" {
				requireMonochromeChromeASCII(t, m.transcript[len(m.transcript)-1].text)
			}
		})
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
