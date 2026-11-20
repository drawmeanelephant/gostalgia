package bloomadapter

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/canvas"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/experimental/windows/fixtures"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
	"gostalgia/sdk"
)

// textFilledBase covers every cell with a visible glyph, so a window that is
// not opaque would be unmistakable.
func textFilledBase(width, height int) bloom.Child {
	line := strings.Repeat("#", width)
	lines := make([]string, height)
	for i := range lines {
		lines[i] = line
	}
	return bloom.Text(strings.Join(lines, "\n"))
}

func openTestWindow(t *testing.T, model *Model, service *windows.Service, window windows.Window, view sdk.View) {
	t.Helper()
	if err := service.Open(window); err != nil {
		t.Fatal(err)
	}
	model.presentations[window.ID] = newPresentationState(view)
	model.activate(window.ID, controlID{})
	model.View()
}

// renderChosen draws base and only the named windows, retaining the service's
// current focus so before/after comparisons differ by depth alone.
func renderChosen(model *Model, base bloom.Child, ids ...windows.ID) string {
	layers := make([]bloom.LayerNode, 0, len(ids))
	for _, id := range model.service.Order() {
		included := false
		for _, want := range ids {
			if id == want {
				included = true
				break
			}
		}
		if !included {
			continue
		}
		window, ok := model.service.Get(id)
		if !ok {
			continue
		}
		layers = append(layers, model.windowLayer(window))
	}
	return model.root.View(model.width, model.height, bloom.Overlay(base, layers...)).Content
}

func styledCells(t *testing.T, content string) *canvas.Canvas {
	t.Helper()
	return canvas.Parse(content, ansi.GraphemeWidth)
}

func cellAt(t *testing.T, cells *canvas.Canvas, x, y int) cellSnapshot {
	t.Helper()
	cell := cells.CellAt(x, y)
	if cell == nil {
		t.Fatalf("no cell at %d,%d", x, y)
	}
	return cellSnapshot{content: cell.Content, fg: fmt.Sprint(cell.Style.Fg), bg: fmt.Sprint(cell.Style.Bg), hasBg: cell.Style.Bg != nil}
}

type cellSnapshot struct {
	content string
	fg      string
	bg      string
	hasBg   bool
}

func TestWindowChromeIsOpaqueOverTextFilledCanvas(t *testing.T) {
	for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256, ui.TrueColor} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			model, service, _, _ := newFixtureModel(t, mode)
			base := textFilledBase(model.width, model.height)
			content := model.compose(base).Content
			if mode == ui.Plain && strings.Contains(content, "\x1b") {
				t.Fatal("plain demo emitted terminal styling")
			}
			cells := styledCells(t, content)
			for _, id := range service.Order() {
				rect, ok := model.LayerRect(id)
				if !ok {
					t.Fatalf("no solved layer rect for %q", id)
				}
				blank := 0
				for y := rect.Min.Y; y < rect.Max.Y; y++ {
					for x := rect.Min.X; x < rect.Max.X; x++ {
						cell := cellAt(t, cells, x, y)
						if cell.content == "#" {
							t.Fatalf("%s cell %d,%d shows canvas glyph %q", id, x, y, cell.content)
						}
						if cell.content == " " {
							blank++
						}
						if mode != ui.Plain && !cell.hasBg {
							t.Fatalf("%s cell %d,%d %q has no explicit background", id, x, y, cell.content)
						}
					}
				}
				if blank == 0 {
					t.Fatalf("%s has no blank interior cells to prove opacity", id)
				}
			}
			plain := strings.Split(content, "\n")
			if !strings.Contains(plain[1], "Screen / form") || !strings.Contains(plain[1], "[X]") {
				t.Fatalf("title row is not readable: %q", plain[1])
			}
		})
	}
}

