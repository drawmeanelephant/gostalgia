package shell

import (
	"fmt"
	"image/color"
	"strings"

	"bloomfx.dev/bloom"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

// frameSlot names the shell frame's slots for Frame.Rect.
type frameSlot int

const (
	slotNote frameSlot = iota + 1
	slotPanel
	slotTabs
	slotBadge
	slotBody
	slotToasts
	slotPrompt
	slotHelp
)

// Collapse priorities of the frame's optional rows. When the panel cannot
// fit every row, the blank spacers go first, then toasts, so tabs, badge,
// minBodyRows of body and the prompt always stay on screen.
const (
	prioFrameSpacer = 1
	prioFrameToasts = 2
)

// minBodyRows is the body height toasts give way to: enough for a screen's
// heading and the item its keys act on. The smallest frame, 30×10, leaves
// exactly this much.
const minBodyRows = 3

// The shell renders its frame only from 30×10; below that it shows sizeNote.
const (
	minFrameWidth  = 30
	minFrameHeight = 10
	sizeNote       = "GOSTALGIA\nResize to 30×10 or larger.\nCtrl-C exits."
	frameTitle     = "G O S T A L G I A  /  PERSONAL COMPUTING"
)

// rootKey is everything a bloom root is built from. A kit change that keeps
// it equal keeps the root.
type rootKey struct {
	theme   string
	mode    ui.ColorMode
	reduced bool
	fg, bg  [4]uint32
}

func colorKey(c color.Color) [4]uint32 {
	if c == nil {
		return [4]uint32{}
	}
	r, g, b, a := c.RGBA()
	return [4]uint32{r, g, b, a}
}

func rootKeyOf(k ui.Kit) rootKey {
	fg, bg := k.Colors()
	return rootKey{theme: k.Theme().Name, mode: k.Mode(), reduced: k.Theme().ReducedMotion,
		fg: colorKey(fg), bg: colorKey(bg)}
}

// root returns the model's own bloom root, built for its current kit and
// rebuilt only when the kit's effective theme, profile or motion changes,
// so a config poll that re-applies the same settings keeps it. Its colors
// come from the theme, never from the terminal: Root.Init, which would
// query the host's colors, is never called.
func (m *Model) root() *bloom.Root {
	if key := rootKeyOf(m.kit); m.ui == nil || m.uiKey != key {
		fg, bg := m.kit.Colors()
		m.ui = bloom.New(bloom.WithColors(fg, bg), bloom.WithCopyFlash(false))
		m.uiKey = key
	}
	return m.ui
}

// forwardToRoot reports whether msg may reach the bloom root. The terminal's
// own colors and profile would replace the explicit theme colors and
// color_mode, so they never do.
func forwardToRoot(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.ForegroundColorMsg, tea.BackgroundColorMsg, tea.ColorProfileMsg:
		return false
	}
	return true
}

// View declares the shell frame plus the terminal modes it needs. Alternate
// screen and other v1 program options are declarative View fields in v2, so
// the shell owns its screen mode here instead of at NewProgram time.
func (m *Model) View() tea.View {
	m.initExperience()
	var v tea.View
	if m.width <= 0 || m.height <= 0 {
		v = tea.NewView("")
	} else {
		m.syncPresentationWindow()
		v = m.root().View(m.width, m.height, m.layout())
	}
	v.AltScreen = true
	// Ask for kitty event types (key releases and repeat flags) on top of
	// the disambiguation v2 already requests. The request is progressive:
	// a terminal that doesn't speak the protocol ignores it, and the
	// negotiated answer arrives as KeyboardEnhancementsMsg.
	v.KeyboardEnhancements.ReportEventTypes = true
	return v
}

// solve lays the current frame out without drawing it.
func (m *Model) solve() *bloom.Frame {
	return bloom.Solve(m.width, m.height, m.layout())
}

// layout is the shell frame as a bloom tree: a kit Panel around tabs, badge,
// body and prompt, with the help bar below it. It reads model state and
// never changes it.
func (m *Model) layout() bloom.Child {
	if m.width < minFrameWidth || m.height < minFrameHeight {
		note := ui.Sized(func(b ui.Bounds) string { return m.kit.Text(sizeNote) })
		return bloom.Column(bloom.Fill(note).ID(slotNote))
	}
	return m.frame(m.homeView())
}

