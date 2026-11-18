package ui

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

func insetThemes() []theme.Theme {
	wide := theme.Nostalgia()
	wide.Name, wide.Spacing.PanelX, wide.Borders.Panel = "wide-pad", 3, lipgloss.DoubleBorder()
	flush := theme.Monochrome()
	flush.Name, flush.Spacing.PanelX = "flush", 0
	return append(builtinThemes(), wide, flush)
}

// A PanelNode draws exactly what Panel draws for the same body, and lays
// its child out in PanelContentRect, so layouts inherit the kit's insets.
func TestPanelNodeMatchesPanel(t *testing.T) {
	for _, th := range insetThemes() {
		for _, size := range [][2]int{{80, 24}, {30, 9}, {12, 5}, {7, 3}, {2, 3}, {1, 1}} {
			t.Run(fmt.Sprintf("%s/%dx%d", th.Name, size[0], size[1]), func(t *testing.T) {
				k := New(th, ANSI256)
				bounds := Bounds{Width: size[0], Height: size[1]}
				x, y, content := k.PanelContentRect(bounds)
				if content != k.PanelContentBounds(bounds) && content != (Bounds{}) {
					t.Fatalf("content %+v, PanelContentBounds %+v", content, k.PanelContentBounds(bounds))
				}
				body := strings.TrimSuffix(strings.Repeat(strings.Repeat("x", content.Width)+"\n", content.Height), "\n")
				leaf := k.Leaf(func(b Bounds) string {
					if b != content {
						t.Errorf("leaf laid out in %+v, want %+v", b, content)
					}
					return body
				})
				node := k.PanelNode(Panel{Title: "TITLE"}, bloom.Fill(leaf).ID("body"))
				f := bloom.Solve(size[0], size[1], node)
				r, ok := f.Rect("body")
				want := image.Rect(x, y, x+content.Width, y+content.Height)
				if content.Width > 0 && content.Height > 0 && (!ok || r != want) {
					t.Fatalf("child at %v (%v), want %v", r, ok, want)
				}
				got := ansi.Strip(bloom.Render(size[0], size[1], node))
				panel := ansi.Strip(k.Panel(Panel{Title: "TITLE", Body: body}, bounds))
				if got != panel {
					t.Fatalf("PanelNode drew\n%s\nPanel draws\n%s", got, panel)
				}
			})
		}
	}
}

func TestLeafFitsItsSlot(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	var seen []Bounds
	leaf := k.Leaf(func(b Bounds) string {
		seen = append(seen, b)
		return "a much longer line than the slot\nsecond\nthird"
	}).Natural(4, 2)
	out := bloom.Render(10, 5, bloom.Column(bloom.Auto(leaf).ID(1), bloom.Fill(nil)))
	f := bloom.Solve(10, 5, bloom.Column(bloom.Auto(leaf).ID(1), bloom.Fill(nil)))
	if r, _ := f.Rect(1); r != image.Rect(0, 0, 10, 2) {
		t.Fatalf("Auto slot = %v, want the natural two rows at full width", r)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "a much lon" || lines[1] != "second    " || strings.TrimSpace(lines[2]) != "" {
		t.Fatalf("leaf not fitted to its slot: %q", lines)
	}
	if len(seen) == 0 || seen[len(seen)-1] != (Bounds{Width: 10, Height: 2}) {
		t.Fatalf("leaf rendered for %v", seen)
	}
	if got := Sized(func(Bounds) string { return "x" }).View(); got != "" {
		t.Fatalf("a leaf outside a layout drew %q", got)
	}
}

func TestBarLeavesUseTheirSlotWidth(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	tabs := []Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Prompt"}}
	bindings := []Binding{{Key: "F1", Help: "HOME"}, {Key: "Ctrl-C", Help: "EXIT"}}
	for _, w := range []int{80, 30, 9} {
		out := bloom.Render(w, 2, bloom.Column(bloom.Fixed(1, k.TabsLeaf(tabs, 2)), bloom.Fixed(1, k.HelpBarLeaf(bindings))))
		lines := strings.Split(out, "\n")
		if want := ansi.Strip(Fit(k.Tabs(tabs, 2, w), Bounds{Width: w, Height: 1})); lines[0] != want {
			t.Fatalf("%d: tabs %q, want %q", w, lines[0], want)
		}
		if want := ansi.Strip(Fit(k.HelpBar(bindings, w), Bounds{Width: w, Height: 1})); lines[1] != want {
			t.Fatalf("%d: help %q, want %q", w, lines[1], want)
		}
	}
}
