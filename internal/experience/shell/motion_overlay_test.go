package shell

import (
	"strings"
	"testing"
	"time"

	"bloomfx.dev/bloom/anim"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// controllableFixture is the populated recording shell with this test's
// clock injected, so a finite entrance can be stepped without real timers.
func controllableFixture(t *testing.T) (*Model, *recordingCaller, *fakeMotionClock) {
	t.Helper()
	m, rec := recordingFixture(t)
	m.setMode(modePrompt)
	clock := newFakeMotionClock()
	m.motion.now = clock.Now
	m.motion.ready = false
	m.syncMotion()
	return m, rec, clock
}

// entranceFrame returns the event's one animation frame from a command that
// may also carry ordinary refresh work.
func entranceFrame(t *testing.T, cmd tea.Cmd) anim.FrameMsg {
	t.Helper()
	messages := commandMessages(t, cmd)
	var frames []anim.FrameMsg
	for _, msg := range messages {
		if frame, ok := msg.(anim.FrameMsg); ok {
			frames = append(frames, frame)
		}
	}
	if len(frames) != 1 {
		t.Fatalf("command messages = %v, want one animation frame", messageTypes(messages))
	}
	return frames[0]
}

// overlayEntry is one actual opening route: its name, the message that opens
// it, and any state the shell needs first.
type overlayEntry struct {
	name  string
	setup func(*testing.T, *Model)
	msg   tea.Msg
	// direct is used by routes whose action runs from a palette item rather
	// than a keystroke.
	direct func(*Model) tea.Cmd
}

func overlayEntries() []overlayEntry {
	return []overlayEntry{
		{"palette ctrl+p", nil, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}, nil},
		{"palette super+space", nil, tea.KeyPressMsg{Code: ' ', Mod: tea.ModSuper}, nil},
		{"palette slash prompt", nil, tea.KeyPressMsg{Code: '/', Text: "/"}, nil},
		{"palette slash home", func(_ *testing.T, m *Model) { m.setMode(modeHome) }, tea.KeyPressMsg{Code: '/', Text: "/"}, nil},
		{"palette command", func(_ *testing.T, m *Model) { m.input, m.cursor = []rune("palette"), len("palette") }, tea.KeyPressMsg{Code: tea.KeyEnter}, nil},
		{"dialog typed shutdown", func(_ *testing.T, m *Model) { m.input, m.cursor = []rune("shutdown"), len("shutdown") }, tea.KeyPressMsg{Code: tea.KeyEnter}, nil},
		{"dialog typed reap", func(_ *testing.T, m *Model) { m.input, m.cursor = []rune("reap"), len("reap") }, tea.KeyPressMsg{Code: tea.KeyEnter}, nil},
		{"dialog typed stop", func(_ *testing.T, m *Model) {
			m.input, m.cursor = []rune("stop com.gostalgia.echo"), len("stop com.gostalgia.echo")
		}, tea.KeyPressMsg{Code: tea.KeyEnter}, nil},
		{"dialog launcher F3", func(_ *testing.T, m *Model) { m.setMode(modeLauncher); m.selected = 0 }, tea.KeyPressMsg{Code: tea.KeyF3}, nil},
		{"dialog tasks x", func(t *testing.T, m *Model) {
			m.taskView = true
			selectPID(t, m, 7)
		}, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "x"}, nil},
		{"dialog tasks r", func(_ *testing.T, m *Model) { m.taskView = true }, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "r"}, nil},
		{"dialog palette shutdown", nil, nil, func(m *Model) tea.Cmd {
			m.openPalette()
			m.motion.overlay.Set(1)
			for _, it := range m.filteredPaletteItems() {
				if it.Title == "shutdown" {
					return m.runPaletteItem(it.key())
				}
			}
			return nil
		}},
		{"dialog palette stop", nil, nil, func(m *Model) tea.Cmd {
			m.openPalette()
			m.motion.overlay.Set(1)
			for _, it := range m.filteredPaletteItems() {
				if strings.HasPrefix(it.Title, "Stop Echo") {
					return m.runPaletteItem(it.key())
				}
			}
			return nil
		}},
	}
}

