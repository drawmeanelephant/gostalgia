package shell

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	xansi "github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/runtime"
)

func startupGeometry(width, height int) tea.Msg {
	return tea.WindowSizeMsg{Width: width, Height: height}
}

// startupConfig is the effective initial policy a real config/list answer
// carries: loaded marks the effective-config read, and configured marks an
// explicit non-default reduced_motion value in a higher layer.
func startupConfig(themeName string, reduced, configured bool) configUpdateMsg {
	return configUpdateMsg{
		loaded: true, theme: themeName,
		reducedMotion: &reduced, motionConfigured: configured,
	}
}

func assertNoAnimation(t *testing.T, cmd tea.Cmd, what string) {
	t.Helper()
	if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
		t.Fatalf("%s scheduled %d animation frames, want none", what, frames)
	}
}

// TestStartupRevealWaitsForEffectivePolicyAndGeometry covers both delivery
// orders: the reveal starts only once usable geometry and the effective
// initial policy are both known, begins at progress zero, changes the styled
// frame at its midpoint, and removes its effect at the endpoint.
func TestStartupRevealWaitsForEffectivePolicyAndGeometry(t *testing.T) {
	if startupRevealDuration <= 0 || startupRevealDuration > 200*time.Millisecond {
		t.Fatalf("startup reveal duration = %v, want a finite budget of at most 200ms", startupRevealDuration)
	}
	for _, policyFirst := range []bool{false, true} {
		name := "geometry_first"
		if policyFirst {
			name = "policy_first"
		}
		t.Run(name, func(t *testing.T) {
			clock := newFakeMotionClock()
			m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)

			deliverConfig := func() tea.Cmd {
				_, cmd := m.Update(startupConfig("nostalgia", false, true))
				return cmd
			}
			deliverGeometry := func() tea.Cmd {
				_, cmd := m.Update(startupGeometry(80, 24))
				return cmd
			}
			if policyFirst {
				assertNoAnimation(t, deliverConfig(), "initial policy before geometry")
				if m.motion.startupPhase != startupWaiting {
					t.Fatalf("startup phase before geometry = %v, want waiting", m.motion.startupPhase)
				}
				if _, ok := m.startupProgress(); ok {
					t.Fatal("startup attached a reveal before geometry")
				}
				cmd := deliverGeometry()
				frame := onlyAnimationFrame(t, cmd)
				if m.motion.startupPhase != startupRevealing {
					t.Fatalf("startup phase after geometry = %v, want revealing", m.motion.startupPhase)
				}
				if !m.motion.startup.Animating() || m.motion.startup.Float() != 0 {
					t.Fatalf("startup did not begin at progress zero: %v (animating %v)",
						m.motion.startup.Float(), m.motion.startup.Animating())
				}
				if m.kit.Theme().ReducedMotion {
					t.Fatal("enabled initial policy was reduced")
				}
				if _, ok := m.startupProgress(); !ok {
					t.Fatal("startup did not attach its reveal")
				}
				start := m.View().Content
				if got := strings.TrimSpace(xansi.Strip(start)); got != "" {
					t.Fatalf("startup frame is not blank at progress zero: %q", got)
				}
				clock.Advance(startupRevealDuration / 4)
				_, cmd = m.Update(frame)
				frame = onlyAnimationFrame(t, cmd)
				mid := m.View().Content
				if progress, ok := m.startupProgress(); !ok || progress <= 0 || progress >= 1 {
					t.Fatalf("midpoint startup progress = %v (attached %v), want strictly between 0 and 1", progress, ok)
				}
				if mid == start {
					t.Fatal("startup midpoint is identical to its blank start")
				}
				clock.Advance(startupRevealDuration)
				_, cmd = m.Update(frame)
				if cmd != nil {
					t.Fatalf("startup endpoint scheduled %v", messageTypes(commandMessages(t, cmd)))
				}
				if _, ok := m.startupProgress(); ok {
					t.Fatal("startup kept its transition at the endpoint")
				}
				if m.motion.startupPhase != startupConsumed {
					t.Fatalf("startup phase at endpoint = %v, want consumed", m.motion.startupPhase)
				}
				settled := m.View().Content
				reference := motionModel(theme.Nostalgia(), ui.ANSI256, newFakeMotionClock())
				if settled != reference.View().Content {
					t.Fatal("settled startup frame differs from a model that never revealed")
				}
				return
			}

			assertNoAnimation(t, deliverGeometry(), "geometry before initial policy")
			if m.motion.startupPhase != startupWaiting {
				t.Fatalf("startup phase before policy = %v, want waiting", m.motion.startupPhase)
			}
			if _, ok := m.startupProgress(); ok {
				t.Fatal("startup attached a reveal before the initial policy")
			}
			cmd := deliverConfig()
			frame := onlyAnimationFrame(t, cmd)
			if m.motion.startupPhase != startupRevealing || !m.motion.startup.Animating() {
				t.Fatal("startup did not begin after the initial policy arrived")
			}
			start := m.View().Content
			if got := strings.TrimSpace(xansi.Strip(start)); got != "" {
				t.Fatalf("startup frame is not blank at progress zero: %q", got)
			}
			clock.Advance(startupRevealDuration / 2)
			_, cmd = m.Update(frame)
			frame = onlyAnimationFrame(t, cmd)
			if progress, ok := m.startupProgress(); !ok || progress <= 0 || progress >= 1 {
				t.Fatalf("midpoint startup progress = %v (attached %v), want strictly between 0 and 1", progress, ok)
			}
			if m.View().Content == start {
				t.Fatal("startup midpoint is identical to its blank start")
			}
			clock.Advance(startupRevealDuration)
			_, cmd = m.Update(frame)
			if cmd != nil {
				t.Fatalf("startup endpoint scheduled %v", messageTypes(commandMessages(t, cmd)))
			}
			if _, ok := m.startupProgress(); ok || m.motion.startupPhase != startupConsumed {
				t.Fatal("startup did not consume and remove its effect at the endpoint")
			}
		})
	}
}

