package shell

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
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
	slotPrompt
	slotHelp
)

// layerID names the layers drawn over the frame for Frame.Rect.
type layerID int

const (
	layerToasts layerID = iota + 1
	layerPalette
	layerDialog
)

// prioFrameSpacer is the collapse priority of the frame's blank spacer rows,
// the only rows that give way when the panel cannot fit every row, so tabs,
// badge, minBodyRows of body and the prompt always stay on screen.
const prioFrameSpacer = 1

// minBodyRows is the body height the spacers give way to: enough for a
// screen's heading and the item its keys act on. The smallest frame, 30×10,
// leaves exactly this much.
const minBodyRows = 3

// toastWidth is the widest the toast stack gets; a narrower body bounds it.
const toastWidth = 48

// backdropDim is how far the backdrop of a modal layer moves the screen
// beneath it toward the theme's background.
const backdropDim = 0.45

// The shell renders its frame only from 30×10; below that it shows sizeNote.
const (
	minFrameWidth  = 30
	minFrameHeight = 10
	sizeNote       = "GOSTALGIA\nResize to 30×10 or larger.\nCtrl-C exits."
	frameTitle     = "G O S T A L G I A  /  PERSONAL COMPUTING"
)

// launcherCompactHintsWidth is the narrowest frame that can show
// "Esc PROMPT · Enter RUN · F3 STOP" (32 cells). Narrower frames keep the
// shelf's three primary hints whole by using the shorter PROMPT label.
const launcherCompactHintsWidth = 32

// rootKey is everything a bloom root is built from. A kit change that keeps
// it equal keeps the root.
type rootKey struct {
	theme    string
	mode     ui.ColorMode
	reduced  bool
	fg, bg   [4]uint32
	mouseOff bool
}

func colorKey(c color.Color) [4]uint32 {
	if c == nil {
		return [4]uint32{}
	}
	r, g, b, a := c.RGBA()
	return [4]uint32{r, g, b, a}
}

func rootKeyOf(k ui.Kit, mouseOff bool) rootKey {
	fg, bg := k.Colors()
	return rootKey{theme: k.Theme().Name, mode: k.Mode(), reduced: k.Theme().ReducedMotion,
		fg: colorKey(fg), bg: colorKey(bg), mouseOff: mouseOff}
}

// root returns the model's own bloom root, built for its current kit and
// mouse setting and rebuilt only when the kit's effective theme, profile or
// motion, or the mouse setting, changes, so a config poll that re-applies
// the same settings keeps it. Its colors come from the theme, never from
// the terminal: Root.Init, which would query the host's colors, is never
// called. It never copies to a clipboard itself: copies go through
// copyCmd, under the operator's clipboard policy.
func (m *Model) root() *bloom.Root {
	if key := rootKeyOf(m.kit, m.mouseOff); m.ui == nil || m.uiKey != key {
		fg, bg := m.kit.Colors()
		opts := []bloom.Option{bloom.WithColors(fg, bg), bloom.WithClock(m.motionNow), bloom.WithCopyFlash(false),
			bloom.WithSelectionStyle(m.kit.MouseSelectionStyle())}
		if !m.mouseOff {
			opts = append(opts, bloom.WithTooltips(bloom.Tooltips{Style: m.kit.TooltipStyle()}))
		}
		m.ui = bloom.New(opts...)
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
		v.Content = m.toProfile(v.Content)
		if m.mouseOff {
			// Selectable text asks for mouse reports by itself; with the
			// mouse off the terminal keeps them, for its own selection.
			v.MouseMode, v.OnMouse = tea.MouseModeNone, nil
		} else if route := v.OnMouse; route != nil {
			// Bubble Tea runs the OnMouse of the last frame drawn, before
			// Update, so a report that arrives after mouse off but before
			// the next frame would still reach a wheel or click handler.
			v.OnMouse = func(msg tea.MouseMsg) tea.Cmd {
				if m.mouseOff {
					return nil
				}
				return route(msg)
			}
		}
	}
	v.AltScreen = true
	// Ask for kitty event types (key releases and repeat flags) on top of
	// the disambiguation v2 already requests. The request is progressive:
	// a terminal that doesn't speak the protocol ignores it, and the
	// negotiated answer arrives as KeyboardEnhancementsMsg.
	v.KeyboardEnhancements.ReportEventTypes = true
	return v
}

