package shell

// delve_presentation_test.go covers the generic, toolkit-neutral parts of
// issue #233: a hinted block's bounded primary viewport with its player kept
// visible, meters bounded to forty cells, and the compact inspection route
// that returns to controls with focus and selection intact. It uses an SDK
// snapshot shaped like Delve's, not the app package, because these guarantees
// belong to the shell renderer and its navigation.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

// delveMap builds a 48x22 map whose row markers make cropping observable and
// whose focal cell holds the player glyph.
func delveMap(x, y int) []string {
	lines := make([]string, 22)
	for row := range lines {
		lines[row] = fmt.Sprintf("Y%02d", row) + strings.Repeat(".", 45)
	}
	runes := []rune(lines[y])
	runes[x] = '@'
	lines[y] = string(runes)
	return lines
}

// delveData returns a version-3 snapshot with the map's optional focal point,
// vitals meters, a movement pad, pack items, and a log region.
func delveData(x, y int) sdk.View {
	return sdk.View{
		Version: 3, Instance: "launch", Title: "Delve — Beneath the Brass Elephant", State: sdk.ViewReady,
		Items: []sdk.Item{
			{ID: "item_1", Label: "a) dagger", Detail: "weapon · equipped"},
			{ID: "item_2", Label: "b) potion of healing", Detail: "potion"},
		},
		Actions: []sdk.Action{
			{ID: "pickup", Label: "Pick up"}, {ID: "descend", Label: "Descend", Disabled: true},
			{ID: "ascend", Label: "Ascend", Disabled: true}, {ID: "rest", Label: "Rest"},
			{ID: "use", Label: "Use"}, {ID: "equip", Label: "Equip"},
			{ID: "drop", Label: "Drop"}, {ID: "new", Label: "New delve"},
			{ID: "help", Label: "Help"},
		},
		Blocks: []sdk.Block{{
			ID: "map", Text: strings.Join(delveMap(x, y), "\n"),
			Focus: &sdk.BlockFocus{X: x, Y: y},
		}},
		Meters: []sdk.Meter{
			{ID: "hp", Label: "HP 10/12", Value: 10.0 / 12.0},
			{ID: "satiety", Label: "Satiety 800", Value: 0.8},
			{ID: "xp", Label: "Lv 1 · XP 7/20", Value: 7.0 / 20.0},
		},
		Grid: &sdk.Grid{Label: "Move", Columns: 3, Cells: []sdk.Cell{
			{ID: "nw", Label: "NW", Action: "move"}, {ID: "n", Label: "N", Action: "move"},
			{ID: "ne", Label: "NE", Action: "move"}, {ID: "w", Label: "W", Action: "move"},
			{ID: "wait", Label: "·", Action: "move"}, {ID: "e", Label: "E", Action: "move"},
			{ID: "sw", Label: "SW", Action: "move"}, {ID: "s", Label: "S", Action: "move"},
			{ID: "se", Label: "SE", Action: "move"},
		}},
		Region: &sdk.Region{ID: "log", Label: "Log", Text: "You slip into the Underhall.\nIt is dark down here."},
		Status: "Underhall — level 1 · HP 10/12 · 0 gold",
	}
}

func delveModel(t *testing.T, width, height int, view sdk.View) *Model {
	t.Helper()
	m := New(context.Background(), noopCaller{}, nil)
	m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	m.showWindow(&appView{data: view, instance: view.Instance, version: view.Version, values: map[string]string{}})
	return m
}

func plainView(t *testing.T, m *Model) string {
	t.Helper()
	m.View()
	return ansi.Strip(m.View().Content)
}

