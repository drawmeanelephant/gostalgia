package shell

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/on"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/ui"
)

// Mouse input is additive: every click, wheel step and copy below has a
// keyboard equivalent, and with the mouse off (mouse off, or
// accessibility.mouse false) the shell asks for no mouse reporting at all,
// so the terminal's own selection works. A click acts through the same
// function as the key it stands for, which checks the state the key checks
// when it runs, not when the frame was drawn.

// mouseConfigKey is the configuration key that turns mouse input on and off
// for new shells, under accessibility. It is on when the key is missing.
const mouseConfigKey = "mouse"

// setMouse turns this shell's mouse input on or off. Either way the next
// frame gets a new bloom root, which drops any press, double click, hover,
// tooltip or selection the old one was following.
func (m *Model) setMouse(on bool) { m.mouseOff = !on }

// applyMouseConfig takes accessibility.mouse from a config refresh, nil
// when it is unset. Only a change of the setting applies, so a refresh that
// repeats it leaves this shell's own mouse command in force.
func (m *Model) applyMouseConfig(v *bool) {
	was, now := m.mouseConfig == nil || *m.mouseConfig, v == nil || *v
	if v != nil {
		val := *v
		m.mouseConfig = &val
	} else {
		m.mouseConfig = nil
	}
	if was != now {
		m.setMouse(now)
	}
}

// mouseState describes this shell's mouse input for the mouse command,
// with the copy policy a blocked copy's toast sends the reader here for: a
// toast in a small terminal has no room for it.
func (m *Model) mouseState() string {
	state := "Mouse is on: click, scroll, hover, and select text to copy it. Type mouse off to let the terminal select text."
	if m.mouseOff {
		state = "Mouse is off: the terminal selects text itself. Type mouse on for clicks, the wheel and copy."
	}
	return state + "\n" + copyPolicy
}

// copyPolicy is what decides whether a copy reaches the host clipboard.
const copyPolicy = "Copies (a mouse selection, or Ctrl-Y) go to the clipboard of the computer the runtime runs on, " +
	"only while its operator policy allows it: an operator sets clipboard.enabled and clipboard.allow_host_write " +
	"with sys/policy/update. The shell never writes the terminal's clipboard."

// baseMouseAllowed reports whether the screen beneath the modal layers may
// take mouse input now. A stale frame may outlive the state it was drawn in.
func (m *Model) baseMouseAllowed() bool { return !m.mouseOff && !m.modalOpen() }

// baseAct runs act for a click or wheel step on the screen beneath the
// modal layers. The mouse may have been turned off, or a palette or
// confirmation opened, since the frame the event landed on was drawn; bloom
// keeps a modal layer's events from what it covers only once it is drawn.
func (m *Model) baseAct(act func() tea.Cmd) tea.Cmd {
	if !m.baseMouseAllowed() {
		return nil
	}
	return act()
}

// screenAct runs act for a click or wheel step on screen s, unless it is
// no longer the screen shown or a command is running, which holds the keys
// the event stands for too.
func (m *Model) screenAct(s bodyScreen, act func() tea.Cmd) tea.Cmd {
	return m.baseAct(func() tea.Cmd {
		if m.busy || m.bodyScreen() != s {
			return nil
		}
		return act()
	})
}

// paletteAct runs act for a click or wheel step on the open palette.
func (m *Model) paletteAct(act func() tea.Cmd) tea.Cmd {
	if m.mouseOff || !m.paletteOpen || m.confirm != nil {
		return nil
	}
	return act()
}

// hit is a click target in a node: its rows and cells relative to the
// node's top-left, and what clicking it does.
type hit struct {
	x, y, w, h int
	act        func() tea.Cmd
}

func (h hit) contains(x, y int) bool {
	return x >= h.x && x < h.x+h.w && y >= h.y && y < h.y+h.h
}

// mouseNode draws child and takes clicks on the hits made for the size it
// is laid out at, and the wheel. Its hits are found from the state the
// frame is drawn from, so a click acts on what was under the pointer, not
// on whatever is at that row by the time it arrives.
type mouseNode struct {
	child  bloom.Child
	hits   func(b ui.Bounds) []hit
	scroll func(dy int) tea.Cmd
}

func (n mouseNode) View() string { return "" }

// Measure implements bloom.Measurer: the node asks for its child's size.
func (n mouseNode) Measure(avail bloom.Size) bloom.Size {
	if c, ok := n.child.(bloom.Measurer); ok {
		return c.Measure(avail)
	}
	return bloom.Size{}
}