// toProfile converts composed frame content to the kit's color profile.
// Kit output is already in that profile, but backdrop blending mixes colors
// in 24-bit. Only the content is converted: cursor, screen-mode and keyboard
// requests travel in the View's own fields and never pass through here.
func (m *Model) toProfile(content string) string {
	if m.backdrop() == nil || !m.modalOpen() {
		return content
	}
	if m.kit.Mode() != ui.ANSI256 {
		return content
	}
	var buf bytes.Buffer
	w := colorprofile.Writer{Forward: &buf, Profile: colorprofile.ANSI256}
	if _, err := w.WriteString(content); err != nil {
		return content
	}
	return buf.String()
}

// modalOpen reports whether a modal layer owns input.
func (m *Model) modalOpen() bool { return m.paletteOpen || m.confirm != nil }

// backdrop is the effect a modal layer applies to the screen beneath it: a
// dim in color themes. Monochrome and ui.Plain get none, as they carry
// meaning in glyphs and plain contrast rather than in color, and neither
// does ANSI16, whose sixteen colors would round most dimmed cells back to
// their own color and leave a scattered few changed.
func (m *Model) backdrop() []fx.Effect {
	switch mode := m.kit.Mode(); {
	case mode != ui.ANSI256 && mode != ui.TrueColor, m.kit.Theme().Name == theme.Monochrome().Name:
		return nil
	}
	return []fx.Effect{fx.Dim{Amount: backdropDim}}
}

// solve lays the current frame out without drawing it.
func (m *Model) solve() *bloom.Frame {
	return bloom.Solve(m.width, m.height, m.layout())
}

// layout is the shell frame as a bloom tree: a kit Panel around tabs, badge,
// body and prompt, with the help bar below it, and the open palette or
// confirmation as a modal layer over all of it. It reads model state and
// never changes it.
func (m *Model) layout() bloom.Child {
	if m.width < minFrameWidth || m.height < minFrameHeight {
		note := ui.Sized(func(b ui.Bounds) string { return m.kit.Text(sizeNote) })
		return bloom.Column(bloom.Fill(note).ID(slotNote))
	}
	frame := m.frame(m.homeView())
	var child bloom.Child = frame
	switch {
	case m.confirm != nil:
		child = bloom.Overlay(frame, m.dialogLayer())
	case m.paletteOpen:
		child = bloom.Overlay(frame, m.paletteLayer())
	}
	return m.startupRevealNode(child)
}

// startupRevealNode draws the shell through the once-per-model CRT-style
// startup reveal while its finite value is active. A consumed or reduced
// startup attaches no transition at all, and the node disappears at progress
// one, so no render keeps an effect of its own.
func (m *Model) startupRevealNode(child bloom.Child) bloom.Child {
	if progress, ok := m.startupProgress(); ok {
		return bloom.Transition(progress, child, fx.Unfold{})
	}
	return child
}

// dialogLayer is the open confirmation as a modal layer over the screen.
// While its opening reveal is active the dialog draws with that progress;
// modal and focus guards are independent of the visual mask.
func (m *Model) dialogLayer() bloom.LayerNode {
	layer := m.kit.DialogLayer(m.confirmDialog()).Backdrop(m.backdrop()...).ID(layerDialog)
	if progress, ok := m.overlayProgress(); ok {
		layer = layer.Transition(progress, fx.Iris{})
	}
	return layer
}

func (m *Model) frame(home homeView) bloom.Child {
	body := m.body(home)
	if m.toastLines() > 0 {
		base := body
		body = ui.SizedNode(func(ctx bloom.Context) bloom.Child {
			return bloom.Overlay(base, m.toastLayer(ctx.Width, ctx.Height))
		})
	}
	rows := []bloom.Child{
		bloom.Fixed(1, m.tabsNode()).ID(slotTabs),
		bloom.Fixed(1, m.badgeNode()).ID(slotBadge),
		bloom.Fixed(1, nil).Collapsible(prioFrameSpacer),
		bloom.Fill(body).Min(minBodyRows).ID(slotBody),
		bloom.Fixed(1, nil).Collapsible(prioFrameSpacer),
		bloom.Fixed(1, m.promptNode()).ID(slotPrompt),
	}
	panel := m.kit.PanelNode(ui.Panel{Title: frameTitle}, bloom.Column(rows...))
	return bloom.Column(
		bloom.Fill(panel).ID(slotPanel),
		bloom.Fixed(1, m.kit.HelpBarLeaf(m.bindings(home))).ID(slotHelp),
	)
}

