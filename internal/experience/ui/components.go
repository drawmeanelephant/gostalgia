package ui

import (
	"fmt"
	"math"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

type Panel struct {
	Title   string
	Body    string // Trusted text or composed component output; sanitize external data.
	State   theme.State
	Focused bool
}

// PanelContentBounds reports space available inside a panel, excluding its
// border, filled title row, and horizontal padding.
func (k Kit) PanelContentBounds(bounds Bounds) Bounds {
	return Bounds{
		Width:  max(0, bounds.Width-2-2*max(0, k.theme.Spacing.PanelX)),
		Height: max(0, bounds.Height-3),
	}
}

func (k Kit) Panel(p Panel, bounds Bounds) string {
	return k.panel(p, bounds, k.theme.Borders.Panel)
}

func (k Kit) panel(p Panel, bounds Bounds, border lipgloss.Border) string {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}
	title := label(p.Title)
	if p.Focused && p.State != theme.Disabled {
		title = k.theme.Focus.Marker + " " + title
	}
	if bounds.Width < 2 || bounds.Height < 3 {
		return Fit(k.Heading(k.Truncate(title, bounds.Width))+"\n"+p.Body, bounds)
	}
	palette := k.theme.Palette
	color := palette.Border
	titleFG, titleBG := palette.HeaderText, palette.Header
	if p.State != "" && p.State != theme.Normal {
		s := k.theme.Status(p.State)
		color, titleFG, titleBG = s.Text, s.Text, s.Fill
	}
	if p.Focused && p.State != theme.Disabled {
		f := k.theme.Focus
		color, titleFG, titleBG = f.Border, f.Text, f.Fill
	}
	stroke := func(s string) string { return k.paint(s, color, palette.Surface, false) }
	inner := bounds.Width - 2
	padding := k.panelPadding(bounds.Width)
	line := func(text string) string {
		return stroke(border.Left) + k.Text(strings.Repeat(" ", padding)+
			Fit(text, Bounds{inner - 2*padding, 1})+strings.Repeat(" ", padding)) + stroke(border.Right)
	}
	out := []string{
		stroke(border.TopLeft + strings.Repeat(border.Top, inner) + border.TopRight),
		stroke(border.Left) + k.paint(Fit(" "+k.Truncate(title, max(0, inner-1)), Bounds{inner, 1}),
			titleFG, titleBG, k.theme.Typography.TitleBold) + stroke(border.Right),
	}
	body := strings.Split(p.Body, "\n")
	for i := 0; i < bounds.Height-3; i++ {
		text := ""
		if i < len(body) {
			text = body[i]
		}
		if inner-2*padding == 0 {
			out = append(out, stroke(border.Left)+k.Text(strings.Repeat(" ", inner))+stroke(border.Right))
		} else {
			out = append(out, line(text))
		}
	}
	out = append(out, stroke(border.BottomLeft+strings.Repeat(border.Bottom, inner)+border.BottomRight))
	return Fit(strings.Join(out, "\n"), bounds)
}

type Tab struct {
	Label    string
	Disabled bool
}

// TabSpan is where Tabs draws one tab on its row: the tab's index in the
// tabs given and the cells it takes.
type TabSpan struct {
	Index, X, Width int
}

// Tabs keeps the active tab visible by dropping leading tabs when necessary.
// Invalid active indices select nothing; a disabled tab is never highlighted.
func (k Kit) Tabs(tabs []Tab, active, width int) string {
	row, _ := k.tabs(tabs, active, width)
	return row
}

// TabSpans reports where Tabs draws each tab it shows in width cells, left
// to right. Tabs it drops to keep the active one visible have no span.
func (k Kit) TabSpans(tabs []Tab, active, width int) []TabSpan {
	_, spans := k.tabs(tabs, active, width)
	return spans
}

