package shell

import (
	"strings"
	"time"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/anim"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
)

// The finite effects are event-owned and comfortably below the experience
// contract's 400 ms ceiling. They have no idle loop: a value schedules frames
// only while it is moving and its transition node disappears at progress one.
const (
	launchShakeDuration   = 160 * time.Millisecond
	overlayRevealDuration = 140 * time.Millisecond
	toastEntranceDuration = 160 * time.Millisecond
	// startupRevealDuration is the once-per-model CRT-style startup reveal.
	// It stays inside the same 400 ms finite-motion budget.
	startupRevealDuration = 180 * time.Millisecond
)

// startupPhase is the model-local once-only startup state. A model waits
// until usable geometry and the effective initial policy are both known, then
// either reveals once or consumes the event in its settled state. Consumed is
// final: no later poll, theme, resize, root rebuild or policy change replays.
type startupPhase uint8

const (
	startupWaiting startupPhase = iota
	startupRevealing
	startupConsumed
)

// motionState belongs to one shell model. In particular, it is not package
// state: SSH-attached shells in one process can choose opposite policies.
//
// overlay is the palette/confirmation opening reveal. toast is the entrance of
// the newest toast identity only; toastID remembers which identity was
// consumed so re-rendering, polling, resizing or a theme change never replays
// it. startup is the model's one startup reveal, keyed by startupPhase.
// Values start settled, and only an event Play moves them.
type motionState struct {
	now          func() time.Time
	launchShake  anim.Value
	overlay      anim.Value
	toast        anim.Value
	toastID      string
	startup      anim.Value
	startupPhase startupPhase
	ready        bool
}

func (m *Model) motionNow() time.Time {
	if m.motion.now == nil {
		m.motion.now = time.Now
	}
	return m.motion.now()
}

// syncMotion creates the one-shot values only for a motion-enabled model.
// ReducedMotion leaves them settled and snap-cancels active effects: Set
// invalidates the outstanding generation, so a frame scheduled before the
// policy change can neither revive the effect nor schedule another one.
func (m *Model) syncMotion() {
	if m.kit.Theme().ReducedMotion {
		m.motion.overlay.Set(1)
		m.motion.toast.Set(1)
		m.motion.launchShake = anim.Value{}
		if m.motion.startupPhase == startupRevealing {
			// Cancellation consumes the once-only event. Set invalidates the
			// outstanding generation, so a frame scheduled before the policy
			// change can neither revive the reveal nor schedule another one,
			// and re-enabling motion cannot replay it.
			m.motion.startup.Set(1)
			m.motion.startupPhase = startupConsumed
		}
		m.motion.ready = false
		return
	}
	if !m.motion.ready {
		m.motion.launchShake = anim.NewValue(0,
			anim.Duration(launchShakeDuration),
			anim.Ease(anim.Linear),
			anim.Clock(m.motionNow),
		)
		m.motion.overlay = anim.NewValue(1,
			anim.Duration(overlayRevealDuration),
			anim.Ease(anim.OutCubic),
			anim.Clock(m.motionNow),
		)
		m.motion.toast = anim.NewValue(1,
			anim.Duration(toastEntranceDuration),
			anim.Ease(anim.OutCubic),
			anim.Clock(m.motionNow),
		)
		m.motion.ready = true
	}
}

func (m *Model) updateMotion(msg tea.Msg) tea.Cmd {
	if m.kit.Theme().ReducedMotion || !m.motion.ready {
		return nil
	}
	cmd := anim.Update(msg, &m.motion.launchShake, &m.motion.overlay, &m.motion.toast, &m.motion.startup)
	if m.motion.startupPhase == startupRevealing && !m.motion.startup.Animating() {
		m.motion.startupPhase = startupConsumed
	}
	return cmd
}

// syncStartupMotion starts the model's one startup reveal once usable
// geometry and the effective initial policy are both known. The effective
// policy may arrive through the initial runtime configuration before or
// after the terminal delivers its size; either order starts the same single
// event. A ReducedMotion policy consumes the event without attaching or
// scheduling anything, and no trigger after consumption starts it again.
// A rebuilt bloom root is not an event: the transition is derived from this
// model value on each render.
func (m *Model) syncStartupMotion() tea.Cmd {
	if m.motion.startupPhase != startupWaiting {
		return nil
	}
	if !m.geometryKnown || m.width < minFrameWidth || m.height < minFrameHeight {
		return nil
	}
	if !m.initialPolicyKnown {
		return nil
	}
	m.motion.startupPhase = startupConsumed
	if m.kit.Theme().ReducedMotion {
		return nil
	}
	m.syncMotion()
	if !m.motion.ready {
		return nil
	}
	m.motion.startup = anim.NewValue(0,
		anim.Duration(startupRevealDuration),
		anim.Ease(anim.OutCubic),
		anim.Clock(m.motionNow),
	)
	cmd := m.motion.startup.Play(0, 1)
	if !m.motion.startup.Animating() {
		// With no global motion driver the value snapped to its endpoint;
		// the event is still consumed.
		return nil
	}
	m.motion.startupPhase = startupRevealing
	return cmd
}

