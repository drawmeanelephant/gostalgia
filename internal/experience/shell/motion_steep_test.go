package shell

import (
	"context"
	"slices"
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
}
