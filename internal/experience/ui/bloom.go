package ui

import (
	"image/color"
	"strings"

	"bloomfx.dev/bloom"
)

// Leaf is kit output placed in a bloom layout. It is a bloom.Layouter: the
// layout decides its exact rectangle and the kit renders into that
// rectangle, so components that take Bounds keep doing their own sizing.
// A leaf has no natural size of its own unless Natural sets one; it takes
// whatever its slot gives it. Its render function must not change state,
// because bloom may lay a leaf out more than once per frame.
type Leaf struct {
	render  func(Bounds) string
	style   func(string) string
	natural bloom.Size
}

// Sized places render's output exactly as rendered, fitted to its bounds.
func Sized(render func(Bounds) string) Leaf {
	return Leaf{render: render}
}

// Leaf places render's output as a panel body line: fitted to its bounds
// and painted on the theme's surface, as Panel paints its own body.
func (k Kit) Leaf(render func(Bounds) string) Leaf {
	return Leaf{render: render, style: k.Text}
}

// Natural sets the size the leaf asks for in an Auto slot.
func (l Leaf) Natural(width, height int) Leaf {
	l.natural = bloom.Size{W: max(0, width), H: max(0, height)}
	return l
}

// View implements bloom.Child. A leaf only draws at a size its layout gives
// it, so outside a layout it is empty.
func (l Leaf) View() string { return "" }

// Measure implements bloom.Measurer.
func (l Leaf) Measure(bloom.Size) bloom.Size { return l.natural }

// Layout implements bloom.Layouter.
func (l Leaf) Layout(ctx bloom.Context) bloom.Child {
	bounds := Bounds{Width: ctx.Width, Height: ctx.Height}
	if bounds.Width <= 0 || bounds.Height <= 0 || l.render == nil {
		return nil
	}
	out := Fit(l.render(bounds), bounds)
	if l.style != nil {
		lines := strings.Split(out, "\n")
		for i := range lines {
			lines[i] = l.style(lines[i])
		}
		out = strings.Join(lines, "\n")
	}
	return bloom.Text(out)
}

// TabsLeaf is Tabs sized by the layout.
func (k Kit) TabsLeaf(tabs []Tab, active int) Leaf {
	return k.Leaf(func(b Bounds) string { return k.Tabs(tabs, active, b.Width) })
}

// HelpBarLeaf is HelpBar sized by the layout.
func (k Kit) HelpBarLeaf(bindings []Binding) Leaf {
	return Sized(func(b Bounds) string { return k.HelpBar(bindings, b.Width) })
}

// PanelContentRect reports where Panel draws its body inside bounds: the
// offset of the body's top-left cell and the body's size. It is the single
// source of the panel's border, title-row and padding insets for layouts.
func (k Kit) PanelContentRect(bounds Bounds) (x, y int, content Bounds) {
	content = k.PanelContentBounds(bounds)
	if bounds.Width < 2 || bounds.Height < 3 || content.Width <= 0 || content.Height <= 0 {
		return 0, 0, Bounds{}
	}
	return 1 + k.panelPadding(bounds.Width), 2, content
}

// panelPadding is the horizontal padding Panel draws inside its border.
func (k Kit) panelPadding(width int) int {
	return min(max(0, k.theme.Spacing.PanelX), max(0, width-2)/2)
}

// PanelNode is Panel as a bloom.Layouter: the kit draws the border, title
// and padding at the size the layout gives it and the child is laid out in
// exactly PanelContentRect, so layouts never repeat the panel's insets.
type PanelNode struct {
	kit   Kit
	panel Panel
	child bloom.Child
}

// PanelNode returns p as a layout node containing child. p.Body is ignored.
func (k Kit) PanelNode(p Panel, child bloom.Child) PanelNode {
	p.Body = ""
	return PanelNode{kit: k, panel: p, child: child}
}

// View implements bloom.Child; a panel only draws at a size it is given.
func (n PanelNode) View() string { return "" }

// Measure implements bloom.Measurer: a panel takes the space its slot gives.
func (n PanelNode) Measure(bloom.Size) bloom.Size { return bloom.Size{} }

// Layout implements bloom.Layouter.
func (n PanelNode) Layout(ctx bloom.Context) bloom.Child {
	bounds := Bounds{Width: ctx.Width, Height: ctx.Height}
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return nil
	}
	chrome := bloom.Text(n.kit.Panel(n.panel, bounds))
	x, y, content := n.kit.PanelContentRect(bounds)
	if n.child == nil || content.Width <= 0 || content.Height <= 0 {
		return chrome
	}
	// The chrome's body rows are already painted surface cells, so a
	// transparent layer keeps them wherever the child leaves a cell blank.
	return bloom.Overlay(chrome, bloom.Layer(n.child).
		Anchor(bloom.Start, bloom.Start).Offset(x, y).
		Size(content.Width, content.Height).Transparent())
}

// Colors returns the theme's text and surface colors in the kit's color
// profile, for a bloom root's default colors. Plain yields no color.
func (k Kit) Colors() (fg, bg color.Color) {
	return k.resolve(k.theme.Palette.Text), k.resolve(k.theme.Palette.Surface)
}