func (m *Model) frame(home homeView) bloom.Child {
	rows := []bloom.Child{
		bloom.Fixed(1, m.kit.TabsLeaf(m.tabs())).ID(slotTabs),
		bloom.Fixed(1, m.kit.Leaf(m.badge)).ID(slotBadge),
		bloom.Fixed(1, nil).Collapsible(prioFrameSpacer),
		bloom.Fill(m.body(home)).Min(minBodyRows).ID(slotBody),
	}
	if n := m.toastLines(); n > 0 {
		toasts := m.kit.Leaf(func(b ui.Bounds) string { return m.notifs.RenderToasts(m.kit, b.Width) })
		rows = append(rows, bloom.Fixed(n, toasts).Collapsible(prioFrameToasts).ID(slotToasts))
	}
	rows = append(rows,
		bloom.Fixed(1, nil).Collapsible(prioFrameSpacer),
		bloom.Fixed(1, m.promptNode()).ID(slotPrompt))
	panel := m.kit.PanelNode(ui.Panel{Title: frameTitle}, bloom.Column(rows...))
	return bloom.Column(
		bloom.Fill(panel).ID(slotPanel),
		bloom.Fixed(1, m.kit.HelpBarLeaf(m.bindings(home))).ID(slotHelp),
	)
}

func (m *Model) tabs() ([]ui.Tab, int) {
	active := 0
	switch m.currentMode() {
	case modeHome:
		active = 0
	case modeLauncher:
		active = 1
	case modePrompt:
		active = 2
	}
	if m.taskView {
		active = 3
	} else if m.notifView {
		active = 4
	}
	notifLabel := "Notifications"
	if m.notifs != nil && m.notifs.UnseenCount() > 0 {
		notifLabel = fmt.Sprintf("Notifications (%d)", m.notifs.UnseenCount())
	}
	tabItems := []ui.Tab{
		{Label: "Home"},
		{Label: "Apps"},
		{Label: "Prompt"},
		{Label: "Tasks"},
		{Label: notifLabel},
	}
	if m.presentation != nil {
		tabItems = append(tabItems, ui.Tab{Label: viewText(m.presentation.data.Title)})
		active = len(tabItems) - 1
	}
	return tabItems, active
}

func (m *Model) badge(b ui.Bounds) string {
	statusLabel := "ONLINE"
	if m.attached {
		statusLabel = "ATTACHED"
	}
	userName := viewText(m.user)
	if userName == "" {
		userName = "guest"
	}
	badge := m.kit.Badge(statusLabel, theme.Success, b.Width) + m.kit.Muted(fmt.Sprintf("   %s · C: environment drive", userName))
	return ui.Truncate(badge, b.Width)
}

func (m *Model) toastLines() int {
	if m.notifs == nil || m.notifView {
		return 0
	}
	toasts := m.notifs.RenderToasts(m.kit, max(1, m.width))
	if toasts == "" {
		return 0
	}
	return strings.Count(toasts, "\n") + 1
}

// body is the frame's main slot for the current screen.
func (m *Model) body(home homeView) bloom.Child {
	switch {
	case m.paletteOpen:
		return m.kit.Leaf(func(b ui.Bounds) string { return strings.Join(m.renderPalette(b.Width, b.Height), "\n") })
	case m.taskView && m.tasks != nil:
		return m.kit.Leaf(func(b ui.Bounds) string { return m.tasks.Render(m.kit, b) })
	case m.notifView && m.notifs != nil:
		return m.kit.Leaf(func(b ui.Bounds) string { return m.notifs.RenderPanel(m.kit, b) })
	case m.presentation != nil:
		return m.kit.Leaf(func(b ui.Bounds) string {
			lines := m.viewLines(b.Height)
			for i, line := range lines {
				lines[i] = ui.Truncate(line, b.Width)
			}
			return strings.Join(lines, "\n")
		})
	}
	switch m.currentMode() {
	case modeLauncher:
		return m.kit.Leaf(func(b ui.Bounds) string { return strings.Join(m.renderLauncher(b.Width, b.Height), "\n") })
	case modeHome:
		return m.homeNode(home)
	}
	return m.kit.Leaf(m.transcriptView)
}