func (k Kit) tabs(tabs []Tab, active, width int) (string, []TabSpan) {
	if width <= 0 {
		return "", nil
	}
	gap := strings.Repeat(" ", max(0, k.theme.Spacing.Gap))
	parts := make([]string, len(tabs))
	for i, tab := range tabs {
		text := k.Truncate(label(tab.Label), max(0, width-2))
		// Header labels use the header foreground token, not body text:
		// HighContrast's header is light with dark text, unlike its canvas.
		fg, bg := k.theme.Palette.HeaderText, k.theme.Palette.Header
		if tab.Disabled {
			text = "(" + text + ")"
			s := k.theme.Status(theme.Disabled)
			fg, bg = s.Text, s.Fill
		} else if i == active {
			text = "[" + text + "]"
			fg, bg = k.theme.Focus.Text, k.theme.Focus.Fill
		} else {
			text = " " + text + " "
		}
		parts[i] = k.paint(text, fg, bg, i == active && !tab.Disabled)
	}
	start := 0
	if active >= 0 && active < len(parts) {
		for start < active && ansi.StringWidth(strings.Join(parts[start:active+1], gap)) > width {
			start++
		}
	}
	var visible []string
	var spans []TabSpan
	used := 0
	for i, part := range parts[start:] {
		cells := ansi.StringWidth(part)
		x := used
		if len(visible) > 0 {
			x += len(gap)
			cells += len(gap)
			if used+cells > width {
				break
			}
		}
		visible = append(visible, part)
		spans = append(spans, TabSpan{Index: start + i, X: x, Width: min(ansi.StringWidth(part), width-x)})
		used += cells
	}
	return k.paint(Fit(strings.Join(visible, gap), Bounds{width, 1}),
		k.theme.Palette.HeaderText, k.theme.Palette.Header, false), spans
}

// Badge uses an explicit symbol and label so state isn't communicated by color
// alone. An empty label uses the state's named label. In monochrome and high-contrast
// modes with bracketed text badges (e.g. [OK], [FAIL], [BUSY]), the text badge is used directly.
func (k Kit) Badge(text string, state theme.State, width int) string {
	s := k.theme.Status(state)
	var content string
	if text == "" {
		if strings.HasPrefix(s.Symbol, "[") && strings.HasSuffix(s.Symbol, "]") {
			content = s.Symbol
		} else {
			content = s.Symbol + " " + label(s.Label)
		}
	} else {
		if s.Symbol != "" {
			content = s.Symbol + " " + label(text)
		} else {
			content = label(text)
		}
	}
	return k.paint(k.Truncate(content, width), s.Text, s.Fill, true)
}

// Severity renders a notification's level cue: the level's own symbol and
// label in the state's colors, or the literal bracketed label in Monochrome
// and Plain, where color and weight alone cannot carry severity. Plain emits
// no escape sequences. The symbol and label are shell-owned vocabulary, so
// external text never supplies either.
func (k Kit) Severity(symbol, name string, state theme.State, width int) string {
	if width <= 0 {
		return ""
	}
	cue := symbol + " " + label(name)
	if k.ASCIIChrome() || k.Mode() == Plain {
		cue = "[" + label(name) + "]"
	}
	s := k.theme.Status(state)
	return k.paint(k.Truncate(cue, width), s.Text, s.Fill, true)
}

type AppCard struct {
	Name, ID, Version, Description string
	// Icon is optional untrusted icon art. AppCard sanitizes it and draws at
	// most two lines of twelve display cells beside the status and identity;
	// missing or unusable art uses a generic ASCII glyph, and a card too
	// narrow for both identity and art shows no icon column at all.
	Icon    string
	Status  string
	State   theme.State
	Focused bool
}

func (k Kit) AppCard(card AppCard, bounds Bounds) string {
	// A border and title would consume all three rows of a compact card.
	// Use text instead, retaining the name and real status before other detail.
	if bounds.Height < 4 {
		title := label(card.Name)
		marker := ""
		if card.Focused && card.State != theme.Disabled {
			marker = k.theme.Focus.Marker + " "
		}
		if bounds.Height == 1 {
			// One row is all a shelf with a launch rejection leaves at
			// 30x10. Pack the name and the actual state, cutting the name
			// rather than the state when both cannot fit.
			badge := k.Badge(card.Status, card.State, bounds.Width)
			nameWidth := bounds.Width - ansi.StringWidth(badge) - 2 - ansi.StringWidth(marker)
			if nameWidth < 1 {
				return Fit(badge, bounds)
			}
			name := k.Truncate(title, nameWidth)
			header := k.StatusText(name, card.State)
			if marker != "" {
				header = k.Selection(marker + name)
			}
			return Fit(header+"  "+badge, bounds)
		}
		header := k.StatusText(title, card.State)
		if marker != "" {
			header = k.Selection(marker + title)
		}
		return Fit(header+"\n"+k.Badge(card.Status, card.State, bounds.Width)+"\n"+
			label(card.Description), bounds)
	}
	content := k.PanelContentBounds(bounds)
	details := label(card.ID)
	if card.Version != "" {
		details += "  v" + label(card.Version)
	}
	status := k.Badge(card.Status, card.State, content.Width)
	body := status + "\n" + k.Muted(k.Truncate(details, content.Width))
	if card.Description != "" {
		body += "\n" + Wrap(Sanitize(card.Description), content.Width)
	}
	body = k.appCardIcon(body, card.Icon, content.Width)
	return k.Panel(Panel{Title: card.Name, Body: body, State: card.State, Focused: card.Focused}, bounds)
}

