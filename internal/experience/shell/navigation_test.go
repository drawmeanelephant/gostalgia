package shell

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

func mockApps() []appStatus {
	return []appStatus{
		{
			Manifest: appManifest{
				ID:          "com.gostalgia.echo",
				Name:        "Echo",
				Version:     "0.1.0",
				Description: "A friendly echo app",
			},
			Running: true,
			PID:     1,
		},
		{
			Manifest: appManifest{
				ID:          "com.gostalgia.clock",
				Name:        "Clock",
				Version:     "1.0.0",
				Description: "Keeps system time",
			},
			Running: false,
		},
		{
			Manifest: appManifest{
				ID:          "com.gostalgia.notepad",
				Name:        "Notepad",
				Version:     "0.5.0",
				Description: "Simple text editor",
			},
			Running: false,
		},
	}
}

func TestViewModeTransitionsAndFocus(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)

	// Default mode on startup is Home.
	if m.Focus() != FocusHome {
		t.Fatalf("expected FocusHome, got %v", m.Focus())
	}
	if m.Focus().String() != "home" {
		t.Fatalf("expected 'home', got %q", m.Focus().String())
	}

	// F1 toggles between Home and Prompt.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after F1, got %v", m.Focus())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
	if m.Focus() != FocusHome {
		t.Fatalf("expected FocusHome after second F1, got %v", m.Focus())
	}

	// F2 switches to Launcher.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.Focus() != FocusLauncher {
		t.Fatalf("expected FocusLauncher after F2, got %v", m.Focus())
	}
	if m.Focus().String() != "launcher" {
		t.Fatalf("expected 'launcher', got %q", m.Focus().String())
	}

	// Esc in Launcher returns to Prompt.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after Esc in Launcher, got %v", m.Focus())
	}

	// F2 toggles back and forth.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.Focus() != FocusLauncher {
		t.Fatalf("expected FocusLauncher, got %v", m.Focus())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF2})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after toggling F2 off, got %v", m.Focus())
	}

	// Ctrl+P opens Command Palette overlay.
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended})
	m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if m.Focus() != FocusPalette {
		t.Fatalf("expected FocusPalette after Ctrl+P, got %v", m.Focus())
	}
	if m.Focus().String() != "palette" {
		t.Fatalf("expected 'palette', got %q", m.Focus().String())
	}

	// Esc closes Command Palette overlay.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after closing palette, got %v", m.Focus())
	}

	// '/' at empty prompt opens palette.
	m.input = nil
	m.cursor = 0
	m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
	if m.Focus() != FocusPalette {
		t.Fatalf("expected FocusPalette after '/' at empty prompt, got %v", m.Focus())
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
}

func TestLauncherSearchFiltering(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = mockApps()
	m.setMode(modeLauncher)

	if len(m.filteredApps()) != 3 {
		t.Fatalf("expected 3 apps initially, got %d", len(m.filteredApps()))
	}

	// Type search query: "clk" should not match, but "clo" should match Clock.
	for _, r := range "clo" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.launcherQuery != "clo" {
		t.Fatalf("expected launcher query 'clo', got %q", m.launcherQuery)
	}
	filtered := m.filteredApps()
	if len(filtered) != 1 || filtered[0].Manifest.Name != "Clock" {
		t.Fatalf("expected 1 match (Clock), got %d matches", len(filtered))
	}

	// Backspace removes characters.
	m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.launcherQuery != "cl" {
		t.Fatalf("expected launcher query 'cl', got %q", m.launcherQuery)
	}

	// Filter by description: "editor" matches Notepad.
	m.launcherQuery = "editor"
	filtered = m.filteredApps()
	if len(filtered) != 1 || filtered[0].Manifest.Name != "Notepad" {
		t.Fatalf("expected 1 match (Notepad) for description query, got %d", len(filtered))
	}

	// Query with no matches shows honest empty state in View.
	m.launcherQuery = "nonexistent"
	filtered = m.filteredApps()
	if len(filtered) != 0 {
		t.Fatalf("expected 0 matches, got %d", len(filtered))
	}
	view := m.View().Content
	if !strings.Contains(view, "No matching apps") {
		t.Fatalf("expected 'No matching apps' in view, got:\n%s", view)
	}

	// Esc with non-empty query clears query first.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.launcherQuery != "" {
		t.Fatalf("expected empty query after first Esc, got %q", m.launcherQuery)
	}
	if m.Focus() != FocusLauncher {
		t.Fatalf("expected to stay in FocusLauncher after clearing query, got %v", m.Focus())
	}

	// Second Esc exits Launcher to Prompt.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after second Esc, got %v", m.Focus())
	}
}