// TestFocalBlockViewportKeepsPlayerVisible checks the bounded primary
// viewport: most of an 80x24 body shows the map even while an action owns
// keyboard focus, all 22 rows return at 200x60, and moving the focal point to
// every edge keeps the player glyph inside the crop.
func TestFocalBlockViewportKeepsPlayerVisible(t *testing.T) {
	m := delveModel(t, 80, 24, delveData(24, 10))
	view := plainView(t, m)
	if !strings.Contains(view, "@") || !strings.Contains(view, "Y10") {
		t.Fatalf("80x24 map viewport lost the player's row:\n%s", view)
	}
	if strings.Contains(view, "Y00") {
		t.Fatalf("80x24 map viewport did not crop to a bounded window:\n%s", view)
	}
	// Meters are pinned beside the map, so the vitals survive action focus.
	if !strings.Contains(view, "HP 10/12") || !strings.Contains(view, "[Pick up]") {
		t.Fatalf("split view hid vitals or the focused action:\n%s", view)
	}

	// A tall view shows the whole map, still without cropping data.
	m = delveModel(t, 200, 60, delveData(24, 10))
	view = plainView(t, m)
	for _, row := range []string{"Y00", "Y10", "Y21", "@"} {
		if !strings.Contains(view, row) {
			t.Fatalf("200x60 view lost map row %q:\n%s", row, view)
		}
	}

	// Every edge keeps the focal glyph inside the window, including the
	// narrow terminal where the map crops horizontally as well as vertically.
	for _, focus := range []sdk.BlockFocus{{X: 0, Y: 0}, {X: 47, Y: 0}, {X: 0, Y: 21}, {X: 47, Y: 21}, {X: 24, Y: 10}} {
		m = delveModel(t, 30, 10, delveData(focus.X, focus.Y))
		m.focusedView().inspect = true
		m.focusedView().inspectTop = -1
		view = plainView(t, m)
		if !strings.Contains(view, "@") {
			t.Fatalf("compact crop lost the player at %+v:\n%s", focus, view)
		}
	}
}

// TestFocalBlockInspectionRetainsSelection covers the compact route at
// exactly 30x10: the default body keeps controls, "m" exposes map context,
// arrows scroll it, and returning leaves focus, cell, and item untouched.
func TestFocalBlockInspectionRetainsSelection(t *testing.T) {
	m := delveModel(t, 30, 10, delveData(24, 10))
	v := m.focusedView()
	v.focus, v.item, v.cell = 4, 1, 5
	before := *v

	view := plainView(t, m)
	if strings.Contains(view, "Y10") {
		t.Fatalf("30x10 controls body unexpectedly shows the map:\n%s", view)
	}
	if !strings.Contains(view, "m MAP") && !strings.Contains(view, "m map") {
		t.Fatalf("compact view does not advertise the inspection route:\n%s", view)
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "m"})
	if !v.inspect {
		t.Fatal("m did not enter map inspection")
	}
	view = plainView(t, m)
	if !strings.Contains(view, "@") {
		t.Fatalf("inspection did not expose the map:\n%s", view)
	}
	// The compact window shows vertical context rather than a single cell:
	// two map rows remain beside the one-line inspection hint.
	if v.blockWindow != 2 {
		t.Fatalf("compact inspection window = %d rows, want 2:\n%s", v.blockWindow, view)
	}
	if v.focus != before.focus || v.item != before.item || v.cell != before.cell {
		t.Fatalf("inspection changed selection: focus=%d item=%d cell=%d", v.focus, v.item, v.cell)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if v.inspectTop <= 0 {
		t.Fatalf("down did not scroll the map window: inspectTop=%d", v.inspectTop)
	}
	if !strings.Contains(plainView(t, m), "@") {
		t.Fatal("scrolled inspection lost the player")
	}

	m.Update(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "m"})
	if v.inspect {
		t.Fatal("m did not return to controls")
	}
	if v.focus != before.focus || v.item != before.item || v.cell != before.cell ||
		v.values == nil || len(v.values) != 0 {
		t.Fatalf("returning from inspection lost state: focus=%d item=%d cell=%d values=%v",
			v.focus, v.item, v.cell, v.values)
	}
}