// TestStartupRevealHonorsTheEffectiveInitialPolicy checks that the effective
// policy, not just the built-in theme default, decides whether the reveal
// happens. ReducedMotion starts settled with no frame command; an explicit
// non-default enabled value wins over a default-reduced theme; `motion on`
// remains ReducedMotion.
func TestStartupRevealHonorsTheEffectiveInitialPolicy(t *testing.T) {
	t.Run("initial reduced starts settled", func(t *testing.T) {
		for _, policyFirst := range []bool{false, true} {
			clock := newFakeMotionClock()
			m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
			if policyFirst {
				assertNoAnimation(t, func() tea.Cmd {
					_, cmd := m.Update(startupConfig("nostalgia", true, true))
					return cmd
				}(), "reduced policy before geometry")
				assertNoAnimation(t, func() tea.Cmd {
					_, cmd := m.Update(startupGeometry(80, 24))
					return cmd
				}(), "geometry after reduced policy")
			} else {
				assertNoAnimation(t, func() tea.Cmd {
					_, cmd := m.Update(startupGeometry(80, 24))
					return cmd
				}(), "geometry before reduced policy")
				assertNoAnimation(t, func() tea.Cmd {
					_, cmd := m.Update(startupConfig("nostalgia", true, true))
					return cmd
				}(), "reduced policy after geometry")
			}
			if m.motion.startupPhase != startupConsumed {
				t.Fatalf("intact reduced policy did not consume startup: phase %v", m.motion.startupPhase)
			}
			if _, ok := m.startupProgress(); ok {
				t.Fatal("intact reduced policy attached a startup reveal")
			}
		}
	})

	t.Run("explicit enabled wins over a reduced theme default", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Monochrome(), ui.Plain, clock)
		assertNoAnimation(t, func() tea.Cmd {
			_, cmd := m.Update(startupGeometry(80, 24))
			return cmd
		}(), "geometry")
		cmd := func() tea.Cmd {
			_, c := m.Update(startupConfig("monochrome", false, true))
			return c
		}()
		onlyAnimationFrame(t, cmd)
		if m.kit.Theme().ReducedMotion {
			t.Fatal("explicit enabled config did not override the theme default")
		}
	})

	t.Run("motion on starts settled", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		reduced := true
		m.Update(resultMsg{setMotion: &reduced})
		assertNoAnimation(t, func() tea.Cmd {
			_, cmd := m.Update(startupGeometry(80, 24))
			return cmd
		}(), "geometry")
		assertNoAnimation(t, func() tea.Cmd {
			_, cmd := m.Update(startupConfig("nostalgia", false, true))
			return cmd
		}(), "enabled config after motion on")
		if !m.kit.Theme().ReducedMotion || m.motion.startupPhase != startupConsumed {
			t.Fatal("`motion on` did not start settled and consume startup")
		}
	})
}

