package shell

import (
	"strconv"

	"bloomfx.dev/bloom/on"
	tea "charm.land/bubbletea/v2"
	"gostalgia/sdk"
)

// Hover is the pointer's target, never the keyboard's. It carries a stable
// semantic identity, so a cue follows the item it was joined to rather than a
// row index: app IDs, screen names, palette item keys, document paths, PIDs
// and grid cell IDs. Selection and focus stay in their own fields and are
// never moved by a hover.
type hoverKind uint8

const (
	hoverNone hoverKind = iota
	hoverLauncherCard
	hoverTab
	hoverPaletteItem
	hoverHomeRow
	hoverTaskRow
	hoverGridCell
)

type hoverTarget struct {
	kind hoverKind
	id   string
}

func (t hoverTarget) valid() bool { return t.kind != hoverNone && t.id != "" }

// hoverAt reports whether kind/id is the target under the pointer. It is
// false while the mouse is off, so a cue can never outlive the input mode.
func (m *Model) hoverAt(kind hoverKind, id string) bool {
	return !m.mouseOff && m.hover.kind == kind && m.hover.id == id
}

// setHover records kind/id as the pointer's target. A stale frame's callback
// can only replace the identity it names; it cannot resurrect a cue for an
// item the current screen no longer shows.
func (m *Model) setHover(kind hoverKind, id string) {
	t := hoverTarget{kind: kind, id: id}
	if !t.valid() || m.mouseOff {
		return
	}
	m.hover = t
}

// clearHover drops the cue only when it still belongs to kind/id, so an old
// leave callback cannot clear a newer target.
func (m *Model) clearHover(kind hoverKind, id string) {
	if m.hover.kind == kind && m.hover.id == id {
		m.hover = hoverTarget{}
	}
}

// clearHoverKind drops any cue of kind. It retires the current frame
// generation because a list or screen it described has changed.
func (m *Model) clearHoverKind(kind hoverKind) {
	if m.hover.kind == kind {
		m.hover = hoverTarget{}
	}
	m.hoverEpoch++
}

// clearHoverAll drops every cue. Navigation, replacement, resize and mouse
// mode changes take this route because no old identity can be trusted then.
// It also retires the frame generation, so a late callback from a frame drawn
// before the change cannot set a cue for a screen that is gone.
func (m *Model) clearHoverAll() {
	m.hover = hoverTarget{}
	m.hoverEpoch++
}

// clearHoverIn drops the cue when it names one of hs. A pointer leaving a
// node must not clear a cue that belongs to another region.
func (m *Model) clearHoverIn(hs []hit) {
	for _, h := range hs {
		if h.hover.kind != hoverNone {
			m.clearHover(h.hover.kind, h.hover.id)
		}
	}
}

// hoverHits applies a pointer event over hs. over is the bloom hover state
// for the node that owns the hits; e carries the position for motion within
// it. It neither changes keyboard state nor reports an error.
func (m *Model) hoverHits(hs []hit, allow func() bool, e on.Event, over bool) tea.Cmd {
	if m.mouseOff || (allow != nil && !allow()) {
		m.clearHoverIn(hs)
		return nil
	}
	if !over || !e.Inside {
		m.clearHoverIn(hs)
		return nil
	}
	for _, h := range hs {
		if h.hover.kind != hoverNone && h.contains(e.X, e.Y) {
			m.setHover(h.hover.kind, h.hover.id)
			return nil
		}
	}
	m.clearHoverIn(hs)
	return nil
}

// hoverSlotHits is the hover callback of a mouse node: bloom reports enter
// and leave with coordinates, and motion with its position.
func (m *Model) hoverSlotHits(hs []hit, allow func() bool) func(on.Event, bool) tea.Cmd {
	return func(e on.Event, over bool) tea.Cmd { return m.hoverHits(hs, allow, e, over) }
}

// hoverScreenKey is the stable identity of a tab's screen.
func hoverScreenKey(s screen) string {
	switch s {
	case screenHome:
		return "home"
	case screenApps:
		return "apps"
	case screenPrompt:
		return "prompt"
	case screenTasks:
		return "tasks"
	case screenNotifications:
		return "notifications"
	case screenWindows:
		return "windows"
	}
	return ""
}

// hoverHomeKey is the stable identity of a Home row.
func hoverHomeKey(kind homeItemType, key string) string {
	switch kind {
	case homeItemApp:
		return "app:" + key
	case homeItemDoc:
		return "doc:" + key
	}
	return ""
}

// pruneGridHover retires a grid cue whose cell the focused window no longer
// offers. A cell that remains keeps the pointer's cue across refreshes.
func (m *Model) pruneGridHover() {
	if m.hover.kind != hoverGridCell {
		return
	}
	v := m.focusedView()
	g := (*sdk.Grid)(nil)
	if v != nil {
		g = v.grid()
	}
	if g != nil {
		for _, c := range g.Cells {
			if c.ID == m.hover.id {
				return
			}
		}
	}
	m.clearHoverKind(hoverGridCell)
}

// hoverTaskKey is the stable identity of a Task Manager row.
func hoverTaskKey(pid int32) string { return strconv.FormatInt(int64(pid), 10) }

// hoverTaskPID is the row the pointer is on, or 0 for none.
func (m *Model) hoverTaskPID() int32 {
	if m.mouseOff || m.hover.kind != hoverTaskRow {
		return 0
	}
	pid, err := strconv.ParseInt(m.hover.id, 10, 32)
	if err != nil {
		return 0
	}
	return int32(pid)
}
