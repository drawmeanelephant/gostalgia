package shell

import (
	"fmt"
	"testing"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// TestHomeLargeCompositionIsBoundedAndCentered checks that at 200x60 Home
// lays its wordmark and sections out as one centered, max-width column that
// stays inside the body.
func TestHomeLargeCompositionIsBoundedAndCentered(t *testing.T) {
	for _, th := range builtinThemes() {
		t.Run(th.Name, func(t *testing.T) {
			m := sized(fixtureModel(th, ui.ANSI256), 200, 60)
			m.setMode(modeHome)
			frame := m.solve()
			body, ok := frame.Rect(slotBody)
			if !ok {
				t.Fatal("Home body is missing")
			}
			comp, ok := frame.Rect(homeCompositionID)
			if !ok {
				t.Fatal("Home composition is missing")
			}
			if comp.Dx() != homeMaxContentWidth {
				t.Fatalf("composition width = %d, want the %d-cell bound", comp.Dx(), homeMaxContentWidth)
			}
			if comp.Dx() >= body.Dx() {
				t.Fatalf("composition %v fills the %v body", comp, body)
			}
			if !comp.In(body) {
				t.Fatalf("composition %v overflows the body %v", comp, body)
			}
			if left, right := comp.Min.X-body.Min.X, body.Max.X-comp.Max.X; abs(left-right) > 1 {
				t.Fatalf("composition %v is not centered in %v: left %d right %d", comp, body, left, right)
			}
			for _, id := range []homeSection{homeWordmarkID, homeSystemID, homeAppsID, homeDocsID} {
				r, ok := frame.Rect(id)
				if !ok {
					t.Fatalf("Home section %d is missing", id)
				}
				if !r.In(comp) || r.Min.X != comp.Min.X || r.Max.X != comp.Max.X {
					t.Fatalf("Home section %d at %v is not in the composition column %v", id, r, comp)
				}
			}
			// The centred column moves the rendered rows away from the
			// body's left edge, so a click must use the solved position.
			if _, ok := findText(m, body, "Echo"); !ok {
				t.Fatal("Echo is not on the Home screen")
			}
			p, _ := findText(m, body, "Echo")
			if p.X <= body.Min.X {
				t.Fatalf("Echo starts at %v without the composition offset from %v", p, body.Min)
			}
		})
	}
}

// TestHomeCompactCompositionKeepsTheBodyWidth pins the existing 30x10 and
// 80x24 layout: only bodies wider than the bound are centered.
func TestHomeCompactCompositionKeepsTheBodyWidth(t *testing.T) {
	for _, size := range [][2]int{{30, 10}, {80, 24}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.ANSI256), size[0], size[1])
			m.setMode(modeHome)
			frame := m.solve()
			body, ok := frame.Rect(slotBody)
			if !ok {
				t.Fatal("Home body is missing")
			}
			comp, ok := frame.Rect(homeCompositionID)
			if !ok {
				t.Fatal("Home composition is missing")
			}
			if comp != body {
				t.Fatalf("composition %v, want the full body %v", comp, body)
			}
			if _, ok := frame.Rect(homeAppsID); !ok {
				t.Fatal("running applications are not visible")
			}
		})
	}
}

// TestHomeCompositionMouseUsesSolvedCoordinates activates a row at the cell
// the centered composition actually renders it on.
func TestHomeCompositionMouseUsesSolvedCoordinates(t *testing.T) {
	m, _ := mouseModel(t, 200, 60)
	m.setMode(modeHome)
	body := rectOf(t, m, slotBody)
	p := mustFind(t, m, body, "Echo")
	comp := rectOf(t, m, homeCompositionID)
	if p.X < comp.Min.X {
		t.Fatalf("Echo at %v is left of the composition %v", p, comp)
	}
	clickAt(t, m, p)
	if !m.onWindows() || m.focusedView().data.Title == "" {
		t.Fatalf("clicking Echo at %v opened no app view", p)
	}
}