func TestWindowTitlesTruncateWithinBorderAndKeepCloseControl(t *testing.T) {
	long := "A deliberately long experimental window title that must never displace the close control"
	cases := []struct {
		name  string
		width int
		title string
	}{
		{name: "normal", width: 38, title: long},
		{name: "narrow", width: 30, title: long},
		{name: "very narrow", width: 20, title: long},
		{name: "wide glyphs", width: 20, title: "ｗｉｄｅ　ｔｉｔｌｅ　ｗｉｔｈ　ｉｄｅｏｇｒａｐｈｓ"},
		{name: "hostile controls", width: 16, title: "safe\x1b[31mRED\x1b[0m\u009b2Jtitle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, service, _, _ := newFixtureModel(t, ui.Plain)
			id := windows.ID("titled")
			openTestWindow(t, model, service, windows.Window{
				ID: id, Title: tc.title, Bounds: windows.Bounds{X: 0, Y: 0, Width: tc.width, Height: 8},
			}, fixtures.ScreenData())
			content := model.View().Content
			if strings.ContainsRune(content, '\x1b') {
				t.Fatalf("hostile title injected terminal styling:\n%q", content)
			}
			layer, ok := model.LayerRect(id)
			if !ok {
				t.Fatal("no solved layer rect")
			}
			title, ok := model.Frame().Rect(controlID{window: id, kind: "title"})
			if !ok {
				t.Fatal("no title allocation")
			}
			close, ok := model.Frame().Rect(controlID{window: id, kind: "close"})
			if !ok {
				t.Fatal("complete close control is missing")
			}
			if !close.In(image.Rect(layer.Min.X, layer.Min.Y, layer.Max.X, layer.Max.Y)) {
				t.Fatalf("close control %v outside window %v", close, layer)
			}
			if close.Dx() != len(closeLabel) || close.Max.Y != layer.Min.Y+1 {
				t.Fatalf("close control geometry = %v, want a %d-cell top-border control", close, len(closeLabel))
			}
			if title.Max.X > close.Min.X || title.Min.X < layer.Min.X || title.Max.X > layer.Max.X {
				t.Fatalf("title %v collides with close %v inside %v", title, close, layer)
			}
			if budget := tc.width - topBorderSpan - close.Dx() - 1; title.Dx() > budget || title.Dx() < 1 {
				t.Fatalf("title width %d exceeds allocation %d", title.Dx(), budget)
			}
			cells := styledCells(t, content)
			var rendered strings.Builder
			for x := title.Min.X; x < title.Max.X; x++ {
				if cell := cellAt(t, cells, x, title.Min.Y); cell.content != "" {
					rendered.WriteString(cell.content)
				}
			}
			if !strings.HasSuffix(rendered.String(), "…") {
				t.Fatalf("long title %q rendered as %q without a clean truncation", tc.title, rendered.String())
			}
			for i, want := range closeLabel {
				cell := cellAt(t, cells, close.Min.X+i, close.Min.Y)
				if cell.content != string(want) {
					t.Fatalf("close control cell %d rendered %q, want %q", i, cell.content, string(want))
				}
			}
			if !strings.Contains(content, "One") {
				t.Fatalf("body is not readable:\n%s", content)
			}
		})
	}
}

