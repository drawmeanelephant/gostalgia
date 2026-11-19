package shell

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// suggestCaller answers app/list from the fixture and records every method
// the shell calls, so a test can prove a suggestion was only displayed.
type suggestCaller struct {
	apps       []appStatus
	appListErr error
	calls      []string
}

func (c *suggestCaller) Call(_ context.Context, method string, _, out any) error {
	c.calls = append(c.calls, method)
	if method != "app/list" {
		return nil
	}
	if c.appListErr != nil {
		return c.appListErr
	}
	data, err := json.Marshal(c.apps)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *suggestCaller) called(method string) bool {
	for _, name := range c.calls {
		if name == method {
			return true
		}
	}
	return false
}

func suggestMessage(t *testing.T, caller Caller, line string) string {
	t.Helper()
	m := NewWithTheme(t.Context(), caller, nil, theme.Nostalgia(), ui.ANSI256)
	result, ok := m.submit(line)().(resultMsg)
	if !ok || result.err == nil {
		t.Fatalf("submit(%q) = %#v, error %v; want unknown-command error", line, result, result.err)
	}
	return result.err.Error()
}

func TestUnknownCommandSuggestsExplicitAppLaunch(t *testing.T) {
	caller := &suggestCaller{apps: []appStatus{
		readyApp("com.gostalgia.delve", "Delve"),
		readyApp("com.gostalgia.echo", "Echo"),
	}}
	for _, line := range []string{"delve", "Delve", "DELVE", "com.gostalgia.delve", "COM.GOSTALGIA.DELVE"} {
		t.Run(line, func(t *testing.T) {
			got := suggestMessage(t, caller, line)
			if !strings.Contains(got, "launch com.gostalgia.delve") {
				t.Fatalf("unknown %q hint = %q, want explicit launch suggestion", line, got)
			}
			if strings.Contains(got, "launch com.gostalgia.echo") {
				t.Fatalf("unknown %q hint suggested an unrelated app: %q", line, got)
			}
			if !strings.Contains(got, "type help") {
				t.Fatalf("unknown %q hint lost the help fallback: %q", line, got)
			}
		})
	}
	if caller.called("app/launch") {
		t.Fatalf("suggestion executed a launch: calls = %v", caller.calls)
	}
}

func TestUnknownCommandSuggestsNearCommandNames(t *testing.T) {
	for _, tc := range []struct {
		line string
		want string
	}{
		{line: "hep", want: "help"},
		{line: "thme", want: "theme"},
		{line: "exi", want: "exit"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			got := suggestMessage(t, &suggestCaller{}, tc.line)
			if !strings.Contains(got, "did you mean:") || !strings.Contains(got, tc.want) {
				t.Fatalf("unknown %q hint = %q, want a %q suggestion", tc.line, got, tc.want)
			}
		})
	}
}

func TestUnknownCommandDistantInputKeepsHelpFallback(t *testing.T) {
	for _, line := range []string{"zzzzzzzzzzzz", "qqqqqqqq", "x"} {
		t.Run(line, func(t *testing.T) {
			got := suggestMessage(t, &suggestCaller{}, line)
			if strings.Contains(got, "did you mean:") {
				t.Fatalf("distant %q produced a suggestion: %q", line, got)
			}
			if !strings.HasSuffix(got, "type help") {
				t.Fatalf("distant %q hint = %q, want the help fallback", line, got)
			}
		})
	}
}

func TestUnknownCommandSuggestionsAreBoundedAndDeterministic(t *testing.T) {
	caller := &suggestCaller{}
	first := suggestMessage(t, caller, "cs")
	second := suggestMessage(t, caller, "cs")
	if first != second {
		t.Fatalf("suggestions are not deterministic:\n%q\n%q", first, second)
	}
	_, suggestions, ok := strings.Cut(first, "did you mean: ")
	if !ok {
		t.Fatalf("no suggestion in %q", first)
	}
	suggestions, _, _ = strings.Cut(suggestions, "? (type help)")
	parts := strings.Split(suggestions, ", ")
	if len(parts) == 0 || len(parts) > maxUnknownCommandSuggestions {
		t.Fatalf("suggestions = %q, want 1..%d", parts, maxUnknownCommandSuggestions)
	}
	for _, name := range parts {
		m := NewWithTheme(t.Context(), noopCaller{}, nil, theme.Nostalgia(), ui.ANSI256)
		result := m.submit(name)().(resultMsg)
		if result.err != nil && strings.Contains(result.err.Error(), "unknown command") {
			t.Fatalf("suggested %q is not an accepted command: %v", name, result.err)
		}
	}
}

