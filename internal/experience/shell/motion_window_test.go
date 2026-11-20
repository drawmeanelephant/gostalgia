package shell

import (
	"context"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
)

// windowMotionModel is a motion-enabled shell on the controlled clock with
// two running apps, Alpha and Beta, whose views load screenData.
func windowMotionModel(th theme.Theme, clock *fakeMotionClock) *Model {
	m := motionModel(th, ui.ANSI256, clock)
	m.client = callerFunc(func(ctx context.Context, method string, params, out any) error {
		return writeJSON(out, screenData())
	})
	m.apps = []appStatus{windowApp(string(alphaID), 1), windowApp(string(betaID), 2)}
	return m
}

// deliverOpen runs the commands an opening returned, delivers the window's
// first snapshot, and returns the animation frames the opening scheduled.
func deliverOpen(t *testing.T, m *Model, cmd tea.Cmd) []anim.FrameMsg {
	t.Helper()
	var frames []anim.FrameMsg
	for _, msg := range commandMessages(t, cmd) {
		switch msg := msg.(type) {
		case anim.FrameMsg:
			frames = append(frames, msg)
		case viewMsg:
			m.Update(msg)
		}
	}
	return frames
}

// revealFrame is the one frame an opening with motion schedules.
func revealFrame(t *testing.T, m *Model, cmd tea.Cmd) anim.FrameMsg {
	t.Helper()
	frames := deliverOpen(t, m, cmd)
	if len(frames) != 1 {
		t.Fatalf("opening scheduled %d animation frames, want 1", len(frames))
	}
	return frames[0]
}

