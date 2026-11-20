package shell

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
	"getsteep.dev/steep"
	xansi "github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/runtime"
)

// newSteepStartupRuntime boots the real runtime and shell with this test's
// clock injected before startup eligibility. The optional settings file is
// written before boot, so a ReducedMotion policy arrives through the same
// initial runtime configuration production reads.
func newSteepStartupRuntime(t *testing.T, reduced bool) (*steep.Harness, *fakeMotionClock) {
	t.Helper()
	root := t.TempDir()
	if reduced {
		if err := runtime.InitRoot(root); err != nil {
			t.Fatal(err)
		}
		configDir := filepath.Join(root, "vfs", "users", "guest", "config")
		if err := os.MkdirAll(configDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
			[]byte(`{"accessibility":{"reduced_motion":true}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rt, client, err := bootShellTestRuntime(root, runtime.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Shutdown("steep startup cleanup") })
	t.Cleanup(func() { client.Close() })
	m := NewWithThemeAndNotifs(context.Background(), client, rt.Done(), theme.Nostalgia(), ui.ANSI256,
		notifications.NewManager(100, 3, 8))
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
	return h, clock
}

// TestSteepStartupReveal runs the real runtime harness: the reveal waits for
// the routed initial configuration and delivered geometry, starts blank,
// advances on the injected clock, and settles at its endpoint.
func TestSteepStartupReveal(t *testing.T) {
	h, clock := newSteepStartupRuntime(t, false)
	frame := h.WaitMessage[anim.FrameMsg]()
	var start string
	h.Mutate(func(m *Model) *Model {
		if !m.initialPolicyKnown || !m.geometryKnown {
			t.Fatalf("startup began before policy/geometry were known: policy %v geometry %v",
				m.initialPolicyKnown, m.geometryKnown)
		}
		if m.kit.Theme().ReducedMotion {
			t.Fatal("real runtime startup ran under ReducedMotion")
		}
		if m.motion.startupPhase != startupRevealing || m.motion.startup.Float() != 0 {
			t.Fatalf("startup state: phase %v progress %v", m.motion.startupPhase, m.motion.startup.Float())
		}
		start = m.View().Content
		if got := strings.TrimSpace(xansi.Strip(start)); got != "" {
			t.Fatalf("startup's first eligible frame is not blank: %q", got)
		}
		return m
	})
	base := clock.Now()
	h.Mutate(func(m *Model) *Model {
		clock.Set(base.Add(startupRevealDuration / 2))
		return m
	}).SendProgram(frame)
	h.Mutate(func(m *Model) *Model {
		progress, attached := m.startupProgress()
		if !attached || progress <= 0 || progress >= 1 {
			t.Fatalf("real runtime midpoint progress = %v, attached %v", progress, attached)
		}
		if m.View().Content == start {
			t.Fatal("real runtime startup midpoint is identical to its blank start")
		}
		return m
	})
	h.Mutate(func(m *Model) *Model {
		clock.Set(base.Add(startupRevealDuration))
		return m
	}).SendProgram(frame)
	h.Mutate(func(m *Model) *Model {
		if _, attached := m.startupProgress(); attached || m.motion.startupPhase != startupConsumed {
			t.Fatal("real runtime startup did not remove its effect at the endpoint")
		}
		if _, cmd := m.Update(struct{}{}); cmd != nil {
			t.Fatalf("settled real runtime scheduled idle work: %v", messageTypes(commandMessages(t, cmd)))
		}
		return m
	})
	h.Wait("Services: 15")
	steepSnapshot(h, "startup-settled")
}

// TestSteepStartupReducedInitialConfig proves an initial ReducedMotion
// configuration starts settled: no transient effect and no animation frame.
func TestSteepStartupReducedInitialConfig(t *testing.T) {
	h, _ := newSteepStartupRuntime(t, true)
	h.WaitMessageWhere[configUpdateMsg](func(msg configUpdateMsg) bool { return msg.loaded })
	h.WaitMessage[tea.WindowSizeMsg]()
	h.Wait("Services: 15")
	h.Mutate(func(m *Model) *Model {
		if !m.initialPolicyKnown || !m.geometryKnown {
			t.Fatalf("reduced startup check ran before policy/geometry were known: policy %v geometry %v",
				m.initialPolicyKnown, m.geometryKnown)
		}
		if !m.kit.Theme().ReducedMotion {
			t.Fatal("initial runtime ReducedMotion configuration was not effective")
		}
		if m.motion.startupPhase != startupConsumed {
			t.Fatalf("reduced startup phase = %v, want consumed", m.motion.startupPhase)
		}
		if _, attached := m.startupProgress(); attached {
			t.Fatal("initial ReducedMotion attached a startup reveal")
		}
		return m
	})
	for msg := range h.MessageHistory() {
		if _, ok := msg.(anim.FrameMsg); ok {
			t.Fatal("initial ReducedMotion real runtime emitted an animation frame")
		}
	}
	steepSnapshot(h, "startup-reduced")
}
