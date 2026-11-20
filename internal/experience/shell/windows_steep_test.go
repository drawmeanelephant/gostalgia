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
// corner at x, y in the body, focused or not.
func waitSteepWindow(t *testing.T, h *steep.Harness, body image.Rectangle, title string, x, y int, focused bool) {
	t.Helper()
	marker := " "
	if focused {
		marker = theme.Nostalgia().Focus.Marker
	}
	want := "┏━" + marker + " " + title
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
		// Settings fills the body; Notes cascades from it.
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, false)
		waitSteepWindow(t, h, body, steepNotesTitle, 2, 1, true)
		steepSnapshot(h, "two-windows")
		h.Mutate(func(m *Model) *Model {
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("an idle Windows screen scheduled work")
			}
			return m
		})

		// F8 focuses and raises Settings, and F8 again Notes.
		h.Key("f8")
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, true)
		h.WaitNot(steepNotesTitle)
		h.Key("f8")
		waitSteepWindow(t, h, body, steepNotesTitle, 2, 1, true)

		// Alt-Left moves Notes a column; a drag on its title moves it on.
		h.Key("alt+left")
		waitSteepWindow(t, h, body, steepNotesTitle, 1, 1, true)
		title := h.Locate(reel.Contains(steepNotesTitle))
		h.LeftDrag(title.X+2, title.Y, title.X+1, title.Y+1)
		waitSteepWindow(t, h, body, steepNotesTitle, 0, 2, true)
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
		h.Key("alt+left").Key("f8")
		settingsClose.LeftClick()
		h.KeyEsc().WaitNot("COMMAND PALETTE")
		h.Key("ctrl+p").Wait("COMMAND PALETTE")
		h.Type("stop notes").Key("enter").Wait("Stop Notes?")
		h.Key("alt+left").Key("f8").Key("ctrl+w")
		settingsClose.LeftClick()
		// Esc cancels: Cancel has focus.
		h.KeyEsc().WaitNot("Stop Notes?")
		h.Wait("Windows (2)")
		waitSteepWindow(t, h, body, steepSettingsTitle, 0, 0, false)
		waitSteepWindow(t, h, body, steepNotesTitle, 2, 1, true)
		steepAppRunning(t, client, "com.gostalgia.notes", true)
		// The windows take keys again once the modal is gone.
		h.Key("alt+left")
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
		h.Key("alt+left").Key("f8").Wait("Settings - Appea").WaitNot(steepNotesTitle)
		h.Key("ctrl+w").Wait([]string{"[Windows (1)]", steepNotesTitle})
		h.KeyEsc().WaitNot("[Windows (1)]")
		h.Key("f8").Wait([]string{"[Windows (1)]", steepNotesTitle})
		// Grown back, Notes floats where it was.
		h.Resize(80, 24).Wait(closeLabel)
		waitSteepWindow(t, h, steepBody(t, h), steepNotesTitle, 2, 1, true)
	})

	t.Run("reduced_motion", func(t *testing.T) {
		h, _ := newSteepWindowsRuntime(t, theme.Nostalgia().WithReducedMotion(true))
		h.Type("motion on").Key("enter").Wait("Reduced motion is now enabled")
		steepOpenTwoWindows(t, h)
		body := steepBody(t, h)
		waitSteepWindow(t, h, body, steepNotesTitle, 2, 1, true)
		h.Key("alt+right")
		waitSteepWindow(t, h, body, steepNotesTitle, 3, 1, true)
		h.Key("ctrl+w").Wait("Windows (1)")
		steepNoFrames(t, h)
	})
}