func TestLauncherKeyboardNavigationAndActions(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = mockApps()
	m.setMode(modeLauncher)

	if m.selected != 0 {
		t.Fatalf("expected selected 0, got %d", m.selected)
	}

	// Down arrow moves selection.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.selected != 1 {
		t.Fatalf("expected selected 1 after down arrow, got %d", m.selected)
	}

	// Up arrow moves selection back.
	m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.selected != 0 {
		t.Fatalf("expected selected 0 after up arrow, got %d", m.selected)
	}

	// Enter on selected app returns launch cmd.
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected launch command on Enter")
	}

	// Reset busy state to simulate command completion.
	m.busy = false

	// F3 on the selected running app asks first, then Tab and Enter
	// confirm the stop. (Before #209, F3 returned the stop command itself.)
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF3}); m.Focus() != FocusDialog {
		t.Fatalf("F3 must open a confirmation, focus %v", m.Focus())
	} else if animationFrames(commandMessages(t, cmd)) != 1 {
		t.Fatal("F3 opened the dialog without scheduling its opening reveal")
	}
	settleFiniteMotion(m)
	m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, stopCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if stopCmd == nil {
		t.Fatal("expected stop command on confirming F3")
	}
}

func TestHomeDashboardNavigationAndShortcuts(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = mockApps()
	m.homeData = homeData{
		UptimeSeconds: 3665,
		User:          "guest",
		ProcessCount:  2,
		ServicesCount: 4,
		Documents: []docShortcut{
			{Name: "welcome.txt", Path: "/users/guest/documents/welcome.txt", Size: 42},
			{Name: "todo.md", Path: "/users/guest/documents/todo.md", Size: 128},
		},
	}

	items := m.homeItems()
	// 1 running app ("Echo") + 2 documents = 3 items.
	if len(items) != 3 {
		t.Fatalf("expected 3 home items, got %d", len(items))
	}

	view := m.View().Content
	if !strings.Contains(view, "1h 1m") {
		t.Fatalf("expected uptime '1h 1m' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "Echo") {
		t.Fatalf("expected 'Echo' in view, got:\n%s", view)
	}
	if !strings.Contains(view, "welcome.txt") {
		t.Fatalf("expected 'welcome.txt' in view, got:\n%s", view)
	}

	// Navigate down to first document.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.homeSelected != 1 {
		t.Fatalf("expected homeSelected 1, got %d", m.homeSelected)
	}

	// Pressing Enter on document submits "type <path>".
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected command on document Enter")
	}

	// Reset busy state to simulate command completion.
	m.busy = false

	// Typing letters from home screen switches to prompt mode seamlessly.
	m.setMode(modeHome)
	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "dir"})
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after typing runes in Home, got %v", m.Focus())
	}
	if string(m.input) != "dir" {
		t.Fatalf("expected input 'dir', got %q", string(m.input))
	}
}