// Layout implements bloom.Layouter.
func (n mouseNode) Layout(ctx bloom.Context) bloom.Child {
	var hs []on.Handler
	if n.hits != nil {
		if hits := n.hits(ui.Bounds{Width: ctx.Width, Height: ctx.Height}); len(hits) > 0 {
			hs = append(hs, on.ClickAt(func(e on.Event) tea.Cmd {
				for _, h := range hits {
					if h.contains(e.X, e.Y) {
						return h.act()
					}
				}
				return nil
			}))
		}
	}
	if n.scroll != nil {
		hs = append(hs, on.Scroll(func(_, dy int) tea.Cmd {
			if dy == 0 {
				return nil
			}
			return n.scroll(dy)
		}))
	}
	if len(hs) == 0 {
		return n.child
	}
	return bloom.Column(bloom.Fill(n.child).On(hs...))
}

// withMouse is child taking clicks on hits and the wheel through scroll,
// or child alone while the mouse is off.
func (m *Model) withMouse(child bloom.Child, hits func(ui.Bounds) []hit, scroll func(dy int) tea.Cmd) bloom.Child {
	if m.mouseOff {
		return child
	}
	return mouseNode{child: child, hits: hits, scroll: scroll}
}

// clampStep is i moved by d and kept within [0, n).
func clampStep(i, d, n int) int { return min(max(0, n-1), max(0, i+d)) }

// bodyScreen is which screen the frame's body shows, as body lays it out.
type bodyScreen int

const (
	bodyTranscript bodyScreen = iota
	bodyHome
	bodyLauncher
	bodyTasks
	bodyNotifications
	bodyApp
)

func (m *Model) bodyScreen() bodyScreen {
	switch {
	case m.taskView && m.tasks != nil:
		return bodyTasks
	case m.notifView && m.notifs != nil:
		return bodyNotifications
	case m.presentation != nil:
		return bodyApp
	}
	switch m.currentMode() {
	case modeLauncher:
		return bodyLauncher
	case modeHome:
		return bodyHome
	}
	return bodyTranscript
}

// screen is a screen the tabs and the F-keys switch to.
type screen int

const (
	screenHome screen = iota
	screenApps
	screenPrompt
	screenTasks
	screenNotifications
)

// tabScreens is the screen of each tab, by its index in tabs. The app
// view's tab, last while one is open, is always the active one.
var tabScreens = []screen{screenHome, screenApps, screenPrompt, screenTasks, screenNotifications}

// switchScreen shows screen to, closing an open app view, as its F-key
// does. With toggle, as for the key pressed again on its own screen, it
// goes back to the prompt instead; the tab of the screen shown is never
// clicked, as it is the active one.
func (m *Model) switchScreen(to screen, toggle bool) tea.Cmd {
	cancel := m.dismissView()
	switch to {
	case screenHome, screenApps:
		mode := modeHome
		if to == screenApps {
			mode = modeLauncher
		}
		m.taskView, m.notifView = false, false
		if toggle && m.currentMode() == mode {
			m.setMode(modePrompt)
		} else {
			m.setMode(mode)
		}
	case screenPrompt:
		m.taskView, m.notifView = false, false
		m.setMode(modePrompt)
	case screenTasks:
		m.taskView = !(toggle && m.taskView)
		if m.taskView {
			m.shelf, m.notifView = false, false
			m.setMode(modePrompt)
		}
	case screenNotifications:
		m.notifView = !(toggle && m.notifView)
		if m.notifView {
			m.shelf, m.taskView = false, false
			m.setMode(modePrompt)
		}
	}
	m.resetScroll()
	return cancel
}

// tabsNode is the tab row. Clicking a tab other than the active one shows
// its screen, as its F-key does.
func (m *Model) tabsNode() bloom.Child {
	tabs, active := m.tabs()
	return m.withMouse(m.kit.TabsLeaf(tabs, active), func(b ui.Bounds) []hit {
		var hits []hit
		for _, s := range m.kit.TabSpans(tabs, active, b.Width) {
			if s.Index == active || s.Index >= len(tabScreens) {
				continue
			}
			to := tabScreens[s.Index]
			hits = append(hits, hit{x: s.X, w: s.Width, h: 1, act: func() tea.Cmd {
				return m.baseAct(func() tea.Cmd {
					// The log tail's search takes every key until it is
					// done, the F-keys included.
					if m.taskView && m.tasks.Searching() {
						return nil
					}
					return m.switchScreen(to, false)
				})
			}})
		}
		return hits
	}, nil)
}