// appCardIcon places icon art in the right-hand columns of a card body's
// first lines, beside the status and identity. The column is reserved only
// when the detail text keeps a readable share of the width, so missing art
// never leaves a decorative gap and a narrow card protects its name/state.
func (k Kit) appCardIcon(body, raw string, width int) string {
	const (
		iconGap   = 2
		minDetail = 18
	)
	icon := k.appIcon(raw)
	iconWidth := 0
	for _, line := range icon {
		iconWidth = max(iconWidth, ansi.StringWidth(line))
	}
	if iconWidth == 0 || width < iconWidth+iconGap+minDetail {
		return body
	}
	rows := strings.Split(body, "\n")
	detailWidth := width - iconWidth - iconGap
	for i := 0; i < len(rows) && i < len(icon); i++ {
		pad := strings.Repeat(" ", iconWidth-ansi.StringWidth(icon[i]))
		rows[i] = Fit(rows[i], Bounds{Width: detailWidth, Height: 1}) +
			strings.Repeat(" ", iconGap) + pad + k.Muted(icon[i])
	}
	return strings.Join(rows, "\n")
}

type Action struct {
	Label    string
	Disabled bool
}

type Dialog struct {
	Title, Body string
	Actions     []Action
	Active      int
	State       theme.State
}

// Dialog returns a centered, bounded canvas, not an overlay on an existing
// screen. The caller owns visibility, focus, and confirmation behavior;
// DialogLayer composes the same box over a screen instead.
func (k Kit) Dialog(dialog Dialog, bounds Bounds) string {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}
	panel, box := k.dialogBox(dialog, bounds)
	left, top := (bounds.Width-box.Width)/2, (bounds.Height-box.Height)/2
	lines := strings.Split(panel, "\n")
	for i := range lines {
		lines[i] = strings.Repeat(" ", left) + lines[i]
	}
	canvas := strings.Repeat("\n", top) + strings.Join(lines, "\n")
	return k.paint(Fit(canvas, bounds), k.theme.Palette.Text, k.theme.Palette.Canvas, false)
}

// dialogBox is the dialog's bordered box alone: at most 48 cells wide and as
// tall as its wrapped body and actions, within bounds. box is its size.
func (k Kit) dialogBox(dialog Dialog, bounds Bounds) (panel string, box Bounds) {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return "", Bounds{}
	}
	width := min(bounds.Width, 48)
	content := k.PanelContentBounds(Bounds{width, bounds.Height})
	// Word-wrap first so a confirmation never splits a word across lines;
	// Wrap then breaks anything still too wide.
	body := Wrap(ansi.Wrap(Sanitize(dialog.Body), content.Width, ""), content.Width)
	if len(dialog.Actions) > 0 {
		bodyLines := strings.Split(body, "\n")
		// A blank row separates the actions only when the whole body fits
		// with it; otherwise the row goes to the body.
		gap := "\n\n"
		if len(bodyLines) > content.Height-2 {
			gap = "\n"
		}
		body = strings.Join(bodyLines[:min(len(bodyLines), max(0, content.Height-len(gap)))], "\n")
		tabs := make([]Tab, len(dialog.Actions))
		for i, action := range dialog.Actions {
			tabs[i] = Tab(action)
		}
		actions := k.Tabs(tabs, dialog.Active, content.Width)
		if content.Height >= 2 && body != "" {
			body += gap + actions
		} else {
			body = actions
		}
	}
	height := min(bounds.Height, max(4, len(strings.Split(body, "\n"))+3))
	box = Bounds{width, height}
	return k.panel(Panel{Title: dialog.Title, Body: body, State: dialog.State, Focused: true},
		box, k.theme.Borders.Dialog), box
}

