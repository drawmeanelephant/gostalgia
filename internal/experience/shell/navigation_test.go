package shell

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"gostalgia/internal/ipc"
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

	// Enter on a running app opens/focuses its window without launching.
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected open-window command on Enter for running app")
	}
	if m.Focus() != FocusWindow || m.FocusedWindow() != "com.gostalgia.echo" {
		t.Fatalf("Enter on running app: focus=%v window=%q, want window com.gostalgia.echo", m.Focus(), m.FocusedWindow())
	}
	settleFiniteMotion(m)

	// Esc returns to the launcher with Echo still selected.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusLauncher {
		t.Fatalf("expected FocusLauncher after Esc from window, got %v", m.Focus())
	}

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
	m.busy = false

	// Enter on a ready (non-running) app returns a launch command.
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.selected != 1 {
		t.Fatalf("expected selected 1 (Clock), got %d", m.selected)
	}
	_, launchCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if launchCmd == nil || !m.busy {
		t.Fatal("expected launch command on Enter for ready app")
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

// launchCaller answers app/launch with launchErr, app/list with apps, and
// optional app/<id>/view with viewErr, so the shelf's asynchronous launch and
// refresh can be observed separately.
type launchCaller struct {
	launchErr   error
	viewErr     error
	launchCalls int
	apps        []appStatus
}

func (c *launchCaller) Call(_ context.Context, method string, _ any, out any) error {
	switch {
	case method == "app/launch":
		c.launchCalls++
		return c.launchErr
	case method == "app/list":
		data, err := json.Marshal(c.apps)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	case strings.HasSuffix(method, "/view"):
		return c.viewErr
	}
	return nil
}

// A rejected launch from the shelf is feedback in the shelf: the user keeps
// the filter, the selected card and any live instance, and the tab stays put.
func TestRejectedLauncherLaunchRetainsContext(t *testing.T) {
	errorText := "app: com.gostalgia.clock is already running or initializing (pid 7)"
	apps := []appStatus{
		readyApp("com.gostalgia.clock", "Clock"),
		runningApp("com.gostalgia.echo", "Echo", 7),
	}
	caller := &launchCaller{launchErr: errors.New(errorText), apps: apps}
	m := New(context.Background(), caller, nil)
	m.apps = apps
	m.setMode(modeLauncher)
	m.launcherQuery = "Clock"
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
	if m.launcherQuery != "Clock" {
		t.Fatalf("filter after rejection = %q, want Clock", m.launcherQuery)
	}
	if got := selectedAppID(m); got != "com.gostalgia.clock" {
		t.Fatalf("selected app after rejection = %q", got)
	}
	if len(m.apps) != len(apps) || !m.apps[1].Running || m.apps[1].PID != 7 {
		t.Fatalf("rejection replaced the live instance: %+v", m.apps)
	}
	body := strings.Join(m.renderLauncher(80, 12), "\n")
	if !strings.Contains(body, "LAUNCH FAILED") || !strings.Contains(body, "already running or initializing") {
		t.Fatalf("shelf shows no readable rejection:\n%s", body)
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
		readyApp("com.gostalgia.echo", "Echo"),
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
		readyApp("com.gostalgia.echo", "Echo"),
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

// A successful shelf launch of an app with a presentation opens its window,
// updates the shelf's live status, and Esc returns to the shelf where Enter
// focuses the existing window without relaunching.
func TestLauncherLaunchSuccessOpensWindowAndReEnterFocuses(t *testing.T) {
	live := runningApp("com.gostalgia.dogcalc", "Dogcalc", 8)
	caller := &launchCaller{apps: []appStatus{live}}
	m := New(context.Background(), caller, nil)
	m.apps = []appStatus{readyApp("com.gostalgia.dogcalc", "Dogcalc")}
	m.setMode(modeLauncher)
	m.launcherQuery = "dog"
	m.selected = 0

	cmd := m.launchSelected()
	if cmd == nil {
		t.Fatal("launch of the selected app returned no command")
	}
	m.Update(cmd())
	if caller.launchCalls != 1 {
		t.Fatalf("launch calls = %d, want 1", caller.launchCalls)
	}
	if got := m.Focus(); got != FocusWindow {
		t.Fatalf("focus after successful launch = %v, want window", got)
	}
	if got := m.FocusedWindow(); got != "com.gostalgia.dogcalc" {
		t.Fatalf("focused window = %q, want com.gostalgia.dogcalc", got)
	}
	if m.views["com.gostalgia.dogcalc"] == nil || m.views["com.gostalgia.dogcalc"].pid != 8 {
		t.Fatalf("opened window did not bind live PID 8: %+v", m.views["com.gostalgia.dogcalc"])
	}
	if m.launchNotice != "" {
		t.Fatalf("successful launch left rejection feedback: %q", m.launchNotice)
	}
	if len(m.apps) != 1 || !m.apps[0].Running || m.apps[0].PID != 8 {
		t.Fatalf("successful launch did not update shelf status: %+v", m.apps)
	}

	// Esc leaves the Windows screen back to the Apps shelf, keeping the filter
	// and the open window.
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.Focus() != FocusLauncher || m.launcherQuery != "dog" {
		t.Fatalf("Esc from window: focus=%v query=%q", m.Focus(), m.launcherQuery)
	}

	// Pressing Enter again on the LIVE app focuses its window without another launch.
	_, refocusCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if refocusCmd != nil {
		t.Fatal("refocusing an already-open window should not refetch or relaunch")
	}
	if caller.launchCalls != 1 {
		t.Fatalf("second Enter on LIVE app called app/launch (%d times), want 1", caller.launchCalls)
	}
	if m.Focus() != FocusWindow || m.FocusedWindow() != "com.gostalgia.dogcalc" {
		t.Fatalf("second Enter on LIVE app did not focus window: focus=%v window=%q", m.Focus(), m.FocusedWindow())
	}
	if m.launchNotice != "" {
		t.Fatalf("second Enter on LIVE app set launchNotice %q", m.launchNotice)
	}
}

// Launching a headless app (no app/<id>/view route) from the shelf keeps the
// user on the shelf instead of opening an error window.
func TestLauncherLaunchHeadlessAppStaysOnShelf(t *testing.T) {
	live := runningApp("com.test.headless", "Headless", 11)
	caller := &launchCaller{
		apps:    []appStatus{live},
		viewErr: &ipc.Error{Code: ipc.CodeUnknownMethod, Message: `unknown method "app/com.test.headless/view"`},
	}
	m := New(context.Background(), caller, nil)
	m.apps = []appStatus{readyApp("com.test.headless", "Headless")}
	m.setMode(modeLauncher)
	m.selected = 0

	cmd := m.launchSelected()
	if cmd == nil {
		t.Fatal("launch returned no command")
	}
	m.Update(cmd())
	if m.Focus() != FocusLauncher || m.onWindows() {
		t.Fatalf("headless app launch changed screen: focus=%v onWindows=%v", m.Focus(), m.onWindows())
	}
	if !m.apps[0].Running || m.apps[0].PID != 11 {
		t.Fatalf("headless app status not updated: %+v", m.apps[0])
	}
}

// Leaving the shelf (or opening a modal) before a shelf launch answers must
// not yank the user into the launched app's window.
func TestLateShelfLaunchSuccessDoesNotStealNavigation(t *testing.T) {
	t.Run("navigated away", func(t *testing.T) {
		live := runningApp("com.gostalgia.dogcalc", "Dogcalc", 14)
		caller := &launchCaller{apps: []appStatus{live}}
		m := New(context.Background(), caller, nil)
		m.apps = []appStatus{readyApp("com.gostalgia.dogcalc", "Dogcalc")}
		m.setMode(modeLauncher)
		pending := m.launchSelected()

		m.setMode(modeHome)
		m.Update(pending())
		if m.Focus() != FocusHome || m.onWindows() {
			t.Fatalf("late shelf launch stole focus: focus=%v onWindows=%v", m.Focus(), m.onWindows())
		}
		// Returning to the shelf later also starts a new epoch; a still-older
		// reply must not open a window either.
		m.setMode(modeLauncher)
		m.Update(pending())
		if m.Focus() != FocusLauncher || m.onWindows() {
			t.Fatalf("late shelf launch from earlier visit stole focus: focus=%v onWindows=%v", m.Focus(), m.onWindows())
		}
	})

	t.Run("modal open", func(t *testing.T) {
		live := runningApp("com.gostalgia.dogcalc", "Dogcalc", 15)
		caller := &launchCaller{apps: []appStatus{live}}
		m := New(context.Background(), caller, nil)
		m.apps = []appStatus{readyApp("com.gostalgia.dogcalc", "Dogcalc")}
		m.setMode(modeLauncher)
		pending := m.launchSelected()

		m.openPalette()
		m.Update(pending())
		if m.Focus() != FocusPalette || m.onWindows() {
			t.Fatalf("shelf launch disrupted open palette: focus=%v onWindows=%v", m.Focus(), m.onWindows())
		}
	})
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