// launcherCard is the ID of an app's card in the launcher.
type launcherCard string

// launcherLine names the launcher's lines about the selected app.
type launcherLine int

const (
	launcherDescription launcherLine = iota + 1
	launcherGrants
)

// launcherNode is the launcher: the search line, the cards of the apps
// around the selection, and the selected app's description and grants,
// which show their full text as a tooltip where they are cut. A click on a
// card selects its app, as the arrow keys do, a double click launches it,
// as Enter does, and the wheel moves the selection.
func (m *Model) launcherNode() bloom.Child {
	node := ui.SizedNode(func(ctx bloom.Context) bloom.Child {
		w, h := ctx.Width, ctx.Height
		filtered := m.filteredApps()
		if w <= 0 || h <= 0 {
			return nil
		}
		if len(filtered) == 0 {
			return m.kit.Leaf(func(b ui.Bounds) string { return strings.Join(m.renderLauncher(b.Width, b.Height), "\n") })
		}
		selected := min(m.selected, len(filtered)-1)
		rows := []bloom.Child{bloom.Fixed(1, m.kit.Leaf(func(b ui.Bounds) string { return ui.Truncate(m.launcherSearch(), b.Width) }))}
		used := 1
		start, end, cardHeight := launcherCards(selected, len(filtered), h, m.launcherTop)
		for i := start; i < end && used < h; i++ {
			a, focused := filtered[i], i == selected
			card := m.kit.Leaf(func(b ui.Bounds) string { return m.launcherCard(a, focused, b.Width, cardHeight) })
			n := min(cardHeight, h-used)
			slot := bloom.Fixed(n, card).ID(launcherCard(a.Manifest.ID))
			if !m.mouseOff {
				id := a.Manifest.ID
				slot = slot.On(
					on.Click(func() tea.Cmd {
						return m.screenAct(bodyLauncher, func() tea.Cmd {
							if m.selectApp(id) {
								m.launcherTop = start
							}
							return nil
						})
					}),
					on.DoubleClick(func() tea.Cmd {
						return m.screenAct(bodyLauncher, func() tea.Cmd {
							if !m.selectApp(id) {
								return nil
							}
							return m.launchSelected()
						})
					}),
				)
			}
			rows = append(rows, slot)
			used += n
		}
		a := filtered[selected]
		for _, line := range []struct {
			id   launcherLine
			text string
		}{
			{launcherDescription, safe(a.Manifest.Description)},
			{launcherGrants, "GRANT  " + safe(strings.Join(a.Manifest.Permissions, " · "))},
		} {
			if used >= h {
				break
			}
			rows = append(rows, bloom.Fixed(1, bloom.Text(line.text).Style(m.kit.MutedStyle()).Truncate().ID(line.id)))
			used++
		}
		return bloom.Column(rows...)
	})
	return m.withMouse(node, nil, func(dy int) tea.Cmd {
		return m.screenAct(bodyLauncher, func() tea.Cmd {
			if n := len(m.filteredApps()); n > 0 {
				m.selected = clampStep(min(m.selected, n-1), dy, n)
			}
			return nil
		})
	})
}

// homeScroll moves Home's selection a row a wheel step, as the arrow keys
// do.
func (m *Model) homeScroll(dy int) tea.Cmd {
	return m.screenAct(bodyHome, func() tea.Cmd {
		if hv := m.homeView(); hv.visible > 0 {
			m.homeSelected = clampStep(hv.selected, dy, hv.visible)
		}
		return nil
	})
}

// homeHit is a click on the row of the Home item with key: it opens the
// item, as Enter does on it.
func (m *Model) homeHit(y, w int, kind homeItemType, key string) hit {
	return hit{y: y, w: w, h: 1, act: func() tea.Cmd {
		return m.screenAct(bodyHome, func() tea.Cmd { return m.openHomeItem(kind, key) })
	}}
}

// homeAppHits are the rows of the running apps renderHomeApps shows in b.
func (m *Model) homeAppHits(b ui.Bounds, hv homeView) []hit {
	var ids []string
	for _, a := range m.apps {
		if a.Running {
			ids = append(ids, a.Manifest.ID)
		}
	}
	n, top := b.Height-1, 1
	if n < 1 {
		n, top = 1, 0
	}
	selectedRow := -1
	if hv.selected >= 0 && hv.selected < len(ids) {
		selectedRow = hv.selected
	}
	start := max(0, min(selectedRow-n+1, len(ids)-n))
	var hits []hit
	for i := start; i < min(len(ids), start+n); i++ {
		hits = append(hits, m.homeHit(top+i-start, b.Width, homeItemApp, ids[i]))
	}
	return hits
}