// TestNarrowWindowsAllocateCloseBeforeTitle locks the demo's narrowest valid
// geometry: the close control is placed from the right edge before the title,
// so all three cells of [X] stay visible, and its focus decoration is shed
// before the control itself would clip.
func TestNarrowWindowsAllocateCloseBeforeTitle(t *testing.T) {
	cases := []struct {
		name         string
		width        int
		closeFocused bool
		closeWidth   int
		titleWidth   int
	}{
		{name: "focused ten", width: 10, closeFocused: true, closeWidth: 5, titleWidth: 0},
		{name: "focused nine", width: 9, closeFocused: true, closeWidth: 5, titleWidth: 0},
		{name: "focused eight sheds decoration", width: 8, closeFocused: true, closeWidth: 3, titleWidth: 0},
		{name: "focused seven sheds decoration", width: 7, closeFocused: true, closeWidth: 3, titleWidth: 0},
		{name: "focused eleven keeps a title remnant", width: 11, closeFocused: true, closeWidth: 5, titleWidth: 1},
		{name: "unfocused ten", width: 10, closeWidth: 3, titleWidth: 2},
		{name: "unfocused nine", width: 9, closeWidth: 3, titleWidth: 1},
		{name: "unfocused seven", width: 7, closeWidth: 3, titleWidth: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, service, _, _ := newFixtureModel(t, ui.ANSI256)
			model.close(padID)
			service.Move(screenID, windows.Bounds{X: 0, Y: 0, Width: tc.width, Height: 8})
			control := controlID{window: screenID, kind: "field", id: "text"}
			if tc.closeFocused {
				control = controlID{window: screenID, kind: "close"}
			}
			model.activate(screenID, control)
			content := model.View().Content
			layer, ok := model.LayerRect(screenID)
			if !ok {
				t.Fatal("no solved layer rect")
			}
			if layer.Dx() != tc.width {
				t.Fatalf("layer width = %d, want %d", layer.Dx(), tc.width)
			}
			closeRect, ok := model.Frame().Rect(controlID{window: screenID, kind: "close"})
			if !ok {
				t.Fatal("close control is missing")
			}
			if closeRect.Dx() != tc.closeWidth || closeRect.Dy() != 1 || closeRect.Min.Y != layer.Min.Y {
				t.Fatalf("close geometry = %v, want a %d-cell top-border control", closeRect, tc.closeWidth)
			}
			if closeRect.Max.X > layer.Max.X-1 {
				t.Fatalf("close control %v overwrites the box corner inside %v", closeRect, layer)
			}
			cells := styledCells(t, content)
			var closeText strings.Builder
			for x := closeRect.Min.X; x < closeRect.Max.X; x++ {
				closeText.WriteString(cellAt(t, cells, x, closeRect.Min.Y).content)
			}
			if !strings.HasSuffix(closeText.String(), closeLabel) {
				t.Fatalf("close control %q is clipped, want %q intact", closeText.String(), closeLabel)
			}
			if tc.closeWidth == len(closeLabel) && closeText.String() != closeLabel {
				t.Fatalf("narrow close kept decoration %q, want bare %q", closeText.String(), closeLabel)
			}
			if tc.closeWidth > len(closeLabel) && !strings.HasPrefix(closeText.String(), model.kit.Theme().Focus.Marker) {
				t.Fatalf("focused close %q lacks the focus decoration", closeText.String())
			}
			titleRect, hasTitle := model.Frame().Rect(controlID{window: screenID, kind: "title"})
			if tc.titleWidth == 0 {
				if hasTitle {
					t.Fatalf("title %v was allocated cells while the close control needs them", titleRect)
				}
			} else {
				if !hasTitle {
					t.Fatal("title is missing despite spare cells")
				}
				if titleRect.Dx() > tc.titleWidth {
					t.Fatalf("title width = %d, want at most %d", titleRect.Dx(), tc.titleWidth)
				}
				if titleRect.Max.X >= closeRect.Min.X {
					t.Fatalf("title %v collides with close %v", titleRect, closeRect)
				}
			}
			if !strings.Contains(content, "On") {
				t.Fatalf("body is not readable at width %d:\n%s", tc.width, content)
			}
		})
	}
}