type Progress struct {
	Label         string
	Value         float64 // Fraction in [0,1]; out-of-range values are clamped.
	Indeterminate bool
	State         theme.State
}

// Progress is a static indicator. Busy/indeterminate rendering never schedules
// a tick or fabricates a percentage. bubbles/progress was evaluated and lost
// (issue #185): its model is a spring-animated color ramp, while the theme
// contract is two static glyphs (Progress.Filled/Track, ASCII under
// monochrome) plus an explicitly non-animated indeterminate state.
func (k Kit) Progress(progress Progress, width int) string {
	if width <= 0 {
		return ""
	}
	value := progress.Value
	if math.IsNaN(value) {
		value = 0
	}
	value = max(0, min(1, value))
	suffix := fmt.Sprintf(" %3d%%", int(math.Round(value*100)))
	if progress.Indeterminate {
		suffix = " " + k.theme.Status(theme.Busy).Label
	}
	prefix := ""
	if progress.Label != "" {
		prefix = k.Truncate(label(progress.Label), width/3) + " "
	}
	cells := width - ansi.StringWidth(prefix+suffix) - 2
	if cells < 1 {
		return k.StatusText(Fit(k.Truncate(strings.TrimSpace(suffix), width), Bounds{width, 1}), progress.State)
	}
	filled := int(math.Round(value * float64(cells)))
	bar := strings.Repeat(k.theme.Progress.Filled, filled) + strings.Repeat(k.theme.Progress.Track, cells-filled)
	if progress.Indeterminate {
		if k.theme.ReducedMotion {
			bar = strings.Repeat(k.theme.Progress.Track, cells)
		} else {
			bar = k.theme.Status(theme.Busy).Symbol + strings.Repeat(k.theme.Progress.Track, cells-1)
		}
	}
	return k.StatusText(Fit(prefix+"["+bar+"]"+suffix, Bounds{width, 1}), progress.State)
}

type Notice struct {
	Title, Message string
	State          theme.State
}

// Notice supplies friendly defaults for empty, busy, disabled, success, and
// error states. It reports state without decorative animation.
func (k Kit) Notice(notice Notice, bounds Bounds) string {
	s := k.theme.Status(notice.State)
	title, message := notice.Title, notice.Message
	if title == "" {
		title = s.Label
	}
	if message == "" {
		message = s.Message
	}
	content := k.PanelContentBounds(bounds)
	body := k.StatusText(Wrap(Sanitize(message), content.Width), notice.State)
	return k.Panel(Panel{Title: s.Symbol + " " + label(title), Body: body, State: notice.State}, bounds)
}

type Binding struct {
	Key, Help string
	Disabled  bool
}

// HelpBar is one row. Put the most important contextual bindings first;
// those that don't fit are dropped whole rather than clipped mid-action, so
// a narrow terminal never shows a hint like "F3 ST" with no action. It
// returns as many whole hints as fit.
// bubbles/help was evaluated and lost (issue #185): it drops disabled
// bindings outright and applies one style set to the whole bar, while the
// contract renders disabled keys parenthesized in the disabled state.
func (k Kit) HelpBar(bindings []Binding, width int) string {
	if width <= 0 {
		return ""
	}
	separator := k.Separator()
	var parts []string
	used := 0
	for _, binding := range bindings {
		s := k.theme.Status(theme.Normal)
		keyText := label(k.KeyLabel(binding.Key))
		if binding.Disabled {
			s = k.theme.Status(theme.Disabled)
			keyText = "(" + keyText + ")"
		}
		key := k.paint(keyText, s.Text, s.Fill, k.theme.Typography.KeyBold)
		part := key + " " + k.paint(label(binding.Help), s.Text, s.Fill, false)
		cells := ansi.StringWidth(part)
		if len(parts) > 0 {
			cells += ansi.StringWidth(separator)
		}
		if used+cells > width {
			break
		}
		parts = append(parts, part)
		used += cells
	}
	return k.Text(Fit(strings.Join(parts, separator), Bounds{width, 1}))
}
