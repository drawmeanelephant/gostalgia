package shell

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
	"getsteep.dev/steep"
	"getsteep.dev/steep/reel"
	xansi "github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/runtime"
)

func newSteepMotionRuntime(t *testing.T, th theme.Theme) (*steep.Harness, *fakeMotionClock) {
	t.Helper()
	rt, client, err := bootShellTestRuntime(t.TempDir(), runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("steep motion cleanup") })
	t.Cleanup(func() { client.Close() })
	m := NewWithThemeAndNotifs(context.Background(), client, rt.Done(), th, ui.ANSI256, notifications.NewManager(100, 3, 8))
	clock := newFakeMotionClock()
	m.motion.now = clock.Now
	m.motion.ready = false
	m.motion.startupPhase = startupConsumed // these scenarios own only their event effects
	m.syncMotion()
	m.setMode(modePrompt)
	h := steep.NewHarness(t, m,
		steep.WithWindowSize(80, 24),
		steep.WithTimeout(30*time.Second),
		steep.WithColor(steep.ColorANSI256),
		steep.WithRecordMessages(true),
	)
	h.Wait("Services: 15")
	return h, clock
}

func waitSteepMotionBadge(t *testing.T, h *steep.Harness, want string) {
	t.Helper()
	want = strings.TrimRight(xansi.Strip(want), " ")
	h.WaitFor(reel.ScreenFunc(func(screen string) bool {
		lines := strings.Split(screen, "\n")
		return len(lines) > 3 && strings.TrimRight(lines[3], " ") == want
	}))
}

// countMessages returns how many messages of type T the program has
// recorded so far.
func countMessages[T any](h *steep.Harness) int {
	n := 0
	for msg := range h.MessageHistory() {
		if _, ok := msg.(T); ok {
			n++
		}
	}
	return n
}

// waitMoreMessages waits until more than n messages of type T have been
// recorded, scanning recorded history as well as live arrivals. It is the
// race-free form of WaitLiveMessage: a message delivered before this call
// still counts, so fast input on a loaded runner is never missed.
func waitMoreMessages[T any](t *testing.T, h *steep.Harness, n int) {
	t.Helper()
	seen := 0
	h.WaitMessageWhere(func(T) bool {
		seen++
		return seen > n
	})
}

// keyPress sends key through the terminal and waits for its KeyPressMsg to
// reach the program, which is what the waits after Key calls exist to
// prove before the next assertion runs.
func keyPress(t *testing.T, h *steep.Harness, key string) {
	t.Helper()
	before := countMessages[tea.KeyPressMsg](h)
	h.Key(key)
	waitMoreMessages[tea.KeyPressMsg](t, h, before)
}

// stepSteepMotion sets the injected clock to target and waits for scheduled
// frames until satisfied reports the expected effect state. A frame left over
// from an earlier generation advances nothing, so the loop retries with the
// next one.
func stepSteepMotion(t *testing.T, h *steep.Harness, clock *fakeMotionClock, target time.Time,
	read func(*Model) (progress float64, animating bool),
	satisfied func(progress float64, animating bool) bool) {
	t.Helper()
	for range 5 {
		h.Mutate(func(m *Model) *Model {
			clock.Set(target)
			return m
		})
		var progress float64
		var animating bool
		h.Mutate(func(m *Model) *Model {
			progress, animating = read(m)
			return m
		})
		if satisfied(progress, animating) {
			return
		}
		waitMoreMessages[anim.FrameMsg](t, h, countMessages[anim.FrameMsg](h))
	}
	t.Fatal("finite motion never advanced on the injected clock")
}