// runningAppCount is how many registered apps run. It reads the accepted
// app/list snapshot reconciled with the newest accepted process poll, so an
// external launch or stop appears without another app/list refresh. Services
// and arbitrary runtime processes are not apps and are not counted.
func (m *Model) runningAppCount() int {
	running := 0
	for _, a := range m.displayApps() {
		if a.Running {
			running++
		}
	}
	return running
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
	// The header counts are read from state the shell already maintains:
	// the accepted app/list snapshot and the notification manager. Rendering
	// neither polls nor changes acknowledgement.
	appsLabel := "Apps"
	if n := m.runningAppCount(); n > 0 {
		appsLabel = fmt.Sprintf("Apps (%d)", n)
	}
	notifLabel := "Notifications"
	if m.notifs != nil && m.notifs.UnseenCount() > 0 {
		notifLabel = fmt.Sprintf("Notifications (%d)", m.notifs.UnseenCount())
	}
	tabItems := []ui.Tab{
		{Label: "Home"},
		{Label: appsLabel},
		{Label: "Prompt"},
		{Label: "Tasks"},
		{Label: notifLabel},
	}
	if n := m.windows.Len(); n > 0 {
		tabItems = append(tabItems, ui.Tab{Label: fmt.Sprintf("Windows (%d)", n)})
		if m.onWindows() {
			active = len(tabItems) - 1
		}
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
	badge := m.kit.Badge(statusLabel, theme.Success, b.Width) + m.kit.Muted(fmt.Sprintf("   %s%sC: environment drive", userName, m.kit.Separator()))
	return m.kit.Truncate(badge, b.Width)
}

// toastLayer is the toast stack over a w×h body, anchored to its
// bottom-right corner over whatever the body shows there. The body is laid
// out the same with or without it: toasts never take rows from the screen
// beneath. The stack covers at most a third of the body, keeping the newest
// whole toasts when they do not all fit, so even the 30×10 frame keeps most
// of its body readable. The newest toast is never cut to fit that third: a
// wrapped one, such as how to allow a blocked copy, is only useful whole,
// so it may cover all but a row of the body until it expires. The layer only
// draws the manager's active toasts; their lifetime, caps and DND stay with
// notifications.Manager and its ticks.
func (m *Model) toastLayer(w, h int) bloom.LayerNode {
	width := min(w, toastWidth)
	blocks, notice := m.notifs.ToastBlocks(m.kit, width)
	room := max(1, h/3)
	// Each entry is one toast's rows (the notice is its own last entry).
	var entries [][]string
	var ids []string
	total := 0
	if n := len(blocks); n > 0 {
		newest := blocks[n-1]
		lines := newest.Rows[:min(len(newest.Rows), max(room, h-1))]
		if len(lines) < len(newest.Rows) {
			lines[len(lines)-1] = m.kit.Truncate(lines[len(lines)-1]+m.kit.Hint("…"), width)
		}
		entries = append(entries, lines)
		ids = append(ids, newest.ID)
		total = len(lines)
		kept := 1
		for ; kept < n && total+len(blocks[n-1-kept].Rows) <= room; kept++ {
			older := blocks[n-1-kept]
			entries = append([][]string{older.Rows}, entries...)
			ids = append([]string{older.ID}, ids...)
			total += len(older.Rows)
		}
		if notice != "" && kept == n && total < room {
			entries = append(entries, []string{notice})
			ids = append(ids, "")
			total++
		}
	}
	rows := make([]bloom.Child, 0, len(entries))
	for i, lines := range entries {
		block := m.kit.Leaf(func(ui.Bounds) string { return strings.Join(lines, "\n") })
		var child bloom.Child = bloom.Fixed(len(lines), block)
		if progress, ok := m.toastProgress(ids[i]); ok {
			child = bloom.Fixed(len(lines), bloom.Transition(progress, block, fx.Wipe{From: bloom.Bottom}))
		}
		rows = append(rows, child)
	}
	stack := bloom.Column(rows...)
	return bloom.Layer(stack).Anchor(bloom.End, bloom.End).Size(width, total).ID(layerToasts)
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
	case m.taskView && m.tasks != nil && m.tasks.Mode() == taskmanager.ModeLogs:
		return m.tasks.LogsNode(m.kit, m.baseMouseAllowed)
	case m.taskView && m.tasks != nil:
		return m.withMouse(m.kit.Leaf(func(b ui.Bounds) string {
			return m.tasks.RenderHover(m.kit, b, m.hoverTaskPID())
		}), m.taskHits, m.taskScroll)
	case m.notifView && m.notifs != nil:
		return m.withMouse(m.kit.Leaf(func(b ui.Bounds) string { return m.notifs.RenderPanel(m.kit, b) }),
			m.notificationHits, m.notificationScroll)
	case m.onWindows():
		v := m.focusedView()
		return m.withMouse(m.kit.Leaf(func(b ui.Bounds) string {
			lines := m.viewLines(v, b.Width, b.Height)
			for i, line := range lines {
				lines[i] = m.kit.Truncate(line, b.Width)
			}
			return strings.Join(lines, "\n")
		}), m.viewHits, nil)
	}
	switch m.currentMode() {
	case modeLauncher:
		return m.launcherNode()
	case modeHome:
		return m.homeNode(home)
	}
	return m.transcriptNode()
}