// TestEveryOverlayEntrySchedulesOneEntrance verifies every palette and dialog
// opening route the shell exposes returns the event's scheduling command.
func TestEveryOverlayEntrySchedulesOneEntrance(t *testing.T) {
	for _, tc := range overlayEntries() {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := controllableFixture(t)
			if tc.setup != nil {
				tc.setup(t, m)
			}
			var cmd tea.Cmd
			if tc.direct != nil {
				cmd = tc.direct(m)
			} else {
				_, cmd = m.Update(tc.msg)
			}
			if m.confirm == nil && !m.paletteOpen {
				t.Fatal("route did not open its overlay")
			}
			if frames := animationFrames(commandMessages(t, cmd)); frames != 1 {
				t.Fatalf("opening scheduled %d animation frames, want 1", frames)
			}
			if !m.motion.overlay.Animating() || m.motion.overlay.Float() != 0 {
				t.Fatalf("opening did not start at progress zero: %v (animating %v)",
					m.motion.overlay.Float(), m.motion.overlay.Animating())
			}
		})
	}
}

func overlayReducedReference(t *testing.T, th theme.Theme, dialog bool) *Model {
	t.Helper()
	m := motionModel(th.WithReducedMotion(true), ui.ANSI256, newFakeMotionClock())
	if dialog {
		m.input, m.cursor = []rune("shutdown"), len("shutdown")
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if animationFrames(commandMessages(t, cmd)) != 0 {
			t.Fatal("ReducedMotion reference scheduled a dialog entrance")
		}
		return m
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if animationFrames(commandMessages(t, cmd)) != 0 {
		t.Fatal("ReducedMotion reference scheduled a palette entrance")
	}
	return m
}

// TestOverlayEntranceLifecycle steps the palette and dialog reveals with the
// controlled clock: a visible midpoint, an endpoint that removes the effect,
// no follow-up command, and stale frames that cannot revive it.
func TestOverlayEntranceLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"palette", tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}},
		{"dialog", tea.KeyPressMsg{Code: tea.KeyEnter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeMotionClock()
			m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
			if tc.name == "dialog" {
				m.input, m.cursor = []rune("shutdown"), len("shutdown")
			}
			_, cmd := m.Update(tc.msg)
			frame := entranceFrame(t, cmd)
			start := m.View().Content
			if start == "" {
				t.Fatal("opening produced an empty frame")
			}

			clock.Advance(overlayRevealDuration / 4)
			_, cmd = m.Update(frame)
			entranceFrame(t, cmd)
			mid := m.View().Content
			if got := m.motion.overlay.Float(); got <= 0 || got >= 1 {
				t.Fatalf("midpoint progress = %v, want strictly between 0 and 1", got)
			}
			if mid == start {
				t.Fatal("the reveal did not change the styled frame")
			}

			clock.Advance(overlayRevealDuration)
			_, cmd = m.Update(frame)
			if cmd != nil {
				t.Fatalf("endpoint scheduled %v", messageTypes(commandMessages(t, cmd)))
			}
			if m.motion.overlay.Animating() || m.motion.overlay.Float() != 1 {
				t.Fatal("the reveal did not settle at its endpoint")
			}
			settled := m.View().Content

			reference := overlayReducedReference(t, theme.Nostalgia(), tc.name == "dialog")
			if settled != reference.View().Content {
				t.Fatal("the endpoint frame differs from the settled ReducedMotion frame")
			}
			// The effect node is gone: the value reports settled and no
			// later update or stale frame schedules another one.
			clock.Advance(time.Second)
			_, cmd = m.Update(frame)
			if cmd != nil {
				t.Fatalf("a stale frame scheduled %v", messageTypes(commandMessages(t, cmd)))
			}
			if m.View().Content != settled {
				t.Fatal("a stale frame changed the settled frame")
			}
			if _, cmd = m.Update(struct{}{}); cmd != nil {
				t.Fatalf("settled shell scheduled idle work: %v", messageTypes(commandMessages(t, cmd)))
			}
		})
	}
}

