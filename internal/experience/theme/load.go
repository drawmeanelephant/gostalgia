package theme

import (
	"github.com/charmbracelet/lipgloss"
	"gostalgia/internal/themedef"
)

// FromDefinition converts a validated user theme definition (see
// internal/themedef) into renderable tokens. The definition arrives fully
// defaulted and validated; this is a mechanical mapping onto the same
// explicit token set the named themes use.
func FromDefinition(d themedef.Definition) Theme {
	return Theme{
		Name:    d.Name,
		Palette: paletteFromDef(d.Palette),
		Spacing: Spacing{PanelX: d.Spacing.PanelX, Gap: d.Spacing.Gap},
		Borders: Borders{
			Panel:  borderFromDef(d.Borders.Panel),
			Dialog: borderFromDef(d.Borders.Dialog),
		},
		Typography: Typography{
			TitleBold:   d.Typography.TitleBold,
			HeadingBold: d.Typography.HeadingBold,
			KeyBold:     d.Typography.KeyBold,
		},
		Focus: Focus{
			Text:   lipgloss.Color(d.Focus.Text),
			Fill:   lipgloss.Color(d.Focus.Fill),
			Border: lipgloss.Color(d.Focus.Border),
			Marker: d.Focus.Marker,
		},
		States: States{
			Normal:   statusFromDef(d.States.Normal),
			Disabled: statusFromDef(d.States.Disabled),
			Busy:     statusFromDef(d.States.Busy),
			Success:  statusFromDef(d.States.Success),
			Error:    statusFromDef(d.States.Error),
			Empty:    statusFromDef(d.States.Empty),
		},
		Progress:      Progress{Filled: d.Progress.Filled, Track: d.Progress.Track},
		ReducedMotion: d.ReducedMotion,
	}
}

func paletteFromDef(p themedef.Palette) Palette {
	return Palette{
		Canvas:     lipgloss.Color(p.Canvas),
		Surface:    lipgloss.Color(p.Surface),
		Text:       lipgloss.Color(p.Text),
		Muted:      lipgloss.Color(p.Muted),
		Accent:     lipgloss.Color(p.Accent),
		Border:     lipgloss.Color(p.Border),
		Header:     lipgloss.Color(p.Header),
		HeaderText: lipgloss.Color(p.HeaderText),
	}
}

func statusFromDef(s themedef.Status) Status {
	return Status{
		Text:    lipgloss.Color(s.Text),
		Fill:    lipgloss.Color(s.Fill),
		Symbol:  s.Symbol,
		Label:   s.Label,
		Message: s.Message,
	}
}

func borderFromDef(b themedef.Border) lipgloss.Border {
	if b.Glyphs != nil {
		return lipgloss.Border{
			Top:         b.Glyphs.Top,
			Bottom:      b.Glyphs.Bottom,
			Left:        b.Glyphs.Left,
			Right:       b.Glyphs.Right,
			TopLeft:     b.Glyphs.TopLeft,
			TopRight:    b.Glyphs.TopRight,
			BottomLeft:  b.Glyphs.BottomLeft,
			BottomRight: b.Glyphs.BottomRight,
		}
	}
	switch b.Preset {
	case "rounded":
		return lipgloss.RoundedBorder()
	case "double":
		return lipgloss.DoubleBorder()
	case "block":
		return lipgloss.BlockBorder()
	case "ascii":
		return ASCIIBorder()
	case "thick":
		return lipgloss.ThickBorder()
	default:
		return lipgloss.NormalBorder()
	}
}
