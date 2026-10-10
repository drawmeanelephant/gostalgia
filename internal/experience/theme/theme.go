// Package theme defines Gostalgia's visual tokens as explicit, copyable values.
// It never inspects the host terminal or chooses colors from its background.
package theme

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

type State string

const (
	Normal   State = "normal"
	Disabled State = "disabled"
	Busy     State = "busy"
	Success  State = "success"
	Error    State = "error"
	Empty    State = "empty"
)

type Palette struct {
	Canvas, Surface, Text, Muted, Accent, Border color.Color
	Header, HeaderText                           color.Color
}

// Spacing values are terminal cells, not bytes or runes.
type Spacing struct {
	PanelX, Gap int
}

type Borders struct {
	Panel, Dialog lipgloss.Border
}

type Typography struct {
	TitleBold, HeadingBold, KeyBold bool
}

type Focus struct {
	Text, Fill, Border color.Color
	Marker             string
}

type Status struct {
	Text, Fill    color.Color
	Symbol, Label string
	Message       string
}

type States struct {
	Normal, Disabled, Busy, Success, Error, Empty Status
}

type Progress struct {
	Filled, Track string
}

// Theme has no maps, slices, renderer, or shared mutable styles. Start with a
// named theme and change a copy to customize a view.
type Theme struct {
	Name          string
	Palette       Palette
	Spacing       Spacing
	Borders       Borders
	Typography    Typography
	Focus         Focus
	States        States
	Progress      Progress
	ReducedMotion bool
}

// ASCIIBorder returns standard 7-bit ASCII borders (+, -, |) for modest terminals.
func ASCIIBorder() lipgloss.Border {
	return lipgloss.Border{
		Top: "-", Bottom: "-", Left: "|", Right: "|",
		TopLeft: "+", TopRight: "+", BottomLeft: "+", BottomRight: "+",
	}
}

// Nostalgia is the default warm cream, amber, and slate palette.
func Nostalgia() Theme {
	p := Palette{
		Canvas: lipgloss.Color("#E8DDC7"), Surface: lipgloss.Color("#FFF4DC"), Text: lipgloss.Color("#35434D"),
		Muted: lipgloss.Color("#746957"), Accent: lipgloss.Color("#8C510D"), Border: lipgloss.Color("#84745E"),
		Header: lipgloss.Color("#E7B65A"), HeaderText: lipgloss.Color("#35434D"),
	}
	return makeTheme("nostalgia", p, lipgloss.Color("#426640"), lipgloss.Color("#A33632"))
}

// Midnight is an explicitly selected slate theme, not an adaptive color.
func Midnight() Theme {
	p := Palette{
		Canvas: lipgloss.Color("#202B33"), Surface: lipgloss.Color("#2D3942"), Text: lipgloss.Color("#FFF4DC"),
		Muted: lipgloss.Color("#C0B6A3"), Accent: lipgloss.Color("#F2BE63"), Border: lipgloss.Color("#9B8C75"),
		Header: lipgloss.Color("#475664"), HeaderText: lipgloss.Color("#FFF4DC"),
	}
	return makeTheme("midnight", p, lipgloss.Color("#ACCE92"), lipgloss.Color("#FFABA0"))
}