func TestCommandPalette(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = mockApps()
	m.homeData.Documents = []docShortcut{
		{Name: "notes.txt", Path: "/users/guest/documents/notes.txt", Size: 10},
	}

	m.openPalette()
	if !m.paletteOpen {
		t.Fatal("expected paletteOpen true")
	}

	items := m.filteredPaletteItems()
	if len(items) == 0 {
		t.Fatal("expected palette items to be populated")
	}

	// Filter by document name.
	for _, r := range "notes" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if m.paletteQuery != "notes" {
		t.Fatalf("expected paletteQuery 'notes', got %q", m.paletteQuery)
	}
	filtered := m.filteredPaletteItems()
	if len(filtered) != 1 || filtered[0].Tag != "DOC" {
		t.Fatalf("expected 1 DOC match for 'notes', got %d", len(filtered))
	}

	// Enter executes the action and closes the palette.
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected command on palette Enter")
	}
	if m.paletteOpen {
		t.Fatal("expected palette to close after Enter")
	}

	// Reopen palette and filter by app name.
	m.openPalette()
	m.paletteQuery = "clock"
	filtered = m.filteredPaletteItems()
	if len(filtered) != 1 || filtered[0].Tag != "APP" {
		t.Fatalf("expected 1 APP match for 'clock', got %d", len(filtered))
	}

	// Filter by command.
	m.paletteQuery = "clear"
	filtered = m.filteredPaletteItems()
	if len(filtered) != 1 || filtered[0].Tag != "CMD" {
		t.Fatalf("expected 1 CMD match for 'clear', got %d", len(filtered))
	}

	// Esc closes palette without executing.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.paletteOpen {
		t.Fatal("expected palette to close on Esc")
	}
}

func TestPromptCommandNavigation(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.setMode(modePrompt)

	// Typing "home" and pressing Enter switches to Home mode.
	m.input = []rune("home")
	m.cursor = 4
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.Focus() != FocusHome {
		t.Fatalf("expected FocusHome after typing 'home', got %v", m.Focus())
	}

	// Typing "launcher" switches to Launcher mode.
	m.setMode(modePrompt)
	m.input = []rune("launcher")
	m.cursor = 8
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.Focus() != FocusLauncher {
		t.Fatalf("expected FocusLauncher after typing 'launcher', got %v", m.Focus())
	}

	// Typing "palette" opens palette.
	m.setMode(modePrompt)
	m.input = []rune("palette")
	m.cursor = 7
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.Focus() != FocusPalette {
		t.Fatalf("expected FocusPalette after typing 'palette', got %v", m.Focus())
	}
}

// launchCaller answers app/launch with launchErr and app/list with apps, so
// the shelf's asynchronous launch and refresh can be observed separately.
type launchCaller struct {
	launchErr error
	apps      []appStatus
}

func (c *launchCaller) Call(_ context.Context, method string, _ any, out any) error {
	switch method {
	case "app/launch":
		return c.launchErr
	case "app/list":
		data, err := json.Marshal(c.apps)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	}
	return nil
}

// A rejected launch from the shelf is feedback in the shelf: the user keeps
// the filter, the selected card and the live instance, and the tab stays put.
func TestRejectedLauncherLaunchRetainsContext(t *testing.T) {
	errorText := "app: com.gostalgia.echo is already running or initializing (pid 7)"
	apps := []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.clock", "Clock"),
	}
	caller := &launchCaller{launchErr: errors.New(errorText), apps: apps}
	m := New(context.Background(), caller, nil)
	m.apps = apps
	m.setMode(modeLauncher)
	m.launcherQuery = "Echo"
	m.selected = 0

	cmd := m.launchSelected()
	if cmd == nil {
		t.Fatal("launch of the selected app returned no command")
	}
	_, effects := m.Update(cmd())
	if frames := animationFrames(commandMessages(t, effects)); frames != 1 {
		t.Fatalf("rejected launch scheduled %d animation frames, want its one bounded effect", frames)
	}

	if got := m.Focus(); got != FocusLauncher {
		t.Fatalf("focus after rejected launch = %v, want launcher", got)
	}
	if m.launcherQuery != "Echo" {
		t.Fatalf("filter after rejection = %q, want Echo", m.launcherQuery)
	}
	if got := selectedAppID(m); got != "com.gostalgia.echo" {
		t.Fatalf("selected app after rejection = %q", got)
	}
	if len(m.apps) != len(apps) || !m.apps[0].Running || m.apps[0].PID != 7 {
		t.Fatalf("rejection replaced the live instance: %+v", m.apps)
	}
	body := strings.Join(m.renderLauncher(80, 12), "\n")
	if !strings.Contains(body, "LAUNCH FAILED") || !strings.Contains(body, "already running or initializing") {
		t.Fatalf("shelf shows no readable rejection:\n%s", body)
	}
	if view := m.View().Content; !strings.Contains(view, "LIVE / PID 7") {
		t.Fatalf("live instance missing from the shelf:\n%s", view)
	}
	// The feedback also fits the shell's smallest frame.
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	small := m.View().Content
	if !strings.Contains(small, "LAUNCH FAILED") {
		t.Fatalf("rejection missing from the 30x10 shelf:\n%s", small)
	}
	if w, h := lipgloss.Width(small), lipgloss.Height(small); w > 30 || h > 10 {
		t.Fatalf("30x10 rejection frame is %dx%d", w, h)
	}
	// The rejection is also kept in the transcript, independently of focus.
	found := false
	for _, e := range m.transcript {
		found = found || strings.Contains(e.text, errorText)
	}
	if !found {
		t.Fatal("rejected launch was not recorded in the transcript")
	}
	// A registry refresh that no longer lists the app clears feedback about it.
	m.Update(appListMsg{seq: m.appsSent, apps: []appStatus{}})
	if m.launchNotice != "" {
		t.Fatalf("rejection outlived its app: %q", m.launchNotice)
	}
	if body := strings.Join(m.renderLauncher(80, 10), "\n"); strings.Contains(body, "LAUNCH FAILED") || !strings.Contains(body, "No apps installed") {
		t.Fatalf("stale rejection left with an empty shelf:\n%s", body)
	}
}