// TestWindowRevealLifecycle steps an opening window's reveal on the
// controlled clock: progress zero at the opening, a visible midpoint, an
// endpoint that drops the effect and schedules nothing, the settled
// ReducedMotion frame at the end, and stale frames that cannot revive it.
func TestWindowRevealLifecycle(t *testing.T) {
	clock := newFakeMotionClock()
	m := windowMotionModel(theme.Nostalgia(), clock)
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	frame := revealFrame(t, m, cmd)
	if !m.onWindows() || m.FocusedWindow() != string(alphaID) {
		t.Fatalf("F4 did not open Alpha's window: focused %q", m.FocusedWindow())
	}
	if progress, ok := m.windowProgress(alphaID); !ok || progress != 0 {
		t.Fatalf("the reveal did not start at progress zero: %v (attached %v)", progress, ok)
	}
	start := m.View().Content

	clock.Advance(windowRevealDuration / 4)
	_, cmd = m.Update(frame)
	entranceFrame(t, cmd)
	if progress, ok := m.windowProgress(alphaID); !ok || progress <= 0 || progress >= 1 {
		t.Fatalf("midpoint progress = %v (attached %v), want strictly between 0 and 1", progress, ok)
	}
	mid := m.View().Content
	if mid == start {
		t.Fatal("the reveal did not change the styled frame")
	}

	clock.Advance(windowRevealDuration)
	_, cmd = m.Update(frame)
	if cmd != nil {
		t.Fatalf("endpoint scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if _, ok := m.windowProgress(alphaID); ok {
		t.Fatal("the settled reveal still attached a transition")
	}
	settled := m.View().Content

	reference := windowMotionModel(theme.Nostalgia().WithReducedMotion(true), newFakeMotionClock())
	_, cmd = reference.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if frames := deliverOpen(t, reference, cmd); len(frames) != 0 {
		t.Fatalf("the ReducedMotion reference scheduled %d frames", len(frames))
	}
	if settled != reference.View().Content {
		t.Fatal("the endpoint frame differs from the settled ReducedMotion frame")
	}

	clock.Advance(time.Second)
	if _, cmd = m.Update(frame); cmd != nil {
		t.Fatalf("a stale frame scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if m.View().Content != settled {
		t.Fatal("a stale frame changed the settled frame")
	}
	if _, cmd = m.Update(struct{}{}); cmd != nil {
		t.Fatalf("an idle Windows screen scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
}

// TestWindowRevealBelongsToTheNewestOpening verifies only an opening
// reveals: a second window takes the reveal from the first, and focusing,
// raising, moving, and closing are instant. Closing a revealing window ends
// its frames.
func TestWindowRevealBelongsToTheNewestOpening(t *testing.T) {
	clock := newFakeMotionClock()
	m := windowMotionModel(theme.Nostalgia(), clock)
	alphaFrame := revealFrame(t, m, m.openWindow(m.apps[0]))
	clock.Advance(windowRevealDuration / 4)
	m.Update(alphaFrame)

	betaFrame := revealFrame(t, m, m.openWindow(m.apps[1]))
	if _, ok := m.windowProgress(alphaID); ok {
		t.Fatal("the older window kept its reveal")
	}
	if progress, ok := m.windowProgress(betaID); !ok || progress != 0 {
		t.Fatalf("the newer window's reveal = %v (attached %v), want progress zero", progress, ok)
	}
	if _, cmd := m.Update(alphaFrame); cmd != nil {
		t.Fatalf("the older reveal's frame scheduled %v", messageTypes(commandMessages(t, cmd)))
	}

	for _, msg := range []tea.Msg{
		tea.KeyPressMsg{Code: tea.KeyF8},
		tea.KeyPressMsg{Code: tea.KeyF8},
		alt(tea.KeyLeft),
	} {
		_, cmd := m.Update(msg)
		if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
			t.Fatalf("%v scheduled %d animation frames", msg, frames)
		}
	}
	if cmd := m.openWindow(m.apps[0]); cmd != nil {
		t.Fatalf("focusing an open window scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if m.FocusedWindow() != string(alphaID) {
		t.Fatalf("opening Alpha again focused %q", m.FocusedWindow())
	}

	// F8 focuses Beta, which is still revealing, and Ctrl-W closes it.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF8})
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
		t.Fatalf("closing a window scheduled %d animation frames", frames)
	}
	if m.views[betaID] != nil || m.windows.Len() != 1 {
		t.Fatalf("Ctrl-W did not close Beta: open %v", m.windows.Order())
	}
	clock.Advance(windowRevealDuration / 4)
	if _, cmd = m.Update(betaFrame); cmd != nil {
		t.Fatalf("a closed window's reveal scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if _, ok := m.windowProgress(betaID); ok {
		t.Fatal("a closed window kept its reveal")
	}
}

// TestWindowRevealReducedMotionSkipsAndCancels covers the motion policy: an
// initially reduced shell attaches no reveal, a switch mid-reveal settles it
// and invalidates its frame, and turning motion back on replays nothing. A
// body below the floor draws no window frame, so nothing reveals.
func TestWindowRevealReducedMotionSkipsAndCancels(t *testing.T) {
	t.Run("initial reduced", func(t *testing.T) {
		m := windowMotionModel(theme.Nostalgia().WithReducedMotion(true), newFakeMotionClock())
		if frames := deliverOpen(t, m, m.openWindow(m.apps[0])); len(frames) != 0 {
			t.Fatalf("ReducedMotion scheduled %d frames", len(frames))
		}
		if !m.onWindows() {
			t.Fatal("ReducedMotion did not open the window")
		}
		if _, ok := m.windowProgress(alphaID); ok {
			t.Fatal("ReducedMotion attached a reveal")
		}
	})

	t.Run("cancelled mid-effect", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := windowMotionModel(theme.Nostalgia(), clock)
		frame := revealFrame(t, m, m.openWindow(m.apps[0]))
		clock.Advance(windowRevealDuration / 4)
		m.Update(frame)

		reduced := true
		_, cmd := m.Update(configUpdateMsg{reducedMotion: &reduced})
		if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
			t.Fatalf("switching to ReducedMotion scheduled %d frames", frames)
		}
		if _, ok := m.windowProgress(alphaID); ok || m.motion.window.Float() != 1 {
			t.Fatal("ReducedMotion did not settle the active reveal")
		}
		static := m.View().Content

		clock.Advance(time.Second)
		if _, cmd = m.Update(frame); cmd != nil {
			t.Fatalf("a stale frame scheduled %v", messageTypes(commandMessages(t, cmd)))
		}
		if m.View().Content != static {
			t.Fatal("a stale frame revived the cancelled reveal")
		}

		enabled := false
		_, cmd = m.Update(configUpdateMsg{reducedMotion: &enabled})
		if animationFrames(commandMessages(t, cmd)) != 0 {
			t.Fatal("turning motion back on replayed the reveal")
		}
		if _, ok := m.windowProgress(alphaID); ok {
			t.Fatal("turning motion back on reattached the reveal")
		}
	})

	t.Run("body below the floor", func(t *testing.T) {
		m := windowMotionModel(theme.Nostalgia(), newFakeMotionClock())
		m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
		if frames := deliverOpen(t, m, m.openWindow(m.apps[0])); len(frames) != 0 {
			t.Fatalf("a window below the floor scheduled %d frames", len(frames))
		}
		if !m.onWindows() || m.floatingWindows() {
			t.Fatal("the window did not open in its small-terminal form")
		}
	})
}

// TestWindowRevealTakesInputFromTheFirstFrame sends keys and a click to a
// window at progress zero, while the reveal still hides every cell of it.
func TestWindowRevealTakesInputFromTheFirstFrame(t *testing.T) {
	t.Run("keys", func(t *testing.T) {
		m := windowMotionModel(theme.Nostalgia(), newFakeMotionClock())
		revealFrame(t, m, m.openWindow(m.apps[0]))
		m.View()
		typeText(m, "first frame")
		if _, ok := m.windowProgress(alphaID); !ok {
			t.Fatal("the reveal ended before the keys arrived")
		}
		if got := m.focusedView().values["text"]; got != "first frame" {
			t.Fatalf("keys at the first frame reached the window as %q", got)
		}
	})

	t.Run("pointer", func(t *testing.T) {
		m, _ := mouseModel(t, 80, 24)
		m.motion.now = newFakeMotionClock().Now
		m.motion.ready = false
		m.syncMotion()
		echo := m.apps[0]
		id := windows.ID(echo.Manifest.ID)
		if frames := animationFrames(commandMessages(t, m.openWindow(echo))); frames != 1 {
			t.Fatalf("opening Echo scheduled %d animation frames, want 1", frames)
		}
		closeRect := rectOf(t, m, windowPart{id, windowClose})
		if got := row(m, closeRect.Min.Y, closeRect.Min.X, closeRect.Max.X); got == closeLabel {
			t.Fatal("the close control showed before the reveal reached it")
		}
		p := closeRect.Min
		for _, msg := range []tea.Msg{
			tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft},
			tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft},
		} {
			if _, ok := m.windowProgress(id); !ok {
				t.Fatal("the reveal ended before the click arrived")
			}
			m.View()
			_, cmd := m.Update(msg)
			drive(t, m, cmd)
		}
		if m.windows.Len() != 0 || m.onWindows() {
			t.Fatalf("a click on the hidden close control left %v open", m.windows.Order())
		}
	})
}
