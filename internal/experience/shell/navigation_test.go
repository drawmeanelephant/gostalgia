package shell

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func mockApps() []appStatus {
	return []appStatus{
		{
			Manifest: struct {
				ID          string   `json:"id"`
				Name        string   `json:"name"`
				Version     string   `json:"version"`
				Description string   `json:"description"`
				Permissions []string `json:"permissions"`
			}{
				ID:          "com.gostalgia.echo",
				Name:        "Echo",
				Version:     "0.1.0",
				Description: "A friendly echo app",
			},
			Running: true,
			PID:     1,
		},
		{
			Manifest: struct {
				ID          string   `json:"id"`
				Name        string   `json:"name"`
				Version     string   `json:"version"`
				Description string   `json:"description"`
				Permissions []string `json:"permissions"`
			}{
				ID:          "com.gostalgia.clock",
				Name:        "Clock",
				Version:     "1.0.0",
				Description: "Keeps system time",
			},
			Running: false,
		},
		{
			Manifest: struct {
				ID          string   `json:"id"`
				Name        string   `json:"name"`
				Version     string   `json:"version"`
				Description string   `json:"description"`
				Permissions []string `json:"permissions"`
			}{
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

	// F3 on selected app returns stop cmd.
	_, stopCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	if stopCmd == nil {
		t.Fatal("expected stop command on F3")
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