// TestStartupRevealNeverReplays covers a completed reveal and a canceled one.
// Existing polls, theme switches, resizes, repeated renders, a rebuilt root,
// stale frames, and later policy changes schedule nothing and restart nothing.
func TestStartupRevealNeverReplays(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		m.Update(startupGeometry(80, 24))
		_, cmd := m.Update(startupConfig("nostalgia", false, true))
		frame := onlyAnimationFrame(t, cmd)
		clock.Advance(startupRevealDuration)
		m.Update(frame)

		oldRoot := m.root()
		for _, trigger := range []struct {
			name string
			msg  tea.Msg
		}{
			{"config poll", startupConfig("nostalgia", false, true)},
			{"theme switch", resultMsg{setTheme: "midnight"}},
			{"resize small", startupGeometry(30, 10)},
			{"resize back", startupGeometry(80, 24)},
			{"idle update", struct{}{}},
			{"functional", tickMsg(clock.Now())},
		} {
			_, cmd := m.Update(trigger.msg)
			assertNoAnimation(t, cmd, trigger.name)
			if _, ok := m.startupProgress(); ok {
				t.Fatalf("%s reattached startup", trigger.name)
			}
		}
		if m.root() == oldRoot {
			t.Fatal("theme/geometry changes did not rebuild the root, so the no-replay case is unproven")
		}
		_ = m.View()
		_ = m.View()
		static := m.View().Content
		clock.Advance(time.Second)
		_, cmd = m.Update(frame)
		if cmd != nil {
			t.Fatalf("a stale startup frame scheduled %v", messageTypes(commandMessages(t, cmd)))
		}
		if got := m.View().Content; got != static {
			t.Fatal("a stale startup frame changed the settled frame")
		}
		if _, ok := m.startupProgress(); ok || m.motion.startupPhase != startupConsumed {
			t.Fatal("startup was not consumed after completion")
		}
	})

	t.Run("theme switch to reduced cancels", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		m.Update(startupGeometry(80, 24))
		_, cmd := m.Update(startupConfig("nostalgia", false, false))
		frame := onlyAnimationFrame(t, cmd)
		clock.Advance(startupRevealDuration / 4)
		m.Update(frame)
		_, cmd = m.Update(resultMsg{setTheme: "monochrome"})
		assertNoAnimation(t, cmd, "theme switch to a reduced default")
		if !m.kit.Theme().ReducedMotion || m.motion.startupPhase != startupConsumed {
			t.Fatalf("a reduced theme switch did not cancel and consume startup: theme %s reduced %v phase %v",
				m.kit.Theme().Name, m.kit.Theme().ReducedMotion, m.motion.startupPhase)
		}
		if _, ok := m.startupProgress(); ok {
			t.Fatal("a reduced theme switch kept the startup reveal attached")
		}
		_, cmd = m.Update(resultMsg{setTheme: "nostalgia"})
		assertNoAnimation(t, cmd, "theme switch back")
		if _, ok := m.startupProgress(); ok {
			t.Fatal("switching back to an enabled theme replayed startup")
		}
		clock.Advance(time.Second)
		_, cmd = m.Update(frame)
		assertNoAnimation(t, cmd, "stale frame after a theme cancellation")
		if m.View().Content == "" {
			t.Fatal("theme cancellation produced an empty frame")
		}
	})

	t.Run("canceled mid-effect", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		m.Update(startupGeometry(80, 24))
		_, cmd := m.Update(startupConfig("nostalgia", false, true))
		frame := onlyAnimationFrame(t, cmd)
		clock.Advance(startupRevealDuration / 4)
		m.Update(frame)
		if progress, ok := m.startupProgress(); !ok || progress <= 0 {
			t.Fatalf("startup was not visible before cancellation: %v attached %v", progress, ok)
		}

		_, cmd = m.Update(startupConfig("nostalgia", true, true))
		assertNoAnimation(t, cmd, "canceling startup")
		if _, ok := m.startupProgress(); ok {
			t.Fatal("ReducedMotion kept the startup reveal attached")
		}
		if m.motion.startupPhase != startupConsumed {
			t.Fatalf("canceled startup phase = %v, want consumed", m.motion.startupPhase)
		}
		static := m.View().Content
		clock.Advance(time.Second)
		_, cmd = m.Update(frame)
		assertNoAnimation(t, cmd, "stale canceled frame")
		if m.View().Content != static {
			t.Fatal("a stale frame revived the canceled startup")
		}

		enabled := false
		_, cmd = m.Update(startupConfig("nostalgia", enabled, true))
		assertNoAnimation(t, cmd, "re-enabling after cancellation")
		if _, ok := m.startupProgress(); ok {
			t.Fatal("re-enabling motion replayed the canceled startup")
		}
		for _, msg := range []tea.Msg{
			startupGeometry(30, 10),
			startupGeometry(80, 24),
			struct{}{},
			tickMsg(clock.Now()),
		} {
			_, cmd = m.Update(msg)
			assertNoAnimation(t, cmd, "post-cancellation trigger")
		}
		if m.motion.startupPhase != startupConsumed {
			t.Fatal("post-cancellation triggers changed the once-only startup state")
		}
	})
}