// Monochrome provides a limited-color / monochrome theme using plain ASCII borders,
// high-contrast black/white palette, and text badges ([OK], [FAIL], [BUSY]).
func Monochrome() Theme {
	p := Palette{
		Canvas: lipgloss.Color("0"), Surface: lipgloss.Color("0"), Text: lipgloss.Color("15"),
		Muted: lipgloss.Color("7"), Accent: lipgloss.Color("15"), Border: lipgloss.Color("7"),
		Header: lipgloss.Color("7"), HeaderText: lipgloss.Color("0"),
	}
	asciiBorder := ASCIIBorder()
	return Theme{
		Name:    "monochrome",
		Palette: p,
		Spacing: Spacing{PanelX: 1, Gap: 1},
		Borders: Borders{Panel: asciiBorder, Dialog: asciiBorder},
		Typography: Typography{
			TitleBold:   true,
			HeadingBold: true,
			KeyBold:     true,
		},
		Focus: Focus{
			Text:   lipgloss.Color("0"),
			Fill:   lipgloss.Color("15"),
			Border: lipgloss.Color("15"),
			Marker: ">",
		},
		States: States{
			Normal:   Status{Text: lipgloss.Color("15"), Fill: lipgloss.Color("0"), Symbol: "[READY]", Label: "READY", Message: "Ready when you are."},
			Disabled: Status{Text: lipgloss.Color("7"), Fill: lipgloss.Color("0"), Symbol: "[DISABLED]", Label: "DISABLED", Message: "This action is unavailable."},
			Busy:     Status{Text: lipgloss.Color("15"), Fill: lipgloss.Color("0"), Symbol: "[BUSY]", Label: "BUSY", Message: "Working on your request…"},
			Success:  Status{Text: lipgloss.Color("15"), Fill: lipgloss.Color("0"), Symbol: "[OK]", Label: "OK", Message: "All set."},
			Error:    Status{Text: lipgloss.Color("15"), Fill: lipgloss.Color("0"), Symbol: "[FAIL]", Label: "FAIL", Message: "Something went wrong. Try again."},
			Empty:    Status{Text: lipgloss.Color("7"), Fill: lipgloss.Color("0"), Symbol: "[EMPTY]", Label: "EMPTY", Message: "Nothing here yet."},
		},
		Progress:      Progress{Filled: "#", Track: "-"},
		ReducedMotion: true,
	}
}

// HighContrast provides high-contrast tokens with distinct state glyphs and text cues.
func HighContrast() Theme {
	p := Palette{
		Canvas: lipgloss.Color("#000000"), Surface: lipgloss.Color("#000000"), Text: lipgloss.Color("#FFFFFF"),
		Muted: lipgloss.Color("#CCCCCC"), Accent: lipgloss.Color("#FFFF00"), Border: lipgloss.Color("#FFFFFF"),
		Header: lipgloss.Color("#FFFFFF"), HeaderText: lipgloss.Color("#000000"),
	}
	return Theme{
		Name:    "high-contrast",
		Palette: p,
		Spacing: Spacing{PanelX: 1, Gap: 1},
		Borders: Borders{Panel: lipgloss.ThickBorder(), Dialog: lipgloss.DoubleBorder()},
		Typography: Typography{
			TitleBold:   true,
			HeadingBold: true,
			KeyBold:     true,
		},
		Focus: Focus{
			Text:   lipgloss.Color("#000000"),
			Fill:   lipgloss.Color("#FFFF00"),
			Border: lipgloss.Color("#FFFF00"),
			Marker: "»",
		},
		States: States{
			Normal:   Status{Text: lipgloss.Color("#FFFFFF"), Fill: lipgloss.Color("#000000"), Symbol: "[READY]", Label: "READY", Message: "Ready when you are."},
			Disabled: Status{Text: lipgloss.Color("#888888"), Fill: lipgloss.Color("#000000"), Symbol: "[DISABLED]", Label: "DISABLED", Message: "This action is unavailable."},
			Busy:     Status{Text: lipgloss.Color("#FFFF00"), Fill: lipgloss.Color("#000000"), Symbol: "[BUSY]", Label: "BUSY", Message: "Working on your request…"},
			Success:  Status{Text: lipgloss.Color("#00FF00"), Fill: lipgloss.Color("#000000"), Symbol: "[OK]", Label: "OK", Message: "All set."},
			Error:    Status{Text: lipgloss.Color("#FF5555"), Fill: lipgloss.Color("#000000"), Symbol: "[FAIL]", Label: "FAIL", Message: "Something went wrong. Try again."},
			Empty:    Status{Text: lipgloss.Color("#CCCCCC"), Fill: lipgloss.Color("#000000"), Symbol: "[EMPTY]", Label: "EMPTY", Message: "Nothing here yet."},
		},
		Progress:      Progress{Filled: "━", Track: "─"},
		ReducedMotion: true,
	}
}

