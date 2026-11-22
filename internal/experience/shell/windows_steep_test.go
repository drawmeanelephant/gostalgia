package shell

import (
	"context"
	"image"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	"getsteep.dev/steep"
	"getsteep.dev/steep/reel"
	xansi "github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/ipc"
	"gostalgia/internal/runtime"
)

const (
	steepNotesTitle    = "Notes - [Untitled]"
	steepSettingsTitle = "Settings - Appearance & Theme"
)

// newSteepWindowsRuntime is the shell at 80×24 on a real runtime, at the
// prompt. Its finite motion runs on the real clock, as the palette's
// entrance must finish for the palette to show.
func newSteepWindowsRuntime(t *testing.T, th theme.Theme) (*steep.Harness, *ipc.Client) {
	t.Helper()
	rt, client, err := bootShellTestRuntime(t.TempDir(), runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("steep windows cleanup") })
	t.Cleanup(func() { client.Close() })
	m := NewWithThemeAndNotifs(context.Background(), client, rt.Done(), th, ui.ANSI256, notifications.NewManager(100, 3, 8))
	m.motion.startupPhase = startupConsumed
	m.setMode(modePrompt)
	h := steep.NewHarness(t, m,
		steep.WithWindowSize(80, 24),
		steep.WithTimeout(30*time.Second),
		steep.WithColor(steep.ColorANSI256),
		steep.WithRecordMessages(true),
	)
	h.Wait("Services: 15")
	return h, client
}

// steepOpenTwoWindows opens Settings with F7, then Notes from the palette
// over it, and leaves Notes focused.
func steepOpenTwoWindows(t *testing.T, h *steep.Harness) {
	t.Helper()
	h.Key("f7").Wait([]string{"Windows (1)", steepSettingsTitle})
	h.KeyEsc().WaitNot("Ctrl-W CLOSE")
	h.Type("launch com.gostalgia.notes").Key("enter").Wait("Launched com.gostalgia.notes")
	h.Key("ctrl+p").Wait("COMMAND PALETTE")
	h.Type("switch to notes").Key("enter").Wait([]string{"Windows (2)", steepNotesTitle})
}

// steepBody is the body's rectangle on screen.
func steepBody(t *testing.T, h *steep.Harness) image.Rectangle {
	t.Helper()
	var body image.Rectangle
	h.Mutate(func(m *Model) *Model {
		r, ok := m.solve().Rect(slotBody)
		if !ok {
			t.Fatal("no body in the frame")
		}
		body = r
		return m
	})
	return body
}

// waitSteepWindow waits until the window titled title shows its top-left
// corner at x, y in the body, focused or not, with no pointer on its title.
func waitSteepWindow(t *testing.T, h *steep.Harness, body image.Rectangle, title string, x, y int, focused bool) {
	t.Helper()
	if focused {
		waitSteepCorner(t, h, body, "┏━"+theme.Nostalgia().Focus.Marker+" "+title, x, y)
		return
	}
	waitSteepCorner(t, h, body, "┌─  "+title, x, y)
}

// waitSteepCorner waits until want starts at x, y in the body.
func waitSteepCorner(t *testing.T, h *steep.Harness, body image.Rectangle, want string, x, y int) {
	t.Helper()
	h.WaitFor(reel.ScreenFunc(func(screen string) bool {
		lines := strings.Split(screen, "\n")
		row := body.Min.Y + y
		if row >= len(lines) {
			return false
		}
		col := body.Min.X + x
		return xansi.Cut(lines[row], col, col+xansi.StringWidth(want)) == want
	}))
}

func steepNoFrames(t *testing.T, h *steep.Harness) {
	t.Helper()
	for msg := range h.MessageHistory() {
		if _, ok := msg.(anim.FrameMsg); ok {
			t.Fatal("the windows scenario emitted an animation frame")
		}
	}
}

