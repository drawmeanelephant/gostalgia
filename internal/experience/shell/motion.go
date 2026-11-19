package shell

import (
	"strings"
	"time"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/anim"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
)

// launchShakeDuration is the entire failed-launch effect. It is deliberately
// below the experience contract's 400 ms ceiling and has no idle loop.
const launchShakeDuration = 160 * time.Millisecond

// motionState belongs to one shell model. In particular, it is not package
// state: SSH-attached shells in one process can choose opposite policies.
type motionState struct {
	now         func() time.Time
	launchShake anim.Value
	ready       bool
}

func (m *Model) motionNow() time.Time {
	if m.motion.now == nil {
		m.motion.now = time.Now
	}
	return m.motion.now()
}

// syncMotion creates the one-shot value only for a motion-enabled model.
// ReducedMotion leaves a zero Value, which neither attaches an effect nor
// responds to frame messages left over from before a policy change.
func (m *Model) syncMotion() {
	if m.kit.Theme().ReducedMotion {
		m.motion.launchShake = anim.Value{}
		m.motion.ready = false
		return
	}
	if !m.motion.ready {
		m.motion.launchShake = anim.NewValue(0,
			anim.Duration(launchShakeDuration),
			anim.Ease(anim.Linear),
			anim.Clock(m.motionNow),
		)
		m.motion.ready = true
	}
}

func (m *Model) updateMotion(msg tea.Msg) tea.Cmd {
	if m.kit.Theme().ReducedMotion || !m.motion.ready {
		return nil
	}
	return anim.Update(msg, &m.motion.launchShake)
}

// startFailedLaunch plays the only enabled transition from its beginning.
// Calling Play for each failure restarts the event's feedback, never an idle
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