func TestWindowShadowDimsCanvasAndLeavesControlsReadable(t *testing.T) {
	model, service, _, _ := newFixtureModel(t, ui.ANSI256)
	model.close(padID)
	service.Move(screenID, windows.Bounds{X: 2, Y: 2, Width: 20, Height: 8})
	base := textFilledBase(model.width, model.height)
	bare := model.root.View(model.width, model.height, base).Content
	content := model.compose(base).Content
	cells := styledCells(t, content)
	bareCells := styledCells(t, bare)
	layer, ok := model.LayerRect(screenID)
	if !ok {
		t.Fatal("no solved layer rect")
	}
	shadow := cellAt(t, cells, layer.Max.X, layer.Min.Y+2)
	under := cellAt(t, bareCells, layer.Max.X, layer.Min.Y+2)
	if shadow.content != "#" || under.content != "#" {
		t.Fatalf("shadow sample content = %q/%q, want untouched canvas glyph", shadow.content, under.content)
	}
	if !shadow.hasBg || shadow.bg == under.bg {
		t.Fatalf("static shadow did not visibly tint the canvas: bg %q over %q", shadow.bg, under.bg)
	}
	if again := model.compose(base).Content; again != content {
		t.Fatal("shadow rendering is not static")
	}
	if model.Init() != nil {
		t.Fatal("adapter scheduled idle animation work")
	}
	if _, cmd := model.Update(tea.WindowSizeMsg{Width: model.width, Height: model.height}); cmd != nil {
		t.Fatal("resize scheduled animation work")
	}

	closeRect, ok := model.Frame().Rect(controlID{window: screenID, kind: "close"})
	if !ok {
		t.Fatal("no close control")
	}
	closeCell := cellAt(t, cells, closeRect.Min.X, closeRect.Min.Y)
	if closeCell.content != "[" {
		t.Fatalf("close control cell = %q", closeCell.content)
	}
	bodyCell := cellAt(t, cells, layer.Min.X+3, layer.Min.Y+2)
	if bodyCell.content == " " {
		t.Fatalf("expected body content at %d,%d", layer.Min.X+3, layer.Min.Y+2)
	}
	if closeCell.fg != bodyCell.fg {
		t.Fatalf("shadow dimmed the close control: close fg %s, body fg %s", closeCell.fg, bodyCell.fg)
	}
	if closeCell.bg != bodyCell.bg {
		t.Fatalf("shadow obscured the close control background: close bg %s, body bg %s", closeCell.bg, bodyCell.bg)
	}
}

func TestOverlappingWindowsShowStaticDepth(t *testing.T) {
	model, service, _, _ := newFixtureModel(t, ui.ANSI256)
	service.Move(screenID, windows.Bounds{X: 7, Y: 2, Width: 30, Height: 10})
	service.Move(padID, windows.Bounds{X: 0, Y: 0, Width: 10, Height: 14})
	service.Focus(padID)
	model.View()
	base := textFilledBase(model.width, model.height)
	lowerOnly := renderChosen(model, base, screenID)
	full := renderChosen(model, base, screenID, padID)

	lower, ok := model.LayerRect(screenID)
	if !ok {
		t.Fatal("no solved lower window")
	}
	pad, ok := model.LayerRect(padID)
	if !ok {
		t.Fatal("no solved upper window")
	}
	// Row 2 is the lower window's title; x=10 is under the upper window's
	// right shadow band but outside the upper window itself.
	x, y := pad.Max.X, lower.Min.Y
	if x >= lower.Max.X {
		t.Skip("windows do not overlap at this size")
	}
	fullCell := cellAt(t, styledCells(t, full), x, y)
	lowerCell := cellAt(t, styledCells(t, lowerOnly), x, y)
	if fullCell.content != lowerCell.content || strings.TrimSpace(fullCell.content) == "" {
		t.Fatalf("shadow sample %d,%d changed content %q -> %q", x, y, lowerCell.content, fullCell.content)
	}
	if !fullCell.hasBg || fullCell.bg == lowerCell.bg {
		t.Fatalf("upper shadow did not tint the lower window: bg %q over %q", fullCell.bg, lowerCell.bg)
	}
	if fullCell.fg != lowerCell.fg {
		t.Fatalf("shadow changed the lower title text color: %s -> %s", lowerCell.fg, fullCell.fg)
	}

	closeRect, ok := model.Frame().Rect(controlID{window: padID, kind: "close"})
	if !ok {
		t.Fatal("upper close control missing")
	}
	padded := cellAt(t, styledCells(t, full), pad.Min.X+3, pad.Min.Y+2)
	closeCell := cellAt(t, styledCells(t, full), closeRect.Min.X, closeRect.Min.Y)
	if closeCell.content != "[" {
		t.Fatalf("upper close cell = %q", closeCell.content)
	}
	if closeCell.fg != padded.fg {
		t.Fatalf("upper shadow dimmed its own control: close fg %s, body fg %s", closeCell.fg, padded.fg)
	}
	if closeCell.bg != padded.bg {
		t.Fatalf("upper shadow obscured its own control: close bg %s, body bg %s", closeCell.bg, padded.bg)
	}
}