func TestSteepWindowsScenarios(t *testing.T) {
	t.Run("open_focus_move_close", func(t *testing.T) {
		h, client := newSteepWindowsRuntime(t, theme.Nostalgia())
		steepOpenTwoWindows(t, h)
		body := steepBody(t, h)
		// Settings fills the body; Notes aligns flush-left one row below.
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, false)
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 1, true)
		steepSnapshot(h, "two-windows")
		h.Mutate(func(m *Model) *Model {
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("an idle Windows screen scheduled work")
			}
			return m
		})

		// F8 focuses and raises Settings while keeping Notes discoverable in
		// the prompt row, and F8 again raises Notes.
		h.Key("f8")
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, true)
		h.Wait("F8 next: " + steepNotesTitle)
		h.Key("f8")
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 1, true)

		// Alt-Right moves Notes a column; a drag on its title moves it on.
		// The pointer rests on the moved title, so its cue shows once the
		// next message after the drag's last frame reaches the root.
		h.Key("alt+right")
		waitSteepWindow(t, h, body, steepNotesTitle, 1, 1, true)
		title := h.Locate(reel.Contains(steepNotesTitle))
		h.LeftDrag(title.X+2, title.Y, title.X+1, title.Y+1)
		waitSteepCorner(t, h, body, "┏━"+theme.Nostalgia().Focus.Marker+ui.HoverMarker+steepNotesTitle, 0, 2)
		steepSnapshot(h, "moved")

		// A click on Settings' title raises it, and its [X] closes it.
		h.Locate(reel.Contains(steepSettingsTitle)).Offset(2, 0).LeftClick()
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, true)
		h.Locate(reel.Contains(closeLabel)).Offset(1, 0).LeftClick()
		h.Wait("Windows (1)").WaitNot(steepSettingsTitle)
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 2, true)

		// Ctrl-W closes the last window and returns to the prompt. Closing
		// a window never stops its app.
		h.Key("ctrl+w").WaitNot([]string{"Windows (", "Ctrl-W CLOSE"})
		steepAppRunning(t, client, "com.gostalgia.notes", true)
		steepAppRunning(t, client, "com.gostalgia.settings", true)
		h.Mutate(func(m *Model) *Model {
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("the shell scheduled idle work after its windows closed")
			}
			return m
		})
		h.Type("echo windows-closed").Key("enter").Wait("[echo #1]")
	})

	t.Run("modal_blocking", func(t *testing.T) {
		h, client := newSteepWindowsRuntime(t, theme.Nostalgia())
		steepOpenTwoWindows(t, h)
		body := steepBody(t, h)
		settingsClose := h.Locate(reel.Contains(closeLabel)).Offset(1, 0)
		// Under the palette and under a confirmation, no window key or
		// click reaches the windows.
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Key("alt+right").Key("f8")
		settingsClose.LeftClick()
		h.KeyEsc().WaitNot("COMMAND PALETTE")
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("stop notes").Key("enter").Wait("Stop Notes?")
		h.Key("alt+right").Key("f8").Key("ctrl+w")
		settingsClose.LeftClick()
		// Esc cancels: Cancel has focus.
		h.KeyEsc().WaitNot("Stop Notes?")
		h.Wait("Windows (2)")
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, false)
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 1, true)
		steepAppRunning(t, client, "com.gostalgia.notes", true)
		// The windows take keys again once the modal is gone.
		h.Key("alt+right")
		waitSteepWindow(t, h, body, steepNotesTitle, 1, 1, true)
	})

	t.Run("small_terminal", func(t *testing.T) {
		h, _ := newSteepWindowsRuntime(t, theme.Nostalgia())
		steepOpenTwoWindows(t, h)
		// Below the 32×12 floor only the focused window shows, filling the
		// body without a frame, and every window key still works.
		h.Resize(30, 10).Wait("[Windows (2)]").WaitNot(closeLabel)
		h.Wait(steepNotesTitle)
		steepSnapshot(h, "small")
		h.Key("alt+right").Key("f8").Wait("Settings - Appea").WaitNot(steepNotesTitle)
		h.Key("ctrl+w").Wait([]string{"[Windows (1)]", steepNotesTitle})
		h.KeyEsc().WaitNot("[Windows (1)]")
		h.Key("f8").Wait([]string{"[Windows (1)]", steepNotesTitle})
		// Grown back, Notes floats where it was.
		h.Resize(80, 24).Wait(closeLabel)
		waitSteepWindow(t, h, steepBody(t, h), steepNotesTitle, 0, 1, true)
	})

	t.Run("compendium_minimum_size", func(t *testing.T) {
		h, _ := newSteepWindowsRuntime(t, theme.Nostalgia())
		h.Type("launch com.gostalgia.compendium").Key("enter").Wait("Launched com.gostalgia.compendium")
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("switch to compendium").Key("enter").Wait([]string{"Windows (1)", "Compendium"})

		// Create and open a real Compendium note.
		h.Type("new Field Guide").Key("enter").Wait("Compendium — Field Guide")
		h.Type("Alpha line").Key("enter").Type("Beta draft").Wait("Beta draft")

		// Resize to the 30x10 minimum: editing context, navigation cues, and
		// whole compact help bindings must stay usable together.
		h.Resize(30, 10).Wait([]string{"[Windows (1)]", "Compendium — Field Guide", "Markdown:", "Beta draft", "[Save]", "Esc BACK"})
		h.RequireNot("Tab focus")

		// Tab reaches Rename and Save; Save commits the note at 30x10.
		h.Key("tab").Wait("Rename to:")
		h.Key("tab").Wait([]string{"[Save]", "Enter GO"})
		h.Key("enter").Wait("Compendium — Field Guide")

		// Below 30x10 keeps the graceful fallback; growing back restores the
		// floating window with the saved note content intact.
		h.Resize(29, 10).Wait("Resize to 30×10")
		h.Resize(80, 24).Wait([]string{closeLabel, "Compendium — Field Guide", "Beta draft"})
	})

	t.Run("reduced_motion", func(t *testing.T) {
		h, _ := newSteepWindowsRuntime(t, theme.Nostalgia().WithReducedMotion(true))
		h.Type("motion on").Key("enter").Wait("Reduced motion is now enabled")
		steepOpenTwoWindows(t, h)
		body := steepBody(t, h)
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 1, true)
		h.Key("alt+right")
		waitSteepWindow(t, h, body, steepNotesTitle, 1, 1, true)
		h.Key("ctrl+w").Wait("Windows (1)")
		steepNoFrames(t, h)
	})

	t.Run("dogcalc_compendium_100x32", func(t *testing.T) {
		h, _ := newSteepWindowsRuntime(t, theme.Nostalgia())
		h.Resize(100, 32).Wait("ONLINE")
		h.Type("launch com.gostalgia.dogcalc").Key("enter").Wait("Launched com.gostalgia.dogcalc")
		h.Type("launch com.gostalgia.compendium").Key("enter").Wait("Launched com.gostalgia.compendium")
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("switch to dogcalc").Key("enter").Wait([]string{"Windows (1)", "Dogcalc"})
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("switch to compendium").Key("enter").Wait([]string{"Windows (2)", "Compendium"})
		body := steepBody(t, h)
		waitSteepWindow(t, h, body, "Dogcalc", 0, 0, false)
		waitSteepWindow(t, h, body, "Compendium", 0, 1, true)
		h.Wait("F8 next: Dogcalc")
		steepSnapshot(h, "dogcalc-compendium-100x32")
		h.Key("f8")
		waitSteepWindow(t, h, body, "Dogcalc", 0, 0, true)
		h.Wait("F8 next: Compendium")
	})
}