// startupProgress reports the reveal's progress while it is attached. A
// settled or consumed startup attaches no transition at all, so the effect
// node disappears at progress one.
func (m *Model) startupProgress() (float64, bool) {
	if m.motion.startupPhase != startupRevealing || !m.motion.startup.Animating() {
		return 0, false
	}
	return m.motion.startup.Float(), true
}

// startOverlayEntrance starts the opening reveal for a palette or dialog.
// Each opening path, whether keyboard, command, or palette action, returns
// this command, so the event itself schedules the frames. ReducedMotion
// returns no command and renders the layer settled.
func (m *Model) startOverlayEntrance() tea.Cmd {
	if m.kit.Theme().ReducedMotion {
		return nil
	}
	m.syncMotion()
	if !m.motion.ready {
		return nil
	}
	return m.motion.overlay.Play(0, 1)
}

// overlayProgress reports the opening reveal's progress while it is active.
// A settled value attaches no transition at all.
func (m *Model) overlayProgress() (float64, bool) {
	if m.kit.Theme().ReducedMotion || !m.motion.ready || !m.motion.overlay.Animating() {
		return 0, false
	}
	return m.motion.overlay.Float(), true
}

// syncToastMotion consumes the newest toast identity exactly once. It runs on
// every shell update after the event handling, because services record into
// the shared manager from their own goroutines and the shell observes the
// result with its next message. A new identity cancels an older entrance
// before starting its own; an unchanged identity, a re-render, a poll, a
// resize, or a theme change schedules nothing.
func (m *Model) syncToastMotion() tea.Cmd {
	newest := ""
	if m.notifs != nil {
		if toasts := m.notifs.ActiveToasts(); len(toasts) > 0 {
			newest = toasts[len(toasts)-1].ID
		}
	}
	if newest == m.motion.toastID {
		return nil
	}
	m.motion.toastID = newest
	// The previous entrance, if any, is stale now even when motion is off.
	m.motion.toast.Set(1)
	if newest == "" || m.kit.Theme().ReducedMotion {
		return nil
	}
	m.syncMotion()
	if !m.motion.ready {
		return nil
	}
	return m.motion.toast.Play(0, 1)
}

// toastProgress reports the entrance progress for the toast with id. Only the
// newest incoming identity animates; older toasts in the stack render settled.
func (m *Model) toastProgress(id string) (float64, bool) {
	if id == "" || id != m.motion.toastID || m.kit.Theme().ReducedMotion ||
		!m.motion.ready || !m.motion.toast.Animating() {
		return 0, false
	}
	return m.motion.toast.Float(), true
}

// startFailedLaunch plays the failed-launch shake from its beginning. Calling
// Play for each failure restarts the event's feedback, never an idle
// animation. ReducedMotion returns no command and leaves the badge static.
func (m *Model) startFailedLaunch() tea.Cmd {
	if m.kit.Theme().ReducedMotion {
		return nil
	}
	m.syncMotion()
	return m.motion.launchShake.Play(0, 1)
}

// badgeNode applies fx.Shake only while the failed-launch event is active.
// At progress zero and one fx.Shake is also a no-op, and a settled value
// removes the Transition node entirely.
func (m *Model) badgeNode() bloom.Child {
	var node bloom.Child = m.kit.Leaf(m.badge)
	if !m.kit.Theme().ReducedMotion && m.motion.ready && m.motion.launchShake.Animating() {
		node = bloom.Transition(m.motion.launchShake.Float(), node, fx.Shake{Amount: 2, Times: 2})
	}
	return node
}

func failedLaunchResult(msg resultMsg) bool {
	if msg.failedLaunch {
		return true
	}
	args, err := words(msg.executedLine)
	return err == nil && len(args) > 0 && (strings.EqualFold(args[0], "launch") || strings.EqualFold(args[0], "run"))
}