func (m *Model) transcriptView(b ui.Bounds) string {
	var lines []string
	for _, e := range m.transcript {
		line := ui.Wrap(e.text, b.Width)
		switch e.kind {
		case "accent":
			line = m.kit.Heading(line)
		case "muted":
			line = m.kit.Muted(line)
		case "command":
			line = m.kit.Heading(line)
		case "error":
			line = m.kit.StatusText(line, theme.Error)
		default:
			line = m.kit.Text(line)
		}
		lines = append(lines, strings.Split(line, "\n")...)
	}
	// Transcript scrollback delegates its window to bubbles/viewport
	// (issue #185). m.scroll stays the canonical "lines back from the
	// tail" offset; the viewport offsets from the top, so callers keep
	// the same scroll contract.
	vp := viewport.New(viewport.WithWidth(b.Width), viewport.WithHeight(b.Height))
	vp.SetContentLines(lines)
	vp.SetYOffset(len(lines) - b.Height - m.scroll)
	return vp.View()
}

// sizedNode builds its subtree for the exact space its slot gives it. It
// asks for no space of its own.
type sizedNode func(ctx bloom.Context) bloom.Child

func (sizedNode) View() string                           { return "" }
func (sizedNode) Measure(bloom.Size) bloom.Size          { return bloom.Size{} }
func (n sizedNode) Layout(ctx bloom.Context) bloom.Child { return n(ctx) }

// promptNode is the prompt row. In editable prompt mode it places the real
// terminal cursor at the insertion point.
func (m *Model) promptNode() bloom.Child {
	return sizedNode(func(ctx bloom.Context) bloom.Child {
		if ctx.Width <= 0 || ctx.Height <= 0 {
			return nil
		}
		line, x, editable := m.promptLine(ctx.Width)
		text := bloom.Text(m.kit.Text(ui.Fit(line, ui.Bounds{Width: ctx.Width, Height: 1})))
		if editable {
			text = text.Cursor(tea.NewCursor(x, 0))
		}
		return text
	})
}

// promptLine renders the prompt row in w cells. editable reports whether the
// row takes typing, and x is then the insertion point's cell.
func (m *Model) promptLine(w int) (line string, x int, editable bool) {
	promptPath := safe(dosPath(m.cwd))
	if lipgloss.Width(promptPath) > w/2 {
		promptPath = "C:…" + ui.Tail(promptPath, max(1, w/2-3))
	}
	prefix := promptPath + "> "
	prompt := m.kit.Heading(prefix)
	switch {
	case m.paletteOpen:
		prompt = m.kit.Heading("PALETTE> ") + m.kit.Muted("Type to filter · ↑↓ navigate · Enter execute · Esc close")
	case m.presentation != nil:
		prompt += m.kit.Muted("App view owns focus. Esc returns to prompt.")
	case m.taskView:
		prompt += m.kit.Muted("Task Manager owns focus. Esc returns to prompt.")
	case m.notifView:
		prompt += m.kit.Muted("Notification Center owns focus. Esc returns to prompt.")
	case m.busy:
		// A static busy badge, not bubbles/spinner (issue #185): the kit is
		// deliberately stateless and never schedules a tick, so ReducedMotion
		// themes lose nothing. Install/update/rollback are one-shot IPC calls
		// with no progress stream to animate anyway.
		prompt += m.kit.Badge("", theme.Busy, max(0, w-lipgloss.Width(prompt)))
	case m.currentMode() == modeLauncher:
		filtered := m.filteredApps()
		sel := min(m.selected, len(filtered)-1)
		if sel >= 0 && filtered[sel].Running {
			prompt += m.kit.StatusText(fmt.Sprintf("%s is LIVE (PID %d). Enter relaunch, F3 stop, F4 view.", viewText(filtered[sel].Manifest.Name), filtered[sel].PID), theme.Success)
		} else if sel >= 0 {
			prompt += m.kit.Muted(fmt.Sprintf("Enter launches %s. F3 stop · F4 view · Esc prompt.", viewText(filtered[sel].Manifest.Name)))
		} else {
			prompt += m.kit.Muted("Esc returns to prompt.")
		}
	case m.currentMode() == modeHome:
		prompt += m.kit.Muted("Home view owns focus. Esc returns to prompt · Ctrl-P palette.")
	default:
		// Show a cursor-centered slice rather than allowing long pasted
		// input to push the prompt off screen. Cell width, not bytes,
		// controls the slice; the cursor's cell stays reserved so the
		// insertion point is on screen at the end of the input too.
		room := max(1, w-lipgloss.Width(prefix)-1)
		before := ui.Tail(string(m.input[:m.cursor]), room/2)
		after := ui.Fit(string(m.input[m.cursor:]), ui.Bounds{Width: room + 1 - lipgloss.Width(before), Height: 1})
		x = lipgloss.Width(prefix) + lipgloss.Width(before)
		return ui.Truncate(prompt+m.kit.Text(before)+m.kit.Text(after), w), x, x < w
	}
	return ui.Truncate(prompt, w), 0, false
}