func TestSteepMotionScenarios(t *testing.T) {
	t.Run("failed_launch_clock_steps", func(t *testing.T) {
		h, clock := newSteepMotionRuntime(t, theme.Nostalgia())
		h.Type("launch com.test.missing").Key("enter").Wait("unknown application")
		frame := h.WaitMessage[anim.FrameMsg]()
		steepSnapshot(h, "start")
		initialBadge := strings.Split(h.View(), "\n")[3]

		var midBadge string
		h.Mutate(func(m *Model) *Model {
			clock.Advance(20 * time.Millisecond)
			return m
		}).SendProgram(frame).Mutate(func(m *Model) *Model {
			if got := m.motion.launchShake.Float(); got != 0.125 {
				t.Fatalf("steep progress at 20ms = %v, want 0.125", got)
			}
			midBadge = strings.Split(m.View().Content, "\n")[3]
			return m
		})
		if strings.TrimRight(xansi.Strip(midBadge), " ") == strings.TrimRight(initialBadge, " ") {
			t.Fatal("20ms badge is identical to its starting position")
		}
		waitSteepMotionBadge(t, h, midBadge)
		steepSnapshot(h, "20ms")

		var settledBadge string
		h.Mutate(func(m *Model) *Model {
			clock.Advance(launchShakeDuration - 20*time.Millisecond)
			return m
		}).SendProgram(frame).Mutate(func(m *Model) *Model {
			if m.motion.launchShake.Animating() || m.motion.launchShake.Float() != 1 {
				t.Fatal("steep transition did not settle at 160ms")
			}
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("settled steep shell scheduled idle work")
			}
			settledBadge = strings.Split(m.View().Content, "\n")[3]
			return m
		})
		waitSteepMotionBadge(t, h, settledBadge)
		steepSnapshot(h, "settled")
		h.Type("echo keyboard-still-works").Key("enter").Wait("[echo #1]")
	})

	t.Run("reduced_motion_static", func(t *testing.T) {
		h, clock := newSteepMotionRuntime(t, theme.Nostalgia().WithReducedMotion(true))
		// Initial real config discovery selects Nostalgia's defaults. Set this
		// session's reduced policy through the same command an operator uses.
		h.Type("motion on").Key("enter").Wait("Reduced motion is now enabled")
		h.Type("launch com.test.missing").Key("enter").Wait("unknown application")
		steepSnapshot(h, "reduced")
		h.Mutate(func(m *Model) *Model {
			clock.Advance(launchShakeDuration)
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("ReducedMotion steep shell scheduled idle work")
			}
			return m
		})
		for msg := range h.MessageHistory() {
			if _, ok := msg.(anim.FrameMsg); ok {
				t.Fatal("ReducedMotion real-runtime scenario emitted an animation frame")
			}
		}
		if got := len(slices.Collect(h.MessageHistory())); got == 0 {
			t.Fatal("real-runtime scenario recorded no messages")
		}
		h.Type("echo reduced-keyboard").Key("enter").Wait("[echo #1]")
	})

	t.Run("overlay_entrances", func(t *testing.T) {
		h, clock := newSteepMotionRuntime(t, theme.Nostalgia())
		base := clock.Now()
		readOverlay := func(m *Model) (float64, bool) {
			return m.motion.overlay.Float(), m.motion.overlay.Animating()
		}
		midReached := func(progress float64, animating bool) bool {
			return animating && progress > 0 && progress < 1
		}
		settled := func(progress float64, animating bool) bool {
			return !animating && progress == 1
		}

		frames := countMessages[anim.FrameMsg](h)
		h.Key("ctrl+p")
		waitMoreMessages[anim.FrameMsg](t, h, frames)
		var start string
		h.Mutate(func(m *Model) *Model {
			if !m.motion.overlay.Animating() || m.motion.overlay.Float() != 0 {
				t.Fatalf("palette start: progress %v animating %v", m.motion.overlay.Float(), m.motion.overlay.Animating())
			}
			start = m.View().Content
			return m
		})
		stepSteepMotion(t, h, clock, base.Add(overlayRevealDuration/4), readOverlay, midReached)
		h.Mutate(func(m *Model) *Model {
			if m.View().Content == start {
				t.Fatal("palette midpoint frame is identical to its start")
			}
			return m
		})
		stepSteepMotion(t, h, clock, base.Add(overlayRevealDuration), readOverlay, settled)
		steepSnapshot(h, "palette-settled")
		// Actual routed input still reaches the settled palette.
		keyPress(t, h, "down")
		h.Mutate(func(m *Model) *Model {
			if m.paletteSelected != 1 {
				t.Fatalf("Down in the settled palette selected %d", m.paletteSelected)
			}
			return m
		})
		keyPress(t, h, "esc")
		h.Mutate(func(m *Model) *Model {
			if m.paletteOpen {
				t.Fatal("Escape did not dismiss the settled palette")
			}
			return m
		})
		h.Type("echo palette-closed").Key("enter").Wait("[echo #1]")

		// A routed command opens the dialog; Enter on the initial Cancel is
		// safe at the midpoint, and the settled dialog still cancels.
		base = clock.Now()
		frames = countMessages[anim.FrameMsg](h)
		h.Type("shutdown").Key("enter")
		waitMoreMessages[anim.FrameMsg](t, h, frames)
		h.Mutate(func(m *Model) *Model {
			if m.confirm == nil || m.confirm.active != confirmCancel {
				t.Fatal("dialog did not open on Cancel")
			}
			return m
		})
		stepSteepMotion(t, h, clock, base.Add(overlayRevealDuration/4), readOverlay, midReached)
		keyPress(t, h, "enter")
		h.Mutate(func(m *Model) *Model {
			if m.confirm != nil {
				t.Fatal("Enter on the initial Cancel left the dialog open")
			}
			return m
		})
		// Settle the dialog's remaining frames so they cannot leak into the
		// next scenario.
		stepSteepMotion(t, h, clock, base.Add(overlayRevealDuration), readOverlay, settled)
		// A typed destructive line keeps its draft for editing after Cancel.
		keyPress(t, h, "ctrl+u")
		h.Type("echo dialog-cancelled").Key("enter").Wait("[echo #2]")
	})

	t.Run("toast_entrance", func(t *testing.T) {
		h, clock := newSteepMotionRuntime(t, theme.Nostalgia())
		base := clock.Now()
		readToast := func(m *Model) (float64, bool) {
			return m.motion.toast.Float(), m.motion.toast.Animating()
		}
		// The shared manager is the same one the runtime's notification sink
		// writes into; the exec-harness toast scenario covers real delivery.
		h.Mutate(func(m *Model) *Model {
			m.notifs.Record(notifications.Notification{
				ID: "steep-motion-toast", Timestamp: clock.Now(),
				Title: "STEEP MOTION TOAST", Message: "entrance",
			})
			return m
		})
		frames := countMessages[anim.FrameMsg](h)
		h.SendProgram(tickMsg(clock.Now()))
		waitMoreMessages[anim.FrameMsg](t, h, frames)
		h.Mutate(func(m *Model) *Model {
			if m.motion.toastID != "steep-motion-toast" || m.motion.toast.Float() != 0 {
				t.Fatalf("toast start: id %q progress %v", m.motion.toastID, m.motion.toast.Float())
			}
			return m
		})
		stepSteepMotion(t, h, clock, base.Add(toastEntranceDuration/2), readToast,
			func(progress float64, animating bool) bool { return animating && progress > 0 && progress < 1 })
		stepSteepMotion(t, h, clock, base.Add(toastEntranceDuration), readToast,
			func(progress float64, animating bool) bool { return !animating && progress == 1 })
		h.Wait("STEEP MOTION TOAST")
		steepSnapshot(h, "toast-settled")

		// An unchanged functional tick after completion schedules no frame.
		h.SendProgram(tickMsg(clock.Now()))
		h.Mutate(func(m *Model) *Model {
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("settled toast shell scheduled idle work")
			}
			return m
		})
		h.Type("echo toast-keyboard").Key("enter").Wait("[echo #1]")
	})
}