// TestActionRevealedPanelStaysReadable covers the Help panel's rendering at
// both a split body and the 30x10 compact body: an action that swaps the
// output surface shows that panel from its first line, focus stays on the
// action that toggles it, and a normal snapshot change returns to controls.
func TestActionRevealedPanelStaysReadable(t *testing.T) {
	help := sdk.Region{ID: "help", Label: "Help",
		Text: "Controls\n\n  move pad — step in any of eight directions\n  help — toggle this panel"}

	for _, size := range [][2]int{{30, 10}, {80, 24}} {
		m := delveModel(t, size[0], size[1], delveData(24, 10))
		m.focusedView().apply(delveData(24, 10), false)
		v := m.focusedView()
		v.focus = len(v.data.Actions) - 1
		before := v.focus

		panel := delveData(24, 10)
		panel.Region = &help
		m.focusedView().apply(panel, true)
		view := plainView(t, m)
		if !strings.Contains(view, "Controls") {
			t.Fatalf("%dx%d: action-revealed Help panel is not readable:\n%s", size[0], size[1], view)
		}
		if size[0] >= 80 && !strings.Contains(view, "toggle this panel") {
			t.Fatalf("%dx%d: Help panel lost its body text:\n%s", size[0], size[1], view)
		}
		if v.surface != "help" || v.focus != before {
			t.Fatalf("%dx%d: surface=%q focus=%d, want help and focus %d",
				size[0], size[1], v.surface, v.focus, before)
		}

		// Navigation leaves the panel and restores the control window.
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		if v.surface != "" {
			t.Fatalf("%dx%d: Tab did not leave the revealed surface", size[0], size[1])
		}
		if view := plainView(t, m); strings.Contains(view, "Controls") || !strings.Contains(view, "[") {
			t.Fatalf("%dx%d: controls did not return:\n%s", size[0], size[1], view)
		}
	}

	// A poll while the panel is on screen must not promote it to the
	// ordinary surface: toggling Help shut still restores its base log.
	door := delveData(24, 10)
	m := delveModel(t, 80, 24, door)
	m.focusedView().apply(door, false)
	m.focusedView().focus = len(m.focusedView().data.Actions) - 1
	doorPanel := door
	doorPanel.Region = &help
	m.focusedView().apply(doorPanel, true)
	m.focusedView().apply(doorPanel, false)
	m.focusedView().apply(door, true)
	if view := plainView(t, m); m.focusedView().surface != "" || strings.Contains(view, "Controls") {
		t.Fatalf("door panel did not clear to its log: surface=%q\n%s", m.focusedView().surface, view)
	}

	// The same sequence during an ongoing run: the ordinary snapshot returns
	// to its base surface instead of surfacing the log over the controls.
	m = delveModel(t, 80, 24, delveData(24, 10))
	m.focusedView().apply(delveData(24, 10), false)
	m.focusedView().focus = len(m.focusedView().data.Actions) - 1
	panel := delveData(24, 10)
	panel.Region = &help
	m.focusedView().apply(panel, true)
	m.focusedView().apply(delveData(24, 10), true)
	if m.focusedView().surface != "" {
		t.Fatalf("returning to the base surface left %q displayed", m.focusedView().surface)
	}
	if view := plainView(t, m); !strings.Contains(view, "[Help]") {
		t.Fatalf("returning from Help did not restore controls:\n%s", view)
	}
}

// TestCompactFocalViewKeepsActionsReachable drives the real key route at
// 30x10: Tab reaches the movement pad and Help, and Enter dispatches their
// semantic identities while the map window is only one keystroke away.
func TestCompactFocalViewKeepsActionsReachable(t *testing.T) {
	var requests []sdk.ActionRequest
	m := New(context.Background(), callerFunc(func(_ context.Context, method string, params, out any) error {
		if strings.HasSuffix(method, "/action") {
			requests = append(requests, params.(sdk.ActionRequest))
		}
		return writeJSON(out, delveData(24, 10))
	}), nil)
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	m.showWindow(&appView{data: delveData(24, 10), instance: "launch", version: 3, values: map[string]string{}})
	v := m.focusedView()

	// The movement pad sits after the nine actions.
	for range len(v.data.Actions) {
		m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if !v.gridFocused() {
		t.Fatalf("tab did not reach the movement pad at 30x10: focus=%d", v.focus)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("movement pad cell was not reachable at 30x10")
	}
	m.Update(cmd())
	if len(requests) != 1 || requests[0].Action != "move" || requests[0].CellID != "n" {
		t.Fatalf("compact movement dispatch = %+v", requests)
	}

	// Help is the last action; its identity survives the compact flow.
	v.focus = len(v.data.Actions) - 1
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Help was not reachable at 30x10")
	}
	m.Update(cmd())
	if len(requests) != 2 || requests[1].Action != "help" {
		t.Fatalf("compact Help dispatch = %+v", requests)
	}
}

