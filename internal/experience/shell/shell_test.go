package shell

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
	"gostalgia/platform"
	"gostalgia/sdk"
)

type noopCaller struct{}

func (noopCaller) Call(context.Context, string, any, any) error { return nil }

func TestEditingHistoryCompletionAndBounds(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("héllo")})
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if string(m.input) != "hélo" {
		t.Fatalf("edited = %s", string(m.input))
	}
	m.history = []string{"apps", "echo hello"}
	m.historyPos = 2
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if string(m.input) != "echo hello" {
		t.Fatal("history up failed")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if string(m.input) != "hélo" {
		t.Fatal("draft not restored")
	}
	m.input = []rune("laun")
	m.cursor = 4
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if string(m.input) != "launch" {
		t.Fatal("completion failed")
	}
	m.append(entry{strings.Repeat("line\n", 1000) + "\x1b]52;c;unsafe\a", "output"})
	if len(m.transcript) != maxTranscript {
		t.Fatalf("scrollback = %d", len(m.transcript))
	}
	m.cwd = "/users/guest/" + strings.Repeat("long-directory/", 20)
	m.input = []rune(strings.Repeat("界", 100))
	m.cursor = 90
	for _, size := range [][2]int{{80, 24}, {40, 12}, {30, 10}, {20, 8}, {120, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		view := m.View()
		if strings.Contains(view, "\a") {
			t.Fatal("rendered terminal control")
		}
		if lipgloss.Height(view) > size[1] || lipgloss.Width(view) > size[0] {
			t.Fatalf("view %dx%d exceeds %dx%d", lipgloss.Width(view), lipgloss.Height(view), size[0], size[1])
		}
	}
}

func TestShelfKeepsSelectedAppVisible(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.shelf = true
	m.height = 12
	m.width = 60
	for i := 0; i < 20; i++ {
		var a appStatus
		a.Manifest.Name = fmt.Sprintf("APP-%02d", i)
		m.apps = append(m.apps, a)
	}
	m.selected = 19
	if !strings.Contains(m.View(), "APP-19") {
		t.Fatal("selected app offscreen")
	}
}

func TestDOSPathsAndParsing(t *testing.T) {
	for input, want := range map[string]string{`C:\users\guest`: "/users/guest", `documents\note.txt`: "/users/guest/documents/note.txt", "..": "/users"} {
		if got := envPath("/users/guest", input); got != want {
			t.Errorf("%s = %s, want %s", input, got, want)
		}
	}
	args, err := words(`type "C:\a folder\file.txt"`)
	if err != nil || len(args) != 2 || args[1] != `C:\a folder\file.txt` {
		t.Fatalf("words = %v, %v", args, err)
	}
	if _, err := words(`echo "unfinished`); err == nil {
		t.Fatal("unclosed quote accepted")
	}
	for _, line := range []string{"launch", "stop", "cat", "cd", "ps extra", "call route {no}", "missing"} {
		if _, _, _, err := command(context.Background(), noopCaller{}, "/", line); err == nil {
			t.Errorf("accepted %q", line)
		}
	}
}

// observedModel runs the actual tea event loop while providing an explicit
// completion barrier; tests never read UI state concurrently with Update.
type observedModel struct {
	*Model
	results chan resultMsg
	views   chan viewMsg
}

func (m *observedModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.Model.Update(msg)
	if r, ok := msg.(resultMsg); ok {
		m.results <- r
	}
	if v, ok := msg.(viewMsg); ok && m.views != nil {
		m.views <- v
	}
	return m, cmd
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

func TestBubbleTeaSocketAppLifecycle(t *testing.T) {
	root := t.TempDir()
	rt, err := runtime.Boot(context.Background(), runtime.Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("shell test cleanup") })
	data, err := os.ReadFile(filepath.Join(root, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	conn, err := platform.DialIPC(rt.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.NewClient(conn, info.Token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	// Issue #132: one 15s budget for the whole lifecycle starved later
	// awaits whenever a loaded CI runner burned the budget early. Instead,
	// give the program the test-scoped context (the model already bounds
	// every IPC call with its own 3-10s deadline) and give each await its
	// own deadline, so one slow stage cannot starve the rest.
	ctx := t.Context()
	m := &observedModel{Model: New(ctx, client, rt.Done()), results: make(chan resultMsg, 20), views: make(chan viewMsg, 10)}
	output := &lockedBuffer{}
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	t.Cleanup(func() { p.Kill(); <-done })
	// perStep must sit strictly above the model's largest production
	// deadline on awaited paths (10s in Init/submit/presentation action) so
	// a production failure surfaces as its real error (e.g.
	// "ipc: context deadline exceeded") before the await's generic timeout
	// fires. The await timer also starts earlier than the production timer
	// (it is armed before tea dispatches the key), so headroom is required,
	// not just >=.
	const perStep = 30 * time.Second
	await := func(step string) resultMsg {
		t.Helper()
		timer := time.NewTimer(perStep)
		defer timer.Stop()
		select {
		case r := <-m.results:
			if r.err != nil {
				t.Fatal(r.err)
			}
			return r
		case <-timer.C:
			t.Fatalf("Bubble Tea result timed out after %v at step %q", perStep, step)
			return resultMsg{}
		}
	}
	await("init app list") // Init's app list.
	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	submit := func(line, step string) resultMsg {
		t.Helper()
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(line)})
		p.Send(tea.KeyMsg{Type: tea.KeyEnter})
		return await(step)
	}
	if r := submit("apps", "apps"); !strings.Contains(r.text, "com.gostalgia.echo") {
		t.Fatal("demo missing")
	}
	if r := submit("echo fabulous", "echo fabulous"); r.text != "fabulous   [echo #1]" {
		t.Fatal(r.text)
	}
	r := submit("call app/com.gostalgia.echo/identity", "call identity")
	var identity struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal([]byte(r.text), &identity); err != nil {
		t.Fatal(err)
	}
	if len(identity.Capabilities) != 1 || identity.Capabilities[0] != "ipc" {
		t.Fatalf("demo grant = %v", identity.Capabilities)
	}
	submit("stop com.gostalgia.echo", "stop")
	if rt.Apps.IsRunning("com.gostalgia.echo") {
		t.Fatal("stop left app running")
	}
	submit("launch com.gostalgia.echo", "launch")
	if !rt.Apps.IsRunning("com.gostalgia.echo") {
		t.Fatal("launch failed")
	}
	if r := submit("echo renewed", "echo renewed"); !strings.Contains(r.text, "echo #1") {
		t.Fatal("instance state not reset")
	}
	awaitView := func(step string) viewMsg {
		t.Helper()
		timer := time.NewTimer(perStep)
		defer timer.Stop()
		select {
		case v := <-m.views:
			if v.err != nil {
				t.Fatal(v.err)
			}
			return v
		case <-timer.C:
			t.Fatalf("presentation timed out after %v at step %q", perStep, step)
			return viewMsg{}
		}
	}
	p.Send(tea.KeyMsg{Type: tea.KeyF4})
	if v := awaitView("open Echo view"); v.data.Title != "Echo" || v.data.State != sdk.ViewReady {
		t.Fatal("Echo view did not open through the shell")
	}
	p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("from the app view")})
	p.Send(tea.KeyMsg{Type: tea.KeyEnter})
	if v := awaitView("shell action through app view"); !v.action || v.data.Status != "2 echoes this launch" || v.data.Items[0].Detail != "from the app view" {
		t.Fatal("shell action did not reach the owning app over IPC")
	}
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	if r := submit("ps", "ps"); !strings.Contains(r.text, "com.gostalgia.echo") || !strings.Contains(r.text, "STATE / STATUS") {
		t.Fatalf("ps output unexpected: %s", r.text)
	}
	if r := submit("logs 1", "logs"); !strings.Contains(r.text, "PROCESS 1") {
		t.Fatalf("logs output unexpected: %s", r.text)
	}
	if r := submit(`cd C:\users\guest\documents`, "cd"); r.cwd != "/users/guest/documents" {
		t.Fatal(r.cwd)
	}
	submit("dir", "dir")
	p.Send(tea.KeyMsg{Type: tea.KeyF2})
	// Filter the shelf to Echo: the first app by ID is not necessarily Echo.
	p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("echo")})
	p.Send(tea.KeyMsg{Type: tea.KeyF3})
	await("shelf stop") // stop through the app shelf
	p.Send(tea.KeyMsg{Type: tea.KeyEnter})
	await("shelf relaunch") // relaunch through the app shelf
	// The first Esc clears the shelf filter; the second returns to the prompt.
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	p.Send(tea.KeyMsg{Type: tea.KeyEsc})
	submit("exit", "exit")
	// Bubble Tea flushes its renderer on quit; verify the actual rendered UI.
	exitTimer := time.NewTimer(perStep)
	defer exitTimer.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		// Replace the cleanup's consumed channel result.
		done <- nil
	case <-exitTimer.C:
		t.Fatalf("program did not exit within %v", perStep)
	}
	if !strings.Contains(output.String(), "G O S T A L G I A") {
		t.Fatal("Bubble Tea did not render the styled shell")
	}
}