// TestSteepNotesLiveDraftStatus exercises Issue #293 end to end in a 100×32
// terminal against a real runtime and Notes process: typing into the multiline
// Content field updates the unsaved markers, footer, and rune/byte size live
// without activating Save, and reverts cleanly on edit-back or Save.
func TestSteepNotesLiveDraftStatus(t *testing.T) {
	rt, client, err := bootShellTestRuntime(t.TempDir(), runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("steep notes live draft cleanup") })
	t.Cleanup(func() { _ = client.Close() })

	m := NewWithThemeAndNotifs(context.Background(), client, rt.Done(), theme.Nostalgia(), ui.ANSI256, notifications.NewManager(100, 3, 8))
	m.motion.startupPhase = startupConsumed
	m.setMode(modePrompt)
	h := steep.NewHarness(t, m,
		steep.WithWindowSize(100, 32),
		steep.WithTimeout(30*time.Second),
		steep.WithColor(steep.ColorANSI256),
	)
	h.Wait("Services: 15")

	// Launch Notes and open its window.
	h.Type("launch com.gostalgia.notes").Key("enter").Wait("Launched com.gostalgia.notes")
	h.Key("ctrl+p").Wait("COMMAND PALETTE")
	h.Type("switch to notes").Key("enter").Wait([]string{"Windows (1)", steepNotesTitle, "Saved (up to date)"})

	// Enter a path and type multiline content without activating Save.
	docPath := "/users/guest/documents/draft.txt"
	h.Type(docPath)
	h.Key("tab")
	h.Type("Hello").Key("enter").Type("world")

	h.Wait([]string{
		"Notes - [Untitled]*",
		"[Untitled] [modified]",
		"Size: 11 runes · 11 bytes",
		"Modified (unsaved changes)",
	}).WaitNot([]string{"[saved]", "Saved (up to date)", "0 runes · 0 bytes"})

	// Clearing the draft reverts the untitled view cleanly.
	h.Key("ctrl+u").Wait([]string{
		"[Untitled] [saved]",
		"Size: 0 runes · 0 bytes",
		"Saved (up to date)",
	}).WaitNot([]string{"Notes - [Untitled]*", "[modified]", "Modified (unsaved changes)"})

	// Grant write access, save a document, then edit and revert back to the saved text.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Call(ctx, "fs/grant", map[string]any{
		"app_id":    "com.gostalgia.notes",
		"path":      "/users/guest/documents",
		"access":    "read-write",
		"recursive": true,
	}, nil); err != nil {
		t.Fatal(err)
	}

	h.Type("Saved note").Wait([]string{
		"Notes - [Untitled]*",
		"[Untitled] [modified]",
		"Size: 10 runes · 10 bytes",
		"Modified (unsaved changes)",
	})
	h.Key("tab").Key("enter").Wait([]string{
		"Notes - draft.txt",
		docPath + " [saved]",
		"Size: 10 runes · 10 bytes",
		"Saved " + docPath + " (10 bytes)",
	}).WaitNot("Notes - draft.txt*")

	// Editing the saved document immediately marks it modified and drops the Saved footer.
	h.Key("shift+tab").Type("!").Wait([]string{
		"Notes - draft.txt*",
		docPath + " [modified]",
		"Size: 11 runes · 11 bytes",
		"Modified (unsaved changes)",
	}).WaitNot([]string{docPath + " [saved]", "Saved " + docPath})

	// Backspacing the edit reverts cleanly to saved state.
	h.Key("backspace").Wait([]string{
		"Notes - draft.txt",
		docPath + " [saved]",
		"Size: 10 runes · 10 bytes",
		"Saved " + docPath + " (10 bytes)",
	}).WaitNot([]string{"Notes - draft.txt*", "[modified]", "Modified (unsaved changes)"})
}