// TestOverlayReducedMotionSkipsAndCancels proves the initial-reduced and
// mid-effect cases for both overlay kinds, including policy toggles and stale
// frames that arrive after cancellation.
func TestOverlayReducedMotionSkipsAndCancels(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.Msg
	}{
		{"palette", tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}},
		{"dialog", tea.KeyPressMsg{Code: tea.KeyEnter}},
	} {
		t.Run(tc.name+" initial reduced", func(t *testing.T) {
			m := motionModel(theme.Nostalgia().WithReducedMotion(true), ui.ANSI256, newFakeMotionClock())
			if tc.name == "dialog" {
				m.input, m.cursor = []rune("shutdown"), len("shutdown")
			}
			_, cmd := m.Update(tc.msg)
			if animationFrames(commandMessages(t, cmd)) != 0 {
				t.Fatal("initial ReducedMotion attached or scheduled an entrance")
			}
			if !m.modalOpen() {
				t.Fatal("ReducedMotion did not open the overlay")
			}
		})

		t.Run(tc.name+" cancelled mid-effect", func(t *testing.T) {
			clock := newFakeMotionClock()
			m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
			if tc.name == "dialog" {
				m.input, m.cursor = []rune("shutdown"), len("shutdown")
			}
			_, cmd := m.Update(tc.msg)
			frame := entranceFrame(t, cmd)
			clock.Advance(overlayRevealDuration / 4)
			m.Update(frame)

			reduced := true
			_, cmd = m.Update(configUpdateMsg{reducedMotion: &reduced})
			if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
				t.Fatalf("switching to ReducedMotion scheduled %d frames", frames)
			}
			if m.motion.overlay.Animating() || m.motion.overlay.Float() != 1 {
				t.Fatal("ReducedMotion did not snap the active reveal to its endpoint")
			}
			if _, ok := m.overlayProgress(); ok {
				t.Fatal("ReducedMotion still attached a transition")
			}
			static := m.View().Content

			clock.Advance(time.Second)
			_, cmd = m.Update(frame)
			if cmd != nil {
				t.Fatalf("a stale frame scheduled %v", messageTypes(commandMessages(t, cmd)))
			}
			if m.View().Content != static {
				t.Fatal("a stale frame revived the canceled reveal")
			}

			enabled := false
			_, cmd = m.Update(configUpdateMsg{reducedMotion: &enabled})
			if animationFrames(commandMessages(t, cmd)) != 0 {
				t.Fatal("disabling ReducedMotion replayed the canceled reveal")
			}
			if _, ok := m.overlayProgress(); ok {
				t.Fatal("disabling ReducedMotion reattached the canceled reveal")
			}
		})
	}

	t.Run("motion on enables ReducedMotion", func(t *testing.T) {
		m := motionModel(theme.Nostalgia(), ui.ANSI256, newFakeMotionClock())
		reduced := true
		m.Update(resultMsg{setMotion: &reduced})
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		if animationFrames(commandMessages(t, cmd)) != 0 {
			t.Fatal("`motion on` left animation enabled")
		}
	})
}