// HighContrastLight provides a high-contrast light theme variant.
func HighContrastLight() Theme {
	p := Palette{
		Canvas: lipgloss.Color("#FFFFFF"), Surface: lipgloss.Color("#FFFFFF"), Text: lipgloss.Color("#000000"),
		Muted: lipgloss.Color("#444444"), Accent: lipgloss.Color("#000080"), Border: lipgloss.Color("#000000"),
		Header: lipgloss.Color("#000000"), HeaderText: lipgloss.Color("#FFFFFF"),
	}
	return Theme{
		Name:    "high-contrast-light",
		Palette: p,
		Spacing: Spacing{PanelX: 1, Gap: 1},
		Borders: Borders{Panel: lipgloss.ThickBorder(), Dialog: lipgloss.DoubleBorder()},
		Typography: Typography{
			TitleBold:   true,
			HeadingBold: true,
			KeyBold:     true,
		},
		Focus: Focus{
			Text:   lipgloss.Color("#FFFFFF"),
			Fill:   lipgloss.Color("#000080"),
			Border: lipgloss.Color("#000080"),
			Marker: "»",
		},
		States: States{
			Normal:   Status{Text: lipgloss.Color("#000000"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[READY]", Label: "READY", Message: "Ready when you are."},
			Disabled: Status{Text: lipgloss.Color("#666666"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[DISABLED]", Label: "DISABLED", Message: "This action is unavailable."},
			Busy:     Status{Text: lipgloss.Color("#804000"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[BUSY]", Label: "BUSY", Message: "Working on your request…"},
			Success:  Status{Text: lipgloss.Color("#006600"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[OK]", Label: "OK", Message: "All set."},
			Error:    Status{Text: lipgloss.Color("#AA0000"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[FAIL]", Label: "FAIL", Message: "Something went wrong. Try again."},
			Empty:    Status{Text: lipgloss.Color("#444444"), Fill: lipgloss.Color("#FFFFFF"), Symbol: "[EMPTY]", Label: "EMPTY", Message: "Nothing here yet."},
		},
		Progress:      Progress{Filled: "━", Track: "─"},
		ReducedMotion: true,
	}
}

// WithReducedMotion returns a copy of the theme with reduced motion configured.
func (t Theme) WithReducedMotion(enabled bool) Theme {
	t.ReducedMotion = enabled
	return t
}

func makeTheme(name string, p Palette, success, failure color.Color) Theme {
	return Theme{
		Name: name, Palette: p,
		Spacing:    Spacing{PanelX: 1, Gap: 1},
		Borders:    Borders{Panel: lipgloss.ThickBorder(), Dialog: lipgloss.DoubleBorder()},
		Typography: Typography{TitleBold: true, HeadingBold: true, KeyBold: true},
		Focus:      Focus{Text: p.Surface, Fill: p.Text, Border: p.Accent, Marker: "›"},
		States: States{
			Normal:   Status{Text: p.Text, Fill: p.Surface, Symbol: "○", Label: "READY", Message: "Ready when you are."},
			Disabled: Status{Text: p.Muted, Fill: p.Canvas, Symbol: "–", Label: "DISABLED", Message: "This action is unavailable."},
			Busy:     Status{Text: p.Accent, Fill: p.Surface, Symbol: "…", Label: "WORKING", Message: "Working on your request…"},
			Success:  Status{Text: success, Fill: p.Surface, Symbol: "✓", Label: "DONE", Message: "All set."},
			Error:    Status{Text: failure, Fill: p.Surface, Symbol: "!", Label: "ERROR", Message: "Something went wrong. Try again."},
			Empty:    Status{Text: p.Muted, Fill: p.Surface, Symbol: "◇", Label: "EMPTY", Message: "Nothing here yet."},
		},
		Progress: Progress{Filled: "━", Track: "·"},
	}
}

// Status returns normal tokens for an unknown state.
func (t Theme) Status(state State) Status {
	switch state {
	case Disabled:
		return t.States.Disabled
	case Busy:
		return t.States.Busy
	case Success:
		return t.States.Success
	case Error:
		return t.States.Error
	case Empty:
		return t.States.Empty
	default:
		return t.States.Normal
	}
}