// TestMetersStayWithinFortyCells checks every rendered meter at any approved
// size: the line fits the body's available width and its bracketed bar never
// exceeds forty display cells. A 30x10 body leaving one content row still
// draws a bounded meter whenever the flow shows it, so the same bound is
// asserted directly through the row renderer there.
func TestMetersStayWithinFortyCells(t *testing.T) {
	for _, size := range [][2]int{{30, 10}, {80, 24}, {200, 60}} {
		m := delveModel(t, size[0], size[1], delveData(24, 10))
		w, h := m.windowContentSize(m.focusedView())
		if w <= 0 || h <= 0 {
			t.Fatalf("%dx%d: no window content area", size[0], size[1])
		}
		for _, meter := range m.focusedView().data.Meters {
			if width := ansi.StringWidth(m.meterRow(meter, w)); width > min(40, w) {
				t.Fatalf("%dx%d: meter %s row = %d cells, window %d",
					size[0], size[1], meter.ID, width, w)
			}
		}
		lines, _ := m.viewLayout(m.focusedView(), w, h)
		bars := 0
		for _, line := range lines {
			plain := ansi.Strip(line)
			open, close := strings.Index(plain, "["), strings.LastIndex(plain, "]")
			if open < 0 || close < open || !strings.Contains(plain[close:], "%") {
				continue
			}
			bars++
			if width := ansi.StringWidth(plain[open+1 : close]); width > 40 {
				t.Fatalf("%dx%d: meter bar = %d cells: %q", size[0], size[1], width, plain)
			}
			if width := ansi.StringWidth(plain); width > min(40, w) {
				t.Fatalf("%dx%d: meter line = %d cells, window %d: %q",
					size[0], size[1], width, w, plain)
			}
		}
		// The split layouts show pinned vitals; 30x10 spends its single
		// content row on the focused control, which the compact route keeps
		// usable and the inspection route leaves reachable.
		if size[0] >= 80 && bars == 0 {
			t.Fatalf("%dx%d: no meter rendered", size[0], size[1])
		}
	}
}

// TestUnhintedBlocksKeepLegacySemantics proves the renderer never requires
// the optional datum: an ordinary art block renders every line in the flow,
// exactly as before the focal-point addition.
func TestUnhintedBlocksKeepLegacySemantics(t *testing.T) {
	m := delveModel(t, 80, 24, padData())
	view := plainView(t, m)
	for _, line := range []string{"Mascot", `(\_/)`, "(o.o)", "> ^ <"} {
		if !strings.Contains(view, line) {
			t.Fatalf("unhinted block lost %q:\n%s", line, view)
		}
	}
}