// homeDocHits are the rows of the documents renderHomeDocs shows in b.
func (m *Model) homeDocHits(b ui.Bounds, hv homeView, base int) []hit {
	docs := m.homeData.Documents
	n := max(1, b.Height-1)
	start := max(0, min(hv.selected-base-n+1, len(docs)-n))
	var hits []hit
	for i := start; i < min(len(docs), start+n) && 1+i-start < b.Height; i++ {
		hits = append(hits, m.homeHit(1+i-start, b.Width, homeItemDoc, docs[i].Path))
	}
	return hits
}

// paletteHits are the rows of the items renderPalette shows in the
// palette's box of b. A click on one runs it, as Enter does.
func (m *Model) paletteHits(b ui.Bounds) []hit {
	filtered := m.filteredPaletteItems()
	cx, cy, content := m.kit.PanelContentRect(b)
	if len(filtered) == 0 || content.Width <= 0 {
		return nil
	}
	// The query, then the spacer under it in a palette tall enough.
	top := 1
	if b.Height-3 >= 4 {
		top = 2
	}
	visible := max(1, b.Height-3-top)
	selected := min(m.paletteSelected, len(filtered)-1)
	start := max(0, selected-visible+1)
	var hits []hit
	for i := start; i < min(len(filtered), start+visible); i++ {
		y := cy + top + i - start
		if y >= cy+content.Height {
			break
		}
		key := filtered[i].key()
		hits = append(hits, hit{x: cx, y: y, w: content.Width, h: 1, act: func() tea.Cmd {
			return m.paletteAct(func() tea.Cmd { return m.runPaletteItem(key) })
		}})
	}
	return hits
}

// paletteScroll moves the palette's selection a row a wheel step.
func (m *Model) paletteScroll(dy int) tea.Cmd {
	return m.paletteAct(func() tea.Cmd {
		if n := len(m.filteredPaletteItems()); n > 0 {
			m.paletteSelected = clampStep(min(m.paletteSelected, n-1), dy, n)
		}
		return nil
	})
}

// taskHits are the process rows the Task Manager's table shows in b. A
// click on one opens it, as Enter does on it.
func (m *Model) taskHits(b ui.Bounds) []hit {
	var hits []hit
	for _, r := range m.tasks.TableRows(m.kit, b) {
		pid := r.PID
		hits = append(hits, hit{x: r.X, y: r.Y, w: r.Width, h: 1, act: func() tea.Cmd {
			return m.screenAct(bodyTasks, func() tea.Cmd {
				if !m.tasks.SelectPID(pid) {
					return nil
				}
				return m.openSelectedTask()
			})
		}})
	}
	return hits
}

// taskScroll moves the table's selection a row a wheel step.
func (m *Model) taskScroll(dy int) tea.Cmd {
	return m.screenAct(bodyTasks, func() tea.Cmd {
		if m.tasks.Mode() != taskmanager.ModeTable {
			return nil
		}
		for range abs(dy) {
			if dy < 0 {
				m.tasks.SelectPrev()
			} else {
				m.tasks.SelectNext()
			}
		}
		return nil
	})
}

// notificationHits are the entries the Notification Center shows in b. A
// click on one opens it, as Enter does on it.
func (m *Model) notificationHits(b ui.Bounds) []hit {
	var hits []hit
	for _, s := range m.notifs.EntrySpans(m.kit, b) {
		id := s.ID
		hits = append(hits, hit{x: s.X, y: s.Y, w: s.Width, h: s.Height, act: func() tea.Cmd {
			return m.screenAct(bodyNotifications, func() tea.Cmd {
				if !m.notifs.Select(id) {
					return nil
				}
				return m.openSelectedNotification()
			})
		}})
	}
	return hits
}

// notificationScroll moves the Notification Center's selection an entry a
// wheel step.
func (m *Model) notificationScroll(dy int) tea.Cmd {
	return m.screenAct(bodyNotifications, func() tea.Cmd {
		for range abs(dy) {
			if dy < 0 {
				m.notifs.SelectPrev()
			} else {
				m.notifs.SelectNext()
			}
		}
		return nil
	})
}