func TestCharmBaselineVersions(t *testing.T) {
	if CharmBubbleTeaVersion != "v1.3.10" {
		t.Errorf("Bubble Tea version = %s, want v1.3.10", CharmBubbleTeaVersion)
	}
	if CharmLipGlossVersion != "v1.1.0" {
		t.Errorf("Lip Gloss version = %s, want v1.1.0", CharmLipGlossVersion)
	}
	if CharmBubblesVersion != "v1.0.0" {
		t.Errorf("Bubbles version = %s, want v1.0.0", CharmBubblesVersion)
	}
}

func TestGraphemeClusterEditing(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	// Test combining character cluster e + acute accent (e\u0301)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e\u0301clair")})
	if string(m.input) != "e\u0301clair" {
		t.Fatalf("input = %s, want e\\u0301clair", string(m.input))
	}
	// Move left 5 clusters (past r, i, a, l, c)
	for i := 0; i < 5; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	}
	// Cursor should now be right after the 2-rune cluster "e\u0301" (index 2)
	if m.cursor != 2 {
		t.Fatalf("cursor after 5 left steps = %d, want 2", m.cursor)
	}
	// Move left once more; cursor should leap over the whole grapheme cluster to 0
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.cursor != 0 {
		t.Fatalf("cursor at start = %d, want 0", m.cursor)
	}
	// Move right once; cursor should land at 2 (not 1 in the middle of combining cluster)
	m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if m.cursor != 2 {
		t.Fatalf("cursor after right = %d, want 2", m.cursor)
	}
	// Backspace deletes the entire "e\u0301" cluster
	m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if string(m.input) != "clair" || m.cursor != 0 {
		t.Fatalf("after backspace cluster: input=%s, cursor=%d", string(m.input), m.cursor)
	}

	// Test ZWJ sequence (woman technologist: 👩 + ZWJ + 💻 = 3 runes)
	m.input = nil
	m.cursor = 0
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("👩\u200d💻!")})
	if len(m.input) != 4 { // 3 runes for emoji + 1 rune for '!'
		t.Fatalf("input runes count = %d, want 4", len(m.input))
	}
	// Cursor is at 4. Move left: cursor is at 3 (before '!').
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.cursor != 3 {
		t.Fatalf("cursor before ! = %d, want 3", m.cursor)
	}
	// Move left again: leaps over 👩\u200d💻 to index 0.
	m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if m.cursor != 0 {
		t.Fatalf("cursor before ZWJ = %d, want 0", m.cursor)
	}
	// Delete key on cluster at index 0 deletes the entire 3-rune emoji
	m.Update(tea.KeyMsg{Type: tea.KeyDelete})
	if string(m.input) != "!" || m.cursor != 0 {
		t.Fatalf("after delete on emoji: input=%s, cursor=%d", string(m.input), m.cursor)
	}
}