// TestValidatedHintedFocusSurvivesSanitizer checks the rendered endpoint of
// the SDK boundary: a validated hinted block may contain wide and combining
// text, and sanitization leaves its code-point columns intact, so a cropped
// viewport keeps the glyph the focus named.
func TestValidatedHintedFocusSurvivesSanitizer(t *testing.T) {
	line := strings.Repeat("界", 30) + strings.Repeat("e\u0301", 10) + "@" + strings.Repeat("x", 60)
	data := sdk.View{
		Version: 3, Instance: "launch", Title: "Map", State: sdk.ViewReady,
		Blocks: []sdk.Block{{
			ID: "map", Text: strings.Join([]string{"Y0", line, "Y2", "Y3"}, "\n"),
			Focus: &sdk.BlockFocus{X: 50, Y: 1},
		}},
		Status: "ok",
	}
	if err := data.Validate(); err != nil {
		t.Fatalf("control-free hinted block rejected: %v", err)
	}
	m := delveModel(t, 30, 10, data)
	m.focusedView().inspect = true
	m.focusedView().inspectTop = -1
	view := plainView(t, m)
	if !strings.Contains(view, "e\u0301@") {
		t.Fatalf("crop lost the focal glyph after a combining cluster:\n%s", view)
	}
	if !strings.Contains(view, "界") || strings.ContainsRune(view, '\uFFFD') {
		t.Fatalf("crop split a wide or combining cluster:\n%s", view)
	}
}

// TestUnhintedBlockKeepsSanitizerSemantics proves the boundary change does
// not leak into legacy art: a block without the optional hint still expands
// tabs and strips ANSI sequences and controls exactly as before.
func TestUnhintedBlockKeepsSanitizerSemantics(t *testing.T) {
	data := sdk.View{
		Version: 3, Instance: "launch", Title: "Legacy art", State: sdk.ViewReady,
		Blocks: []sdk.Block{{ID: "art", Label: "Art", Text: "a\tb\n\x00\x1b[31mred\x1b[0m"}},
		Status: "ok",
	}
	if err := data.Validate(); err != nil {
		t.Fatalf("unhinted legacy block rejected: %v", err)
	}
	m := delveModel(t, 80, 24, data)
	view := plainView(t, m)
	if !strings.Contains(view, "a       b") {
		t.Fatalf("unhinted block did not expand the tab to its 8-cell stop:\n%s", view)
	}
	if !strings.Contains(view, "red") || strings.ContainsRune(view, '\x00') {
		t.Fatalf("unhinted block did not sanitize controls:\n%q", view)
	}
	if strings.Contains(m.View().Content, "\x1b[31m") {
		t.Fatalf("unhinted block leaked app ANSI into the frame: %q", m.View().Content)
	}
}

// TestDelveSemanticPresentationAcrossThemesAndPlain covers the finite theme
// population plus Plain: each renders the map view, vitals, and reachable
// actions with readable text, and no theme invents ANSI in Plain mode.
func TestDelveSemanticPresentationAcrossThemesAndPlain(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.ANSI256, ui.TrueColor} {
			m := NewWithTheme(context.Background(), noopCaller{}, nil, th, mode)
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			data := delveData(24, 10)
			m.showWindow(&appView{data: data, instance: "launch", version: 3, values: map[string]string{}})
			// Focus the Help action: the secondary window then shows the
			// reachable control without hiding the primary map.
			m.focusedView().focus = len(data.Actions) - 1
			view := ansi.Strip(m.View().Content)
			for _, want := range []string{"Delve", "@", "HP", "[Help]"} {
				if !strings.Contains(view, want) {
					t.Fatalf("%s/%v lost %q:\n%s", th.Name, mode, want, view)
				}
			}
			// The movement pad is its own reachable stop.
			m.focusedView().focus = len(data.Fields) + len(data.Actions)
			if view := ansi.Strip(m.View().Content); !strings.Contains(view, "[N]") {
				t.Fatalf("%s/%v lost the movement pad:\n%s", th.Name, mode, view)
			}
		}
	}
	m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Midnight(), ui.Plain)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	data := delveData(24, 10)
	m.showWindow(&appView{data: data, instance: "launch", version: 3, values: map[string]string{}})
	m.focusedView().focus = len(data.Actions) - 1
	content := m.View().Content
	if strings.Contains(content, "\x1b") {
		t.Fatal("Plain Delve view emitted ANSI")
	}
	for _, want := range []string{"Delve", "@", "[Help]"} {
		if !strings.Contains(content, want) {
			t.Fatalf("Plain Delve view lost %q:\n%s", want, content)
		}
	}
}