// TestAttachedModelsOwnStartupAndPolicy runs two attached shell models that
// share one real runtime. Their clocks, effective policies and once-only
// startup state stay independent, and canceling one never touches the other.
func TestAttachedModelsOwnStartupAndPolicy(t *testing.T) {
	root := t.TempDir()
	rt, first, err := bootShellTestRuntime(root, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("startup attach test cleanup") })
	t.Cleanup(func() { first.Close() })
	second, err := dialShellTestRuntime(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })

	now := time.Unix(1_700_000_000, 0)
	enabledClock := &fakeMotionClock{now: now}
	reducedClock := &fakeMotionClock{now: now}
	enabled := NewWithThemeAndNotifs(context.Background(), first, rt.Done(), theme.Nostalgia(), ui.ANSI256,
		notifications.NewManager(100, 3, 8))
	reduced := NewWithThemeAndNotifs(context.Background(), second, rt.Done(), theme.Nostalgia(), ui.ANSI256,
		notifications.NewManager(100, 3, 8))
	enabled.motion.now = enabledClock.Now
	reduced.motion.now = reducedClock.Now
	for _, m := range []*Model{enabled, reduced} {
		m.SetAttached(true)
		m.motion.ready = false
		m.syncMotion()
		m.setMode(modePrompt)
	}

	for _, m := range []*Model{enabled, reduced} {
		m.Update(startupGeometry(80, 24))
	}
	_, enabledCmd := enabled.Update(startupConfig("nostalgia", false, true))
	enabledFrame := onlyAnimationFrame(t, enabledCmd)
	_, reducedCmd := reduced.Update(startupConfig("nostalgia", true, true))
	assertNoAnimation(t, reducedCmd, "reduced attached model")
	if enabled.motion.startupPhase != startupRevealing || reduced.motion.startupPhase != startupConsumed {
		t.Fatal("attached models did not start with independent policies")
	}

	enabledClock.Advance(startupRevealDuration / 2)
	enabled.Update(enabledFrame)
	reducedStatic := reduced.View().Content
	if reducedClock.Now() != now {
		t.Fatal("advancing one model's clock moved the other's")
	}
	if _, ok := reduced.startupProgress(); ok {
		t.Fatal("the reduced attached model attached a startup reveal")
	}
	if reduced.View().Content != reducedStatic {
		t.Fatal("the other model's startup progress changed this model's frame")
	}

	// Cancel only the enabled model mid-effect; the already-consumed reduced
	// model keeps its state.
	if enabled.Theme().ReducedMotion || !reduced.Theme().ReducedMotion {
		t.Fatal("attached models shared one effective policy before cancellation")
	}
	_, cancelCmd := enabled.Update(startupConfig("nostalgia", true, true))
	assertNoAnimation(t, cancelCmd, "canceling one attached model")
	if enabled.motion.startupPhase != startupConsumed || reduced.motion.startupPhase != startupConsumed {
		t.Fatal("one model's cancellation changed shared startup state")
	}
	if _, ok := enabled.startupProgress(); ok {
		t.Fatal("the canceled attached model kept the reveal")
	}
	if !reduced.Theme().ReducedMotion || reduced.View().Content != reducedStatic {
		t.Fatal("canceling one model changed the other model's policy or frame")
	}

	// Re-enabling the canceled model does not replay startup.
	enabledOff := false
	assertNoAnimation(t, func() tea.Cmd {
		_, cmd := enabled.Update(startupConfig("nostalgia", enabledOff, true))
		return cmd
	}(), "re-enabling one attached model")
	if _, ok := enabled.startupProgress(); ok {
		t.Fatal("the canceled attached model replayed startup")
	}
}