func TestThemeAndMotionCommands(t *testing.T) {
	ctx := context.Background()
	m := New(ctx, noopCaller{}, nil)

	// Test theme command with no args shows active theme
	res := execute(ctx, m.client, m.cwd, "theme")
	if !res.showTheme {
		t.Fatalf("expected showTheme true")
	}
	m.Update(res)
	foundActive := false
	for _, entry := range m.transcript {
		if strings.Contains(entry.text, "Active theme: nostalgia") {
			foundActive = true
			break
		}
	}
	if !foundActive {
		t.Fatalf("transcript after theme command missing active theme: %v", m.transcript)
	}

	// Test switching theme to monochrome
	res = execute(ctx, m.client, m.cwd, "theme monochrome")
	if res.setTheme != "monochrome" {
		t.Fatalf("setTheme = %s, want monochrome", res.setTheme)
	}
	m.Update(res)
	if m.Theme().Name != "monochrome" || !m.Theme().ReducedMotion {
		t.Fatalf("theme name = %s, motion = %v", m.Theme().Name, m.Theme().ReducedMotion)
	}

	// Test switching to high-contrast
	res = execute(ctx, m.client, m.cwd, "theme high-contrast")
	if res.setTheme != "high-contrast" {
		t.Fatalf("setTheme = %s, want high-contrast", res.setTheme)
	}
	m.Update(res)
	if m.Theme().Name != "high-contrast" {
		t.Fatalf("theme name = %s", m.Theme().Name)
	}

	// Test switching to high-contrast-light
	m.SetThemeByName("high-contrast-light")
	if m.Theme().Name != "high-contrast-light" {
		t.Fatalf("theme name = %s", m.Theme().Name)
	}

	// Test switching back to nostalgia
	m.SetThemeByName("nostalgia")
	if m.Theme().Name != "nostalgia" {
		t.Fatalf("theme name = %s", m.Theme().Name)
	}

	// Test invalid theme
	res = execute(ctx, m.client, m.cwd, "theme nonexistent")
	if res.setTheme != "nonexistent" {
		t.Fatalf("expected setTheme nonexistent, got %s", res.setTheme)
	}
	m.Update(res)
	foundErr := false
	for _, entry := range m.transcript {
		if strings.Contains(strings.ToLower(entry.text), `unknown theme "nonexistent"`) {
			foundErr = true
			break
		}
	}
	if !foundErr {
		t.Fatalf("expected unknown theme error in transcript, got: %v", m.transcript)
	}

	// Test motion toggling
	m.SetReducedMotion(false)
	res = execute(ctx, m.client, m.cwd, "motion")
	if !res.toggleMotion {
		t.Fatalf("expected toggleMotion true")
	}
	m.Update(res)
	if !m.Theme().ReducedMotion {
		t.Fatalf("expected reduced motion true after toggle")
	}

	// Test motion off
	res = execute(ctx, m.client, m.cwd, "motion off")
	if res.setMotion == nil || *res.setMotion {
		t.Fatalf("expected setMotion false")
	}
	m.Update(res)
	if m.Theme().ReducedMotion {
		t.Fatalf("expected reduced motion false")
	}

	// Test motion on
	res = execute(ctx, m.client, m.cwd, "motion on")
	if res.setMotion == nil || !*res.setMotion {
		t.Fatalf("expected setMotion true")
	}
	m.Update(res)
	if !m.Theme().ReducedMotion {
		t.Fatalf("expected reduced motion true")
	}

	// Test tab completion for theme and motion
	m.input = []rune("theme mono")
	m.cursor = len(m.input)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if string(m.input) != "theme monochrome" {
		t.Fatalf("completed theme = %s", string(m.input))
	}

	m.input = []rune("motion of")
	m.cursor = len(m.input)
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if string(m.input) != "motion off" {
		t.Fatalf("completed motion = %s", string(m.input))
	}
}

