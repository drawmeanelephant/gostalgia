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
		return Fit(k.Heading(Truncate(title, bounds.Width))+"\n"+p.Body, bounds)
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
	padding := min(max(0, k.theme.Spacing.PanelX), inner/2)
	line := func(text string) string {
		return stroke(border.Left) + k.Text(strings.Repeat(" ", padding)+
			Fit(text, Bounds{inner - 2*padding, 1})+strings.Repeat(" ", padding)) + stroke(border.Right)
	}
	out := []string{
		stroke(border.TopLeft + strings.Repeat(border.Top, inner) + border.TopRight),
		stroke(border.Left) + k.paint(Fit(" "+Truncate(title, max(0, inner-1)), Bounds{inner, 1}),
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

// Tabs keeps the active tab visible by dropping leading tabs when necessary.
// Invalid active indices select nothing; a disabled tab is never highlighted.
func (k Kit) Tabs(tabs []Tab, active, width int) string {
	if width <= 0 {
		return ""
	}
	gap := strings.Repeat(" ", max(0, k.theme.Spacing.Gap))
	parts := make([]string, len(tabs))
	for i, tab := range tabs {
		text := Truncate(label(tab.Label), max(0, width-2))
		fg, bg := k.theme.Palette.Text, k.theme.Palette.Header
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
	used := 0
	for _, part := range parts[start:] {
		cells := ansi.StringWidth(part)
		if len(visible) > 0 {
			cells += len(gap)
			if used+cells > width {
				break
			}
		}
		visible = append(visible, part)
		used += cells
	}
	return k.paint(Fit(strings.Join(visible, gap), Bounds{width, 1}),
		k.theme.Palette.Text, k.theme.Palette.Header, false)
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
	return k.paint(Truncate(content, width), s.Text, s.Fill, true)
}

type AppCard struct {
	Name, ID, Version, Description string
	Status                         string
	State                          theme.State
	Focused                        bool
}

func (k Kit) AppCard(card AppCard, bounds Bounds) string {
	// A border and title would consume all three rows of a compact card.
	// Use text instead, retaining the name and real status before other detail.
	if bounds.Height < 4 {
		title := label(card.Name)
		header := k.StatusText(title, card.State)
		if card.Focused && card.State != theme.Disabled {
			header = k.Selection(k.theme.Focus.Marker + " " + title)
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
	body := status + "\n" + k.Muted(Truncate(details, content.Width))
	if card.Description != "" {
		body += "\n" + Wrap(Sanitize(card.Description), content.Width)
	}
	return k.Panel(Panel{Title: card.Name, Body: body, State: card.State, Focused: card.Focused}, bounds)
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
// screen. The caller owns visibility, focus, and confirmation behavior.
func (k Kit) Dialog(dialog Dialog, bounds Bounds) string {
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return ""
	}
	width := min(bounds.Width, 48)
	content := k.PanelContentBounds(Bounds{width, bounds.Height})
	body := Wrap(Sanitize(dialog.Body), content.Width)
	if len(dialog.Actions) > 0 {
		bodyLines := strings.Split(body, "\n")
		body = strings.Join(bodyLines[:min(len(bodyLines), max(0, content.Height-2))], "\n")
		tabs := make([]Tab, len(dialog.Actions))
		for i, action := range dialog.Actions {
			tabs[i] = Tab(action)
		}
		actions := k.Tabs(tabs, dialog.Active, content.Width)
		if content.Height >= 3 {
			body += "\n\n" + actions
		} else {
			body = actions
		}
	}
	height := min(bounds.Height, max(4, len(strings.Split(body, "\n"))+3))
	panel := k.panel(Panel{Title: dialog.Title, Body: body, State: dialog.State, Focused: true},
		Bounds{width, height}, k.theme.Borders.Dialog)
	left, top := (bounds.Width-width)/2, (bounds.Height-height)/2
	lines := strings.Split(panel, "\n")
	for i := range lines {
		lines[i] = strings.Repeat(" ", left) + lines[i]
	}
	canvas := strings.Repeat("\n", top) + strings.Join(lines, "\n")
	return k.paint(Fit(canvas, bounds), k.theme.Palette.Text, k.theme.Palette.Canvas, false)
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
		prefix = Truncate(label(progress.Label), width/3) + " "
	}
	cells := width - ansi.StringWidth(prefix+suffix) - 2
	if cells < 1 {
		return k.StatusText(Fit(Truncate(strings.TrimSpace(suffix), width), Bounds{width, 1}), progress.State)
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

// HelpBar is one row. Put the most important contextual bindings first; those
// that don't fit are clipped rather than wrapping over a prompt or dialog.
// bubbles/help was evaluated and lost (issue #185): it drops disabled
// bindings outright and applies one style set to the whole bar, while the
// contract renders disabled keys parenthesized in the disabled state.
func (k Kit) HelpBar(bindings []Binding, width int) string {
	var parts []string
	for _, binding := range bindings {
		s := k.theme.Status(theme.Normal)
		keyText := label(binding.Key)
		if binding.Disabled {
			s = k.theme.Status(theme.Disabled)
			keyText = "(" + keyText + ")"
		}
		key := k.paint(keyText, s.Text, s.Fill, k.theme.Typography.KeyBold)
		parts = append(parts, key+" "+k.paint(label(binding.Help), s.Text, s.Fill, false))
	}
	return k.Text(Fit(strings.Join(parts, " · "), Bounds{width, 1}))
}