// promptNode is the prompt row. In editable prompt mode it places the real
// terminal cursor at the insertion point.
func (m *Model) promptNode() bloom.Child {
	return ui.SizedNode(func(ctx bloom.Context) bloom.Child {
		if ctx.Width <= 0 || ctx.Height <= 0 {
			return nil
		}
		line, x, editable := m.promptLine(ctx.Width)
		text := bloom.Text(m.kit.Text(ui.Fit(line, ui.Bounds{Width: ctx.Width, Height: 1})))
		// Under a confirmation the draft stays in view, but the dialog owns
		// input, so the prompt shows no insertion point.
		if editable && m.confirm == nil {
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
		ellipsis := m.kit.Hint("…")
		promptPath = "C:" + ellipsis + ui.Tail(promptPath, max(1, w/2-2-lipgloss.Width(ellipsis)))
	}
	prefix := promptPath + "> "
	prompt := m.kit.Heading(prefix)
	switch {
	case m.paletteOpen:
		prompt = m.kit.Heading("PALETTE> ") + m.kit.Muted(m.kit.Hint("Type to filter · ↑↓ navigate · Enter execute · Esc close"))
	case m.onWindows():
		prompt += m.kit.Muted("Window owns focus. Esc goes back, Ctrl-W closes.")
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
			prompt += m.kit.Muted(fmt.Sprintf("Enter launches %s. F3 stop%sF4 view%sEsc prompt.", viewText(filtered[sel].Manifest.Name), m.kit.Separator(), m.kit.Separator()))
		} else {
			prompt += m.kit.Muted("Esc returns to prompt.")
		}
	case m.currentMode() == modeHome:
		prompt += m.kit.Muted(m.kit.Hint("Home view owns focus. Esc returns to prompt · Ctrl-P palette."))
	default:
		// A draft that fits with its cursor is shown whole, even when it
		// exceeds half of the row. Only an overflowing draft scrolls, and
		// then its viewport slides around the cursor across every
		// available cell (see promptDraftViewport).
		prefixWidth := lipgloss.Width(prefix)
		before, after, offset := promptDraftViewport(m.input, m.cursor, max(1, w-prefixWidth))
		x = prefixWidth + offset
		return m.kit.Truncate(prompt+m.kit.Text(before)+m.kit.Text(after), w), x, x < w
	}
	return m.kit.Truncate(prompt, w), 0, false
}

// promptDraftViewport fits the editable draft into width cells. A draft that
// leaves a cell for the insertion cursor is returned whole. Otherwise the
// viewport slides around the cursor using the full width: one cell always
// stays free for the cursor, the grapheme after the cursor keeps its cells,
// and the text on the shorter side of the cursor gives way first so an end
// cursor reaches the row edge instead of stopping mid-row. offset is the
// cursor's cell within the returned text.
func promptDraftViewport(input []rune, cursor, width int) (before, after string, offset int) {
	if width <= 0 {
		return "", "", 0
	}
	if cursor < 0 || cursor > len(input) {
		cursor = max(0, min(cursor, len(input)))
	}
	if lipgloss.Width(string(input)) < width {
		before = string(input[:cursor])
		return before, string(input[cursor:]), lipgloss.Width(before)
	}
	// Leave a cell for the cursor and show the text after it up to half the
	// row, always whole for the next cluster; the rest goes to the text
	// before the cursor. Near either end that side takes the remaining
	// space, so an end cursor sits against the row edge instead of mid-row.
	next := 0
	if cursor < len(input) {
		next = max(1, ui.GraphemeWidth(ui.FirstCluster(string(input[cursor:]))))
	}
	afterBudget := min(lipgloss.Width(string(input[cursor:])), max(next, width/2))
	beforeBudget := min(width-1, width-afterBudget)
	before = ui.Tail(string(input[:cursor]), beforeBudget)
	after = ui.Fit(string(input[cursor:]), ui.Bounds{Width: max(0, width-lipgloss.Width(before)), Height: 1})
	return before, after, lipgloss.Width(before)
}

func (m *Model) bindings(home homeView) []ui.Binding {
	exitHelp := "EXIT"
	if m.attached {
		exitHelp = "DETACH"
	}
	switch {
	case m.confirm != nil:
		return []ui.Binding{
			{Key: "Tab", Help: "NEXT ACTION"}, {Key: "Enter", Help: "CHOOSE"},
			{Key: "Esc", Help: "CANCEL"}, {Key: "Ctrl-C", Help: exitHelp},
		}
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
			if m.tasks.Searching() {
				return []ui.Binding{
					{Key: "Enter", Help: "FIND"}, {Key: "Esc", Help: "CANCEL"}, {Key: "Ctrl-C", Help: exitHelp},
				}
			}
			return []ui.Binding{
				{Key: "Esc", Help: "BACK"}, {Key: "↑↓", Help: "SCROLL"}, {Key: "f", Help: "FOLLOW"},
				{Key: "/", Help: "SEARCH"}, {Key: "n/N", Help: "MATCH"}, {Key: "Ctrl-C", Help: exitHelp},
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
	case m.onWindows():
		v := m.focusedView()
		blocked := v.busy || v.instance == "" || v.data.State == sdk.ViewLoading
		action := max(0, v.focus-len(v.data.Fields))
		actionBlocked := blocked || action >= len(v.data.Actions) || v.data.Actions[action].Disabled
		// The window keys lead the presentation's own; Ctrl-C, which
		// every screen shares, gives way first.
		bindings := []ui.Binding{
			{Key: "Esc", Help: "CANCEL/BACK"}, {Key: "Ctrl-W", Help: "CLOSE"},
			{Key: "Tab", Help: "FOCUS", Disabled: blocked}, {Key: "Enter", Help: "ACTION", Disabled: actionBlocked},
			{Key: "↑↓", Help: "ITEM", Disabled: blocked}, {Key: "F8", Help: "NEXT", Disabled: m.windows.Len() < 2},
			{Key: "F2", Help: "APPS"}, {Key: "Ctrl-C", Help: exitHelp},
		}
		if v.focalBlock() != nil {
			// A hinted block's inspection route leads so a 30-cell help bar
			// keeps it whole rather than dropping it behind longer hints.
			mapHelp := "MAP"
			if v.inspect {
				mapHelp = "CONTROLS"
			}
			bindings = append([]ui.Binding{{Key: "m", Help: mapHelp, Disabled: blocked}}, bindings...)
		}
		return bindings
	case m.currentMode() == modeLauncher:
		filtered := m.filteredApps()
		sel := min(m.selected, len(filtered)-1)
		back := "PROMPT"
		if m.width < launcherCompactHintsWidth {
			// "Esc PROMPT · Enter RUN · F3 STOP" needs 32 cells. The 30-cell
			// frame shows all three primaries with the shorter label, and the
			// help bar drops any binding that would not fit whole.
			back = "BACK"
		}
		return []ui.Binding{
			{Key: "Esc", Help: back},
			{Key: "Enter", Help: "RUN", Disabled: m.busy || len(filtered) == 0},
			{Key: "F3", Help: "STOP", Disabled: m.busy || sel < 0 || !filtered[sel].Running},
			{Key: "F4", Help: "VIEW", Disabled: m.busy || sel < 0 || !filtered[sel].Running},
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