func TestTerminalRestoration(t *testing.T) {
	var buf bytes.Buffer
	Restore(&buf)
	got := buf.String()
	// Check for mouse disable, bracketed paste disable, cursor show, SGR reset, alt screen exit
	for _, expected := range []string{
		"\x1b[?1000l",
		"\x1b[?1002l",
		"\x1b[?1003l",
		"\x1b[?1006l",
		"\x1b[?2004l",
		"\x1b[?25h",
		"\x1b[0m",
		"\x1b[?1049l",
	} {
		if !strings.Contains(got, expected) {
			t.Errorf("Restore missing sequence %q in %q", expected, got)
		}
	}

	// Restore(nil) must not panic
	Restore(nil)
}

func TestSmallViewportDegradation(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	// Fill transcript with content
	m.append(entry{"line 1", "output"})
	m.append(entry{"line 2", "output"})
	m.append(entry{"line 3", "output"})

	for _, mode := range []viewMode{modeHome, modeLauncher, modePrompt} {
		m.setMode(mode)
		for _, size := range []ui.Bounds{
			{Width: 80, Height: 24},
			{Width: 60, Height: 18},
			{Width: 40, Height: 12},
			{Width: 30, Height: 10},
			{Width: 20, Height: 8},
			{Width: 10, Height: 5},
			{Width: 5, Height: 2},
			{Width: 0, Height: 0},
		} {
			m.Update(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
			view := m.View()
			if size.Width == 0 || size.Height == 0 {
				if view != "" {
					t.Fatalf("expected empty for %dx%d", size.Width, size.Height)
				}
				continue
			}
			w := lipgloss.Width(view)
			h := lipgloss.Height(view)
			if w > size.Width || h > size.Height {
				t.Fatalf("mode %d at %dx%d rendered %dx%d (exceeds bounds)", mode, size.Width, size.Height, w, h)
			}
		}
	}
}

type fakeFSCaller struct {
	noopCaller
	data map[string]string
}

func (f fakeFSCaller) Call(ctx context.Context, method string, in, out any) error {
	if method == "fs/read" {
		inMap, _ := in.(map[string]string)
		content, ok := f.data[inMap["path"]]
		if !ok {
			return fmt.Errorf("file not found: %s", inMap["path"])
		}
		outStruct, _ := out.(*struct {
			Data string `json:"data_base64"`
		})
		if outStruct != nil {
			outStruct.Data = base64.StdEncoding.EncodeToString([]byte(content))
		}
		return nil
	}
	if method == "app/com.gostalgia.echo/echo" {
		inMap, _ := in.(map[string]string)
		msg := inMap["msg"]
		outStruct, _ := out.(*struct {
			Msg    string `json:"msg"`
			Echoes int64  `json:"echoes"`
		})
		if outStruct != nil {
			outStruct.Msg = msg
			outStruct.Echoes = 1
		}
		return nil
	}
	return f.noopCaller.Call(ctx, method, in, out)
}

func TestExternalDataSanitizationInShell(t *testing.T) {
	evilPayload := "Safe text\x1b[2J\x1b[H\x1b]52;c;evil\aMore text\x1b[?1049h"
	client := fakeFSCaller{
		data: map[string]string{
			"/users/guest/evil.txt": evilPayload,
		},
	}
	m := New(context.Background(), client, nil)

	// Execute cat on evil.txt
	res := execute(context.Background(), m.client, m.cwd, `cat evil.txt`)
	if strings.Contains(res.text, "\x1b") || strings.Contains(res.text, "\a") {
		t.Fatalf("cat output contained escape sequences: %q", res.text)
	}
	if !strings.Contains(res.text, "Safe text") || !strings.Contains(res.text, "More text") {
		t.Fatalf("cat output lost safe content: %q", res.text)
	}

	// Test echo with escape sequences
	res = execute(context.Background(), m.client, m.cwd, "echo \x1b]0;Title\aHello \x1b[31mWorld\x1b[0m")
	if strings.Contains(res.text, "\x1b") || strings.Contains(res.text, "\a") {
		t.Fatalf("echo output contained escape sequences: %q", res.text)
	}
	if !strings.Contains(res.text, "Hello") || !strings.Contains(res.text, "World") {
		t.Fatalf("echo output lost words: %q", res.text)
	}
}

type fakeProfileCaller struct {
	noopCaller
	profiles []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	active string
}

func (f *fakeProfileCaller) Call(ctx context.Context, method string, in, out any) error {
	switch method {
	case "profile/list":
		res, _ := out.(*struct {
			Profiles []struct {
				ID          string `json:"id"`
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"profiles"`
			Active string `json:"active"`
		})
		if res != nil {
			res.Profiles = f.profiles
			res.Active = f.active
		}
		return nil
	case "profile/create":
		inMap, _ := in.(map[string]string)
		id := inMap["id"]
		name := inMap["name"]
		f.profiles = append(f.profiles, struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}{ID: id, Name: name, Description: "Workspace"})
		res, _ := out.(*struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		})
		if res != nil {
			res.ID = id
			res.Name = name
		}
		return nil
	case "profile/switch":
		inMap, _ := in.(map[string]string)
		f.active = inMap["id"]
		res, _ := out.(*struct {
			ID string `json:"id"`
		})
		if res != nil {
			res.ID = f.active
		}
		return nil
	case "profile/delete":
		inMap, _ := in.(map[string]string)
		id := inMap["id"]
		var next []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		for _, p := range f.profiles {
			if p.ID != id {
				next = append(next, p)
			}
		}
		f.profiles = next
		return nil
	case "app/list":
		return nil
	case "sys/status":
		res, _ := out.(*struct {
			UptimeSeconds float64 `json:"uptime_seconds"`
			User          string  `json:"user"`
			Processes     []any   `json:"processes"`
			Services      []any   `json:"services"`
		})
		if res != nil {
			res.User = f.active
		}
		return nil
	case "fs/list":
		return nil
	}
	return nil
}

func TestShellProfileCommandsAndSwitching(t *testing.T) {
	client := &fakeProfileCaller{
		profiles: []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}{
			{ID: "guest", Name: "Guest User", Description: "Default guest"},
		},
		active: "guest",
	}

	m := New(context.Background(), client, nil)
	if m.User() != "guest" {
		t.Fatalf("expected initial user guest, got %s", m.User())
	}

	// 1. List profiles
	res := execute(context.Background(), client, m.cwd, "profile list")
	if res.err != nil {
		t.Fatalf("profile list failed: %v", res.err)
	}
	if !strings.Contains(res.text, "* guest (Guest User) [active]") {
		t.Fatalf("expected active guest indicator, got: %q", res.text)
	}

	// 2. Create profile
	res = execute(context.Background(), client, m.cwd, "profile create developer Lead Dev")
	if res.err != nil {
		t.Fatalf("profile create failed: %v", res.err)
	}
	if !strings.Contains(res.text, "Profile developer (Lead Dev) created") {
		t.Fatalf("unexpected create response: %q", res.text)
	}

	// 3. Switch profile
	res = execute(context.Background(), client, m.cwd, "profile switch developer")
	if res.err != nil {
		t.Fatalf("profile switch failed: %v", res.err)
	}
	if res.switchedUser != "developer" {
		t.Fatalf("expected switchedUser developer, got %s", res.switchedUser)
	}
	if res.cwd != "/users/developer" {
		t.Fatalf("expected cwd /users/developer, got %s", res.cwd)
	}

	// Apply result to Model
	m.Update(res)
	if m.User() != "developer" {
		t.Fatalf("expected model user developer, got %s", m.User())
	}
	if m.cwd != "/users/developer" {
		t.Fatalf("expected model cwd /users/developer, got %s", m.cwd)
	}

	// Verify View reflects active profile in badge
	view := m.View()
	if !strings.Contains(view, "developer · C: environment drive") {
		t.Fatalf("expected developer in badge view, got: %s", view)
	}

	// 4. Delete profile
	res = execute(context.Background(), client, m.cwd, "profile delete developer")
	if res.err != nil {
		t.Fatalf("profile delete failed: %v", res.err)
	}
	if !strings.Contains(res.text, "Profile developer deleted") {
		t.Fatalf("unexpected delete response: %q", res.text)
	}
}