// TestToastEntranceIsKeyedToNewestIdentity verifies only a newly arriving
// toast animates, older toasts never replay, and unchanged polls, renders,
// resizes and theme changes schedule nothing.
func TestToastEntranceIsKeyedToNewestIdentity(t *testing.T) {
	clock := newFakeMotionClock()
	m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
	m.setMode(modePrompt)

	m.notifs.Record(notifications.Notification{ID: "older", Timestamp: clock.Now(), Title: "OLDER",
		Message: "first arrival long enough to wrap over more than one toast row for the entrance", Wrap: true})
	_, cmd := m.Update(tickMsg(clock.Now()))
	frame := entranceFrame(t, cmd)
	if m.motion.toastID != "older" || !m.motion.toast.Animating() {
		t.Fatalf("older entrance state: id %q animating %v", m.motion.toastID, m.motion.toast.Animating())
	}
	if _, ok := m.toastProgress("older"); !ok {
		t.Fatal("older did not own the active entrance")
	}
	start := m.View().Content
	clock.Advance(toastEntranceDuration / 2)
	m.Update(frame)
	mid := m.View().Content
	if mid == start {
		t.Fatal("the toast entrance did not change the styled frame")
	}

	// Re-rendering, resizing, a theme change and an unchanged poll are not
	// arrivals for the identity already consumed.
	for _, msg := range []tea.Msg{
		struct{}{},
		tea.WindowSizeMsg{Width: 100, Height: 30},
		resultMsg{setTheme: "midnight"},
		tickMsg(clock.Now()),
	} {
		_, cmd = m.Update(msg)
		if frames := animationFrames(commandMessages(t, cmd)); frames != 0 {
			t.Fatalf("%T replayed the entrance with %d frames", msg, frames)
		}
	}
	_ = m.View()
	if m.motion.toastID != "older" {
		t.Fatalf("the consumed identity changed to %q", m.motion.toastID)
	}

	// A second arrival cancels the older entrance and animates only itself.
	m.notifs.Record(notifications.Notification{ID: "newer", Timestamp: clock.Now(), Title: "NEWER", Message: "second arrival"})
	_, cmd = m.Update(tickMsg(clock.Now()))
	frame = entranceFrame(t, cmd)
	if m.motion.toastID != "newer" || !m.motion.toast.Animating() || m.motion.toast.Float() != 0 {
		t.Fatalf("newest entrance state: id %q progress %v", m.motion.toastID, m.motion.toast.Float())
	}
	if _, ok := m.toastProgress("older"); ok {
		t.Fatal("the older toast kept its entrance")
	}
	if _, ok := m.toastProgress("newer"); !ok {
		t.Fatal("the newer toast did not own the entrance")
	}
	text := m.View().Content
	if !strings.Contains(text, "OLDER") {
		t.Fatal("the older toast was hidden by the newer entrance")
	}
	if strings.Contains(text, "NEWER") {
		t.Fatal("the newest toast was fully visible at progress zero")
	}

	clock.Advance(toastEntranceDuration)
	_, cmd = m.Update(frame)
	if cmd != nil {
		t.Fatalf("toast endpoint scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if m.motion.toast.Animating() || m.motion.toast.Float() != 1 {
		t.Fatal("the toast entrance did not settle")
	}
	if !strings.Contains(m.View().Content, "NEWER") {
		t.Fatal("the settled toast was not visible")
	}

	// Expiry changes the newest identity without scheduling decoration.
	m.notifs.Tick()
	for range 7 {
		m.notifs.Tick()
	}
	_, cmd = m.Update(struct{}{})
	if cmd != nil {
		t.Fatalf("expiry scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if m.motion.toastID != "" {
		t.Fatalf("expired toast identity = %q, want empty", m.motion.toastID)
	}
}

// TestToastReducedMotionSkipsCancelsAndNeverReplays covers the toast policy
// cases: initial reduced attaches nothing, a mid-effect switch snaps and
// invalidates the frames, and re-enabling does not replay the same identity.
func TestToastReducedMotionSkipsCancelsAndNeverReplays(t *testing.T) {
	clock := newFakeMotionClock()
	m := motionModel(theme.Nostalgia().WithReducedMotion(true), ui.ANSI256, clock)
	m.setMode(modePrompt)
	m.notifs.Record(notifications.Notification{ID: "reduced", Timestamp: clock.Now(), Title: "QUIET", Message: "no entrance"})
	_, cmd := m.Update(tickMsg(clock.Now()))
	if animationFrames(commandMessages(t, cmd)) != 0 {
		t.Fatal("initial ReducedMotion attached a toast entrance")
	}
	if m.motion.toastID != "reduced" {
		t.Fatal("ReducedMotion did not consume the toast identity")
	}

	enabled := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
	enabled.setMode(modePrompt)
	enabled.notifs.Record(notifications.Notification{ID: "active", Timestamp: clock.Now(), Title: "ACTIVE", Message: "entering"})
	_, cmd = enabled.Update(tickMsg(clock.Now()))
	frame := entranceFrame(t, cmd)
	clock.Advance(toastEntranceDuration / 4)
	enabled.Update(frame)

	reduced := true
	_, cmd = enabled.Update(configUpdateMsg{reducedMotion: &reduced})
	if animationFrames(commandMessages(t, cmd)) != 0 {
		t.Fatal("switching to ReducedMotion scheduled a toast frame")
	}
	if enabled.motion.toast.Animating() || enabled.motion.toast.Float() != 1 {
		t.Fatal("ReducedMotion did not snap the toast entrance")
	}
	static := enabled.View().Content
	clock.Advance(time.Second)
	_, cmd = enabled.Update(frame)
	if cmd != nil {
		t.Fatalf("stale toast frame scheduled %v", messageTypes(commandMessages(t, cmd)))
	}
	if enabled.View().Content != static {
		t.Fatal("a stale frame revived the canceled toast entrance")
	}

	off := false
	_, cmd = enabled.Update(configUpdateMsg{reducedMotion: &off})
	if animationFrames(commandMessages(t, cmd)) != 0 {
		t.Fatal("disabling ReducedMotion replayed the toast entrance")
	}
	_, cmd = enabled.Update(tickMsg(clock.Now()))
	if animationFrames(commandMessages(t, cmd)) != 0 {
		t.Fatal("an unchanged toast identity replayed after re-enabling motion")
	}
}

// TestOverlayEntrancesPreserveInputAndModalGuards runs real keyboard,
// command and pointer routes during an active reveal and checks the settled
// rules still decide navigation, cancellation and blocking.
func TestOverlayEntrancesPreserveInputAndModalGuards(t *testing.T) {
	t.Run("palette navigation and dismissal mid-reveal", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		m.input, m.cursor = []rune("draft"), len("draft")
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		frame := entranceFrame(t, cmd)
		clock.Advance(overlayRevealDuration / 4)
		m.Update(frame)

		before := m.paletteSelected
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		if m.paletteSelected != before+1 {
			t.Fatalf("Down moved palette selection %d -> %d", before, m.paletteSelected)
		}
		m.Update(typed("help"))
		if m.paletteQuery != "help" {
			t.Fatalf("typing mid-reveal produced query %q", m.paletteQuery)
		}
		if string(m.input) != "draft" {
			t.Fatalf("palette input reached the covered prompt: %q", string(m.input))
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyF1})
		if m.currentMode() != modePrompt {
			t.Fatal("a covered shortcut changed the screen under the palette")
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		if m.paletteOpen {
			t.Fatal("Escape did not dismiss the palette mid-reveal")
		}
		if string(m.input) != "draft" {
			t.Fatalf("dismissal changed the prompt draft: %q", string(m.input))
		}
	})

	t.Run("palette blocks pointer activation mid-reveal", func(t *testing.T) {
		m, rec := mouseModel(t, 80, 24)
		m.setMode(modeHome)
		m.motion.now = newFakeMotionClock().Now
		m.motion.ready = false
		m.syncMotion()
		p, ok := findText(m, rectOf(t, m, slotBody), "Echo")
		if !ok {
			t.Fatal("Echo row is not visible")
		}
		m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		clickAt(t, m, p)
		if m.presentation != nil || m.currentMode() != modeHome {
			t.Fatal("a click under the opening palette reached the Home row")
		}
		if len(rec.all()) != 0 {
			t.Fatalf("a blocked click made requests: %+v", rec.all())
		}
	})

	t.Run("dialog blocks pointer activation mid-reveal", func(t *testing.T) {
		m, rec := mouseModel(t, 80, 24)
		m.setMode(modeHome)
		m.motion.now = newFakeMotionClock().Now
		m.motion.ready = false
		m.syncMotion()
		p, ok := findText(m, rectOf(t, m, slotBody), "Echo")
		if !ok {
			t.Fatal("Echo row is not visible")
		}
		m.openConfirm(confirmation{kind: confirmShutdown})
		clickAt(t, m, p)
		if m.presentation != nil || m.currentMode() != modeHome {
			t.Fatal("a click under the opening dialog reached the Home row")
		}
		if len(rec.all()) != 0 {
			t.Fatalf("a blocked click made requests: %+v", rec.all())
		}
	})

	t.Run("dialog cancel is initial and Enter alone is safe mid-reveal", func(t *testing.T) {
		m, rec, clock := controllableFixture(t)
		m.input, m.cursor = []rune("shutdown"), len("shutdown")
		_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		frame := entranceFrame(t, cmd)
		if m.confirm == nil || m.confirm.active != confirmCancel {
			t.Fatal("the dialog did not open on Cancel")
		}
		clock.Advance(overlayRevealDuration / 4)
		m.Update(frame)

		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.confirm != nil {
			t.Fatal("Enter on the initial Cancel left the dialog open")
		}
		if got := rec.destructive(); len(got) != 0 {
			t.Fatalf("Enter alone performed %v", got)
		}

		// At the endpoint the same controls still work: reopen, Tab to the
		// action, and Enter sends exactly one request.
		m.input, m.cursor = []rune("stop com.gostalgia.echo"), len("stop com.gostalgia.echo")
		_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		frame = entranceFrame(t, cmd)
		clock.Advance(overlayRevealDuration)
		m.Update(frame)
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		_, stopCmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		commandMessages(t, stopCmd)
		if got := rec.destructive(); len(got) != 1 || got[0].method != "app/stop" {
			t.Fatalf("settled confirmation sent %+v", got)
		}
	})

	t.Run("toast entrance does not steal focus", func(t *testing.T) {
		clock := newFakeMotionClock()
		m := motionModel(theme.Nostalgia(), ui.ANSI256, clock)
		m.setMode(modePrompt)
		m.notifs.Record(notifications.Notification{ID: "focus", Timestamp: clock.Now(), Title: "FOCUS", Message: "toast"})
		_, cmd := m.Update(tickMsg(clock.Now()))
		entranceFrame(t, cmd)
		m.Update(typed("x"))
		if string(m.input) != "x" || m.Focus() != FocusPrompt {
			t.Fatalf("the toast entrance stole input: %q at %v", string(m.input), m.Focus())
		}
	})
}