// Leaving the shelf before a rejected launch answers must not drag the user
// back, rewrite the current filter, or appear as late shelf feedback.
func TestLateLauncherRejectionDoesNotStealNavigation(t *testing.T) {
	apps := []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.clock", "Clock"),
	}
	caller := &launchCaller{
		launchErr: errors.New("app: com.gostalgia.echo is already running or initializing (pid 7)"),
		apps:      apps,
	}
	m := New(context.Background(), caller, nil)
	m.apps = apps
	m.setMode(modeLauncher)
	m.launcherQuery = "Echo"
	m.selected = 0
	pending := m.launchSelected()

	m.setMode(modeHome)
	m.launcherQuery = "Clock"
	m.selected = 0
	m.Update(pending())

	if got := m.Focus(); got != FocusHome {
		t.Fatalf("late rejection moved focus to %v, want home", got)
	}
	if m.launcherQuery != "Clock" || selectedAppID(m) != "com.gostalgia.clock" {
		t.Fatalf("late rejection changed filter/selection: %q / %q", m.launcherQuery, selectedAppID(m))
	}
	m.setMode(modeLauncher)
	if body := strings.Join(m.renderLauncher(80, 12), "\n"); strings.Contains(body, "LAUNCH FAILED") {
		t.Fatalf("late rejection reappeared in the shelf:\n%s", body)
	}
}

// Overlapping shelf launches keep the newest outcome: an older rejection that
// arrives after a newer launch neither replaces its feedback nor revives a
// rejection a successful launch cleared.
func TestShelfLaunchRejectionsKeepNewestOutcome(t *testing.T) {
	apps := []appStatus{
		runningApp("com.gostalgia.echo", "Echo", 7),
		readyApp("com.gostalgia.clock", "Clock"),
	}
	caller := &launchCaller{apps: apps}
	m := New(context.Background(), caller, nil)
	m.apps = apps
	m.setMode(modeLauncher)

	caller.launchErr = errors.New("app: com.gostalgia.echo is already running or initializing (pid 7)")
	m.selected = 0
	first := m.launchSelected()()
	caller.launchErr = errors.New("app: com.gostalgia.clock is already running or initializing (pid 9)")
	m.selected = 1
	second := m.launchSelected()()

	m.Update(second)
	m.Update(first)
	if !strings.Contains(m.launchNotice, "clock") || strings.Contains(m.launchNotice, "echo") {
		t.Fatalf("stale rejection replaced the newest feedback: %q", m.launchNotice)
	}

	// A newer success clears an earlier rejection and leaves it cleared when
	// the earlier answer finally arrives.
	caller.launchErr = errors.New("app: com.gostalgia.echo is already running or initializing (pid 7)")
	m.selected = 0
	staleFailure := m.launchSelected()()
	caller.launchErr = nil
	m.selected = 1
	success := m.launchSelected()()
	m.Update(success)
	m.Update(staleFailure)
	if m.launchNotice != "" {
		t.Fatalf("stale rejection outlived the newer success: %q", m.launchNotice)
	}
}