func (m *Model) bindings(home homeView) []ui.Binding {
	exitHelp := "EXIT"
	if m.attached {
		exitHelp = "DETACH"
	}
	switch {
	case m.paletteOpen:
		items := m.filteredPaletteItems()
		return []ui.Binding{
			{Key: "Esc", Help: "CLOSE"},
			{Key: "Enter", Help: "EXECUTE", Disabled: len(items) == 0},
			{Key: "↑↓", Help: "NAVIGATE", Disabled: len(items) == 0},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	case m.taskView && m.tasks != nil:
		switch m.tasks.Mode() {
		case taskmanager.ModeLogs:
			return []ui.Binding{
				{Key: "Esc", Help: "BACK"}, {Key: "↑↓", Help: "SCROLL"}, {Key: "Ctrl-C", Help: exitHelp},
			}
		case taskmanager.ModeReceipt:
			return []ui.Binding{
				{Key: "Esc", Help: "BACK"}, {Key: "Ctrl-C", Help: exitHelp},
			}
		}
		return []ui.Binding{
			{Key: "Esc", Help: "PROMPT"}, {Key: "Enter", Help: "VIEW"},
			{Key: "l", Help: "LOGS"}, {Key: "c", Help: "RECEIPT"},
			{Key: "x", Help: "STOP"}, {Key: "r", Help: "REAP"},
			{Key: "↑↓", Help: "SELECT"}, {Key: "Ctrl-C", Help: exitHelp},
		}
	case m.notifView:
		return []ui.Binding{
			{Key: "Esc", Help: "PROMPT"}, {Key: "Enter", Help: "VIEW"},
			{Key: "d", Help: "DISMISS"}, {Key: "c", Help: "CLEAR"},
			{Key: "n", Help: "DND"}, {Key: "↑↓", Help: "SELECT"},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	case m.presentation != nil:
		v := m.presentation
		blocked := v.busy || v.instance == "" || v.data.State == sdk.ViewLoading
		action := max(0, v.focus-len(v.data.Fields))
		actionBlocked := blocked || action >= len(v.data.Actions) || v.data.Actions[action].Disabled
		return []ui.Binding{
			{Key: "Esc", Help: "CANCEL/BACK"}, {Key: "Ctrl-C", Help: exitHelp},
			{Key: "Tab", Help: "FOCUS", Disabled: blocked}, {Key: "Enter", Help: "ACTION", Disabled: actionBlocked},
			{Key: "↑↓", Help: "ITEM", Disabled: blocked}, {Key: "F2", Help: "APPS"},
		}
	case m.currentMode() == modeLauncher:
		filtered := m.filteredApps()
		return []ui.Binding{
			{Key: "Esc", Help: "PROMPT"},
			{Key: "Enter", Help: "RUN", Disabled: m.busy || len(filtered) == 0},
			{Key: "F3", Help: "STOP", Disabled: m.busy || len(filtered) == 0},
			{Key: "F4", Help: "VIEW", Disabled: m.busy || len(filtered) == 0},
			{Key: "F5", Help: "TASKS"},
			{Key: "↑↓", Help: "SELECT", Disabled: m.busy || len(filtered) == 0},
			{Key: "Ctrl-P", Help: "PALETTE"},
		}
	case m.currentMode() == modeHome:
		return []ui.Binding{
			{Key: "F1", Help: "PROMPT"},
			{Key: "F2", Help: "APPS"},
			{Key: "F5", Help: "TASKS"},
			{Key: "F6", Help: "ALERTS"},
			{Key: "Ctrl-P", Help: "PALETTE"},
			{Key: "↑↓", Help: "SELECT", Disabled: home.visible == 0},
			{Key: "Enter", Help: "OPEN", Disabled: home.visible == 0},
			{Key: "Ctrl-C", Help: exitHelp},
		}
	}
	return []ui.Binding{
		{Key: "Ctrl-C", Help: exitHelp},
		{Key: "F1", Help: "HOME"},
		{Key: "F2", Help: "APPS"},
		{Key: "F5", Help: "TASKS"},
		{Key: "F6", Help: "ALERTS"},
		{Key: "Tab", Help: "COMPLETE", Disabled: m.busy},
		{Key: "↑↓", Help: "HISTORY", Disabled: m.busy},
		{Key: "Ctrl-P", Help: "PALETTE"},
	}
}