// viewHits are the actions and grid cells the app view shows in b. A click
// on one focuses it and runs it, as Tab and Enter do.
func (m *Model) viewHits(b ui.Bounds) []hit {
	v := m.presentation
	if v == nil {
		return nil
	}
	_, targets := m.viewLayout(b.Height)
	var hits []hit
	for _, t := range targets {
		if t.line >= b.Height || t.x >= b.Width {
			continue
		}
		hits = append(hits, hit{x: t.x, y: t.line, w: min(t.w, b.Width-t.x), h: 1, act: func() tea.Cmd {
			return m.screenAct(bodyApp, func() tea.Cmd { return m.activateViewTarget(v, t) })
		}})
	}
	return hits
}

func abs(n int) int { return max(n, -n) }

// copyMsg is the answer to a copy: how much went, or why nothing did.
type copyMsg struct {
	chars int
	err   error
}

// copyCmd copies text to the host clipboard through the environment's
// clipboard/write route, so the operator's clipboard policy decides. It
// asks for the host clipboard explicitly: auto would quietly keep a copy
// the policy denies in the environment's internal clipboard and report
// success. The shell never writes the terminal's clipboard itself (OSC 52):
// over SSH that would be the client's clipboard, outside the policy.
func (m *Model) copyCmd(text string) tea.Cmd {
	if text == "" || m.client == nil {
		return nil
	}
	client, ctx := m.client, m.ctx
	return func() tea.Msg {
		c, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err := client.Call(c, "clipboard/write", map[string]any{"text": text, "target": "host"}, nil)
		return copyMsg{chars: utf8.RuneCountInString(text), err: err}
	}
}

// copyNotice is the notification for the answer to a copy. A denial says
// which operator policy settings would allow it: the shell cannot grant
// them. IPC errors reach the shell as text only, so the kinds of denial are
// told apart by the runtime's wording, which
// TestMouseSelectionCopiesThroughTheRuntimesPolicy pins against the real
// clipboard service.
func copyNotice(msg copyMsg) notifications.Notification {
	n := notifications.Notification{Level: notifications.LevelInfo, Source: "clipboard", Badge: "COPY", Wrap: true}
	switch err := msg.err; {
	case err == nil:
		n.Title = "Copied"
		n.Message = fmt.Sprintf("%d characters to the host clipboard.", msg.chars)
	case strings.Contains(err.Error(), "policy_denied") && strings.Contains(err.Error(), "writing is disabled"):
		n.Level = notifications.LevelWarning
		n.Title = "Copy blocked"
		n.Message = "Type mouse for how to allow it. Policy turns host clipboard writes off. An operator can set clipboard.allow_host_write with sys/policy/update."
	case strings.Contains(err.Error(), "policy_denied"):
		n.Level = notifications.LevelWarning
		n.Title = "Copy blocked"
		n.Message = "Type mouse for how to allow it. Policy keeps the host clipboard off. An operator can set clipboard.enabled and clipboard.allow_host_write with sys/policy/update."
	case strings.Contains(err.Error(), "clipboard integration is unsupported"):
		n.Level = notifications.LevelWarning
		n.Title = "Copy failed"
		n.Message = "The runtime's host has no clipboard to copy to. Nothing was copied."
	default:
		n.Level = notifications.LevelWarning
		n.Title = "Copy failed"
		n.Message = safe(err.Error())
	}
	return n
}

// flash shows n as a toast only, except on the Notification Center, which
// draws no toasts: there n goes to its history, where it shows at once.
func (m *Model) flash(n notifications.Notification) {
	if m.notifView {
		m.notifs.Record(n)
		return
	}
	m.notifs.Flash(n)
}

// nothingToCopy is the notification for Ctrl-Y where there is nothing to
// copy.
var nothingToCopy = notifications.Notification{
	Level: notifications.LevelInfo, Source: "clipboard", Badge: "COPY", Wrap: true, Title: "Nothing to copy",
	Message: "Ctrl-Y copies the text selected with the mouse, or the transcript or log lines in view.",
}

// copyInView is what Ctrl-Y copies: the text selected with the mouse, or
// with none, the lines of the open log tail or transcript in view. ok is
// false when there is nothing to copy here.
func (m *Model) copyInView() (cmd tea.Cmd, ok bool) {
	if !m.mouseOff {
		if text, ok := m.root().Selection(); ok && text != "" {
			return m.copyCmd(text), true
		}
	}
	var text string
	switch f := m.solve(); m.bodyScreen() {
	case bodyTasks:
		if m.tasks.Mode() == taskmanager.ModeLogs {
			text = m.tasks.LogsInView(f)
		}
	case bodyTranscript:
		if v, ok := f.Viewport(viewTranscript); ok {
			text = ui.ViewportText(v, m.transcriptBuffer())
		}
	}
	return m.copyCmd(text), text != ""
}