// A launch typed at the prompt still reports there, on the command line.
func TestPromptLaunchRejectionStaysOnCommandLine(t *testing.T) {
	errorText := "app: com.gostalgia.echo is already running or initializing (pid 1)"
	caller := &launchCaller{launchErr: errors.New(errorText)}
	m := New(context.Background(), caller, nil)
	m.setMode(modePrompt)

	m.Update(m.submit("launch com.gostalgia.echo")())

	if got := m.Focus(); got != FocusPrompt {
		t.Fatalf("focus after prompt launch rejection = %v, want prompt", got)
	}
	found := false
	for _, e := range m.transcript {
		found = found || strings.Contains(e.text, errorText)
	}
	if !found {
		t.Fatal("prompt launch rejection missing from the command line")
	}
	if m.launchNotice != "" {
		t.Fatalf("prompt rejection leaked into the shelf: %q", m.launchNotice)
	}
}

// A prompt command that answers after navigation does not pull focus back.
func TestLatePromptLaunchResultDoesNotStealFocus(t *testing.T) {
	caller := &launchCaller{launchErr: errors.New("app: com.gostalgia.echo is already running")}
	m := New(context.Background(), caller, nil)
	m.setMode(modePrompt)
	pending := m.submit("launch com.gostalgia.echo")

	m.setMode(modeHome)
	m.Update(pending())

	if got := m.Focus(); got != FocusHome {
		t.Fatalf("late prompt result moved focus to %v, want home", got)
	}
}

// A successful shelf launch keeps the shelf as the successful path did and
// its accepted registry refresh shows the new live instance.
func TestLauncherLaunchSuccessKeepsShelf(t *testing.T) {
	live := runningApp("com.gostalgia.echo", "Echo", 8)
	caller := &launchCaller{apps: []appStatus{live}}
	m := New(context.Background(), caller, nil)
	m.apps = []appStatus{readyApp("com.gostalgia.echo", "Echo")}
	m.setMode(modeLauncher)
	m.selected = 0

	if cmd := m.launchSelected(); cmd == nil {
		t.Fatal("launch of the selected app returned no command")
	} else {
		m.Update(cmd())
	}
	if got := m.Focus(); got != FocusLauncher {
		t.Fatalf("focus after successful launch = %v, want launcher", got)
	}
	if m.launchNotice != "" {
		t.Fatalf("successful launch left rejection feedback: %q", m.launchNotice)
	}

	// The result's registry refresh is what turns the card LIVE.
	for _, msg := range commandMessages(t, m.refreshAppsCmd()) {
		if appMsg, ok := msg.(appListMsg); ok {
			m.Update(appMsg)
		}
	}
	if len(m.apps) != 1 || !m.apps[0].Running || m.apps[0].PID != 8 {
		t.Fatalf("successful launch did not reach the shelf: %+v", m.apps)
	}
}

type errCaller struct {
	err error
}

func (e errCaller) Call(context.Context, string, any, any) error {
	return e.err
}

func TestSingleInstanceRejectionErrorHandling(t *testing.T) {
	singleInstanceErr := errors.New("app: com.gostalgia.echo is already running (single-instance policy)")
	caller := errCaller{err: singleInstanceErr}
	m := New(context.Background(), caller, nil)
	m.apps = mockApps()
	m.setMode(modeLauncher)

	cmd := m.submit("launch com.gostalgia.echo")
	msg := cmd()
	m.Update(msg)

	// Single instance error should transition to prompt and record error in transcript.
	if m.Focus() != FocusPrompt {
		t.Fatalf("expected FocusPrompt after launch rejection, got %v", m.Focus())
	}
	found := false
	for _, entry := range m.transcript {
		if strings.Contains(entry.text, "single-instance policy") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected single-instance policy error in transcript")
	}
}