func TestUnknownCommandSuggestsThroughTiedNames(t *testing.T) {
	got := suggestMessage(t, &suggestCaller{}, "cs")
	if want := "did you mean: cd, cls, ls? (type help)"; !strings.Contains(got, want) {
		t.Fatalf("tied suggestions = %q, want deterministic %q", got, want)
	}
}

func TestUnknownCommandSuggestionsSurviveRegistryFailure(t *testing.T) {
	caller := &suggestCaller{appListErr: errors.New("app/list: socket closed")}
	if got := suggestMessage(t, caller, "hep"); !strings.Contains(got, "did you mean: help") {
		t.Fatalf("registry failure lost command suggestion: %q", got)
	}
	if got := suggestMessage(t, caller, "delve"); strings.Contains(got, "did you mean:") || !strings.HasSuffix(got, "type help") {
		t.Fatalf("registry failure hint = %q, want the help fallback", got)
	}
	if caller.called("app/launch") {
		t.Fatalf("registry failure executed a launch: calls = %v", caller.calls)
	}
}

func TestUnknownCommandHintsSanitizedAndMonochromeASCII(t *testing.T) {
	hostileID := "com.test.\x1b[31m界"
	caller := &suggestCaller{apps: []appStatus{
		readyApp(hostileID, "Hostile\x1b[2JName"),
	}}
	got := suggestMessage(t, caller, hostileID)
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("hint retained terminal controls: %q", got)
	}
	if !strings.Contains(got, "launch com.test.界") {
		t.Fatalf("sanitized hint lost the app ID: %q", got)
	}
	_, suggestion, _ := strings.Cut(got, "did you mean: ")
	if strings.ContainsAny(suggestion, "\x1b\a") {
		t.Fatalf("suggestion retained registry controls: %q", suggestion)
	}

	input := "ev\x1b[31mil"
	got = suggestMessage(t, &suggestCaller{}, input)
	if strings.ContainsRune(got, '\x1b') {
		t.Fatalf("hint retained submitted controls: %q", got)
	}

	m := NewWithTheme(t.Context(), caller, nil, theme.Monochrome(), ui.Plain)
	result := m.submit(hostileID)().(resultMsg)
	if result.err == nil {
		t.Fatal("hostile app ID unexpectedly resolved as a command")
	}
	message := result.err.Error()
	if !strings.Contains(message, "launch com.test.界") ||
		!strings.Contains(message, " - did you mean: ") || !strings.Contains(message, "? (type help)") {
		t.Fatalf("Monochrome hint = %q", message)
	}
	requireMonochromeChromeASCII(t, message)
}

func TestUnknownCommandHintsReadableInEveryThemeAndPlain(t *testing.T) {
	for _, tc := range []struct {
		name  string
		theme theme.Theme
		mode  ui.ColorMode
	}{
		{name: "Nostalgia", theme: theme.Nostalgia(), mode: ui.ANSI256},
		{name: "Midnight", theme: theme.Midnight(), mode: ui.ANSI256},
		{name: "Monochrome", theme: theme.Monochrome(), mode: ui.Plain},
		{name: "HighContrast", theme: theme.HighContrast(), mode: ui.ANSI256},
		{name: "HighContrastLight", theme: theme.HighContrastLight(), mode: ui.ANSI256},
		{name: "Plain", theme: theme.Nostalgia(), mode: ui.Plain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewWithTheme(t.Context(), &suggestCaller{}, nil, tc.theme, tc.mode)
			result := m.submit("hep")().(resultMsg)
			if result.err == nil || !strings.Contains(result.err.Error(), "help") {
				t.Fatalf("%s hint = %v, want the help suggestion", tc.name, result.err)
			}
		})
	}
}

func TestAcceptedCommandsStillExecute(t *testing.T) {
	for _, line := range []string{"help", "ps", "session"} {
		m := NewWithTheme(t.Context(), noopCaller{}, nil, theme.Nostalgia(), ui.ANSI256)
		result := m.submit(line)().(resultMsg)
		if result.err != nil && strings.Contains(result.err.Error(), "unknown command") {
			t.Fatalf("accepted %q became an unknown-command hint: %v", line, result.err)
		}
	}
}
