package bloomadapter

import (
	"image"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/events"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/experimental/windows/fixtures"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

const (
	screenID windows.ID = "screen"
	padID    windows.ID = "pad"
)

func newFixtureModel(t *testing.T, mode ui.ColorMode) (*Model, *windows.Service, *events.Bus, *[]events.Envelope) {
	t.Helper()
	bus := events.NewBus()
	envelopes := new([]events.Envelope)
	cancel := bus.Subscribe("*", func(envelope events.Envelope) {
		*envelopes = append(*envelopes, envelope)
	})
	t.Cleanup(cancel)
	service := windows.NewService(bus, "test/bloom-adapter")
	if err := service.Open(windows.Window{
		ID: screenID, Title: "Screen / form", Bounds: windows.Bounds{X: 1, Y: 1, Width: 38, Height: 20}, Presentation: fixtures.ScreenData(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Open(windows.Window{
		ID: padID, Title: "Pad / blocks and grid", Bounds: windows.Bounds{X: 41, Y: 1, Width: 38, Height: 20}, Presentation: fixtures.PadData(),
	}); err != nil {
		t.Fatal(err)
	}
	model := New(service, ui.New(theme.Nostalgia(), mode))
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model.View()
	return model, service, bus, envelopes
}

func update(model *Model, msg tea.Msg) {
	model.Update(msg)
	model.View()
}

func mouse(model *Model, msg tea.MouseMsg) {
	view := model.View()
	if view.OnMouse != nil {
		_ = view.OnMouse(msg)
	}
	update(model, msg)
}

func click(model *Model, point image.Point) {
	mouse(model, tea.MouseClickMsg{X: point.X, Y: point.Y, Button: tea.MouseLeft})
	mouse(model, tea.MouseReleaseMsg{X: point.X, Y: point.Y, Button: tea.MouseLeft})
}

func focus(model *Model, control controlID) {
	model.activate(control.window, control)
	model.View()
}

func TestTwoPresentationWindowsDragFocusStackAndBorderClose(t *testing.T) {
	model, service, _, _ := newFixtureModel(t, ui.Plain)
	content := model.View().Content
	for _, want := range []string{"Screen / form", "Pad / blocks and grid", "One", "Mascot", "[X]"} {
		if !strings.Contains(content, want) {
			t.Fatalf("initial frame lacks %q:\n%s", want, content)
		}
	}
	if strings.Count(content, "[X]") != 2 {
		t.Fatalf("initial frame has %d border close controls, want 2", strings.Count(content, "[X]"))
	}
	screenBefore, _ := service.Get(screenID)
	padBefore, _ := service.Get(padID)
	title, ok := model.Frame().Rect(controlID{window: screenID, kind: "title"})
	if !ok {
		t.Fatal("no title drag target")
	}
	start := title.Min.Add(image.Pt(1, 0))
	end := start.Add(image.Pt(5, 2))
	mouse(model, tea.MouseClickMsg{X: start.X, Y: start.Y, Button: tea.MouseLeft})
	mouse(model, tea.MouseMotionMsg{X: end.X, Y: end.Y, Button: tea.MouseLeft})
	mouse(model, tea.MouseReleaseMsg{X: end.X, Y: end.Y, Button: tea.MouseLeft})
	screenAfter, _ := service.Get(screenID)
	padAfter, _ := service.Get(padID)
	wantBounds := screenBefore.Bounds
	wantBounds.X += 5
	wantBounds.Y += 2
	if screenAfter.Bounds != wantBounds {
		t.Fatalf("dragged bounds = %+v, want %+v", screenAfter.Bounds, wantBounds)
	}
	if padAfter.Bounds != padBefore.Bounds {
		t.Fatalf("other window moved: before %+v after %+v", padBefore.Bounds, padAfter.Bounds)
	}
	if rect, ok := model.LayerRect(screenID); !ok || rect.Min != image.Pt(wantBounds.X, wantBounds.Y) || rect.Dx() != wantBounds.Width || rect.Dy() != wantBounds.Height {
		t.Fatalf("solved dragged layer = %v, %v; bounds %+v", rect, ok, wantBounds)
	}

	// Overlap the windows with Screen on top, then click Pad's exposed body.
	overlapped := screenAfter.Bounds
	overlapped.X += 4
	service.Move(padID, overlapped)
	service.Focus(screenID)
	model.View()
	click(model, image.Pt(overlapped.X+overlapped.Width-2, overlapped.Y+4))
	if focused, _ := service.Focused(); focused != padID {
		t.Fatalf("clicked window focus = %q, want pad", focused)
	}
	order := service.Order()
	if len(order) != 2 || order[1] != padID {
		t.Fatalf("z-order after focus = %v, want pad on top", order)
	}
	closeRect, ok := model.Frame().Rect(controlID{window: padID, kind: "close"})
	if !ok {
		t.Fatal("no Edge close target")
	}
	click(model, image.Pt(closeRect.Min.X+1, closeRect.Min.Y))
	if _, ok := service.Get(padID); ok {
		t.Fatal("border close left pad open")
	}
	if _, ok := service.Get(screenID); !ok {
		t.Fatal("border close removed the other window")
	}
	if content := model.View().Content; !strings.Contains(content, "Screen / form") || strings.Contains(content, "Pad / blocks") {
		t.Fatalf("surviving frame is wrong:\n%s", content)
	}
}

func TestFocusScopesKeepDraftsAndSemanticActionsIndependent(t *testing.T) {
	model, service, _, _ := newFixtureModel(t, ui.Plain)
	padField := controlID{window: padID, kind: "field", id: "text"}
	screenField := controlID{window: screenID, kind: "field", id: "text"}
	focus(model, padField)
	update(model, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "pad"})
	update(model, tea.KeyPressMsg{Code: tea.KeyF6})
	focus(model, screenField)
	update(model, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "screen"})
	update(model, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := model.presentations[padID].values["text"]; got != "pad" {
		t.Fatalf("pad draft changed through screen scope: %q", got)
	}
	if got := model.presentations[screenID].values["text"]; got != "screen" {
		t.Fatalf("screen draft = %q", got)
	}
	for range 4 {
		update(model, tea.KeyPressMsg{Code: tea.KeyTab})
		control, ok := model.focus.Current()
		if !ok || control.window != screenID || !model.focus.Within(model.Frame(), scopeID{window: screenID}) {
			t.Fatalf("Tab escaped screen scope: %+v, %v", control, ok)
		}
	}
	screenAction := controlID{window: screenID, kind: "action", id: "send"}
	focus(model, screenAction)
	update(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	actions := model.Actions(screenID)
	if len(actions) != 1 {
		t.Fatalf("screen emitted %d actions, want 1", len(actions))
	}
	action := actions[0]
	if action.Action != "send" || action.Values["text"] != "screen" || action.ItemID != "two" || action.Instance != "launch" || action.Version != 1 || action.RequestID == "" {
		t.Fatalf("screen semantic request = %+v", action)
	}
	if len(model.Actions(padID)) != 0 {
		t.Fatal("screen action leaked into pad")
	}

	update(model, tea.KeyPressMsg{Code: tea.KeyF6})
	if focused, _ := service.Focused(); focused != padID {
		t.Fatalf("F6 focus = %q", focused)
	}
	if got := model.presentations[padID].values["text"]; got != "pad" {
		t.Fatalf("pad draft was not retained: %q", got)
	}
	cell := controlID{window: padID, kind: "cell", id: "nw"}
	focus(model, cell)
	update(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	padActions := model.Actions(padID)
	if len(padActions) != 1 || padActions[0].Action != "press" || padActions[0].CellID != "nw" || padActions[0].Values["text"] != "pad" || padActions[0].Version != 2 {
		t.Fatalf("pad semantic requests = %+v", padActions)
	}
	focus(model, controlID{window: padID, kind: "cell", id: "ne"})
	focus(model, controlID{window: padID, kind: "action", id: "adopt"})
	update(model, tea.KeyPressMsg{Code: tea.KeyEnter})
	padActions = model.Actions(padID)
	if len(padActions) != 2 || padActions[1].Action != "adopt" || padActions[1].CellID != "ne" || padActions[1].Values["text"] != "pad" {
		t.Fatalf("top-level action lost selected grid cell: %+v", padActions)
	}
	model.activateCell(padID, sdk.Cell{ID: "se", Label: "SE", Action: "press", Disabled: true})
	if len(model.Actions(padID)) != 2 {
		t.Fatal("disabled cell emitted an action")
	}
}

func TestKeyboardMoveUsesSameBoundsTransition(t *testing.T) {
	model, service, _, envelopes := newFixtureModel(t, ui.Plain)
	model.activate(screenID, controlID{})
	before, _ := service.Get(screenID)
	otherBefore, _ := service.Get(padID)
	*envelopes = nil
	update(model, tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt})
	after, _ := service.Get(screenID)
	otherAfter, _ := service.Get(padID)
	want := before.Bounds
	want.X++
	if after.Bounds != want {
		t.Fatalf("Alt-Right bounds = %+v, want %+v", after.Bounds, want)
	}
	if otherAfter.Bounds != otherBefore.Bounds {
		t.Fatalf("Alt-Right moved other window: %+v", otherAfter.Bounds)
	}
	if len(*envelopes) != 1 {
		t.Fatalf("Alt-Right published %d events, want one move", len(*envelopes))
	}
	moved, ok := (*envelopes)[0].Payload.(windows.Moved)
	if !ok || moved.ID != screenID || moved.From != before.Bounds || moved.To != want {
		t.Fatalf("Alt-Right event = %#v", (*envelopes)[0].Payload)
	}
}

func TestGridArrowNavigationTracksTabFocusAndDisabledCells(t *testing.T) {
	model, _, _, _ := newFixtureModel(t, ui.Plain)
	focus(model, controlID{window: padID, kind: "cell", id: "nw"})
	update(model, tea.KeyPressMsg{Code: tea.KeyTab})
	if control, _ := model.focus.Current(); control.id != "ne" || model.presentations[padID].cell != 1 {
		t.Fatalf("Tab did not synchronize NE: control %+v, index %d", control, model.presentations[padID].cell)
	}
	update(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if control, _ := model.focus.Current(); control.id != "sw" || model.presentations[padID].cell != 2 {
		t.Fatalf("Right from NE did not select SW: control %+v, index %d", control, model.presentations[padID].cell)
	}
	update(model, tea.KeyPressMsg{Code: tea.KeyRight})
	if control, _ := model.focus.Current(); control.id != "sw" || model.presentations[padID].cell != 2 {
		t.Fatalf("disabled SE desynchronized selection: control %+v, index %d", control, model.presentations[padID].cell)
	}
	update(model, tea.KeyPressMsg{Code: tea.KeyLeft})
	if control, _ := model.focus.Current(); control.id != "ne" || model.presentations[padID].cell != 1 {
		t.Fatalf("Left after disabled target did not return to NE: control %+v, index %d", control, model.presentations[padID].cell)
	}
}

func TestKeyboardCloseLeavesEitherSurvivorUsable(t *testing.T) {
	for _, closeID := range []windows.ID{screenID, padID} {
		t.Run(string(closeID), func(t *testing.T) {
			model, service, _, _ := newFixtureModel(t, ui.Plain)
			model.activate(closeID, controlID{})
			update(model, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
			if _, ok := service.Get(closeID); ok {
				t.Fatalf("Ctrl-W did not close %s", closeID)
			}
			content := model.View().Content
			closedTitle := "Screen / form"
			if closeID == padID {
				closedTitle = "Pad / blocks and grid"
			}
			closedContent := []string{closedTitle}
			if closeID == screenID {
				closedContent = append(closedContent, "Two — second", "[Send]")
			} else {
				closedContent = append(closedContent, "Mascot", "[Adopt]")
			}
			for _, stale := range closedContent {
				if strings.Contains(content, stale) {
					t.Fatalf("closed window left %q painted behind:\n%s", stale, content)
				}
			}
			survivor := screenID
			field := controlID{window: survivor, kind: "field", id: "text"}
			if closeID == screenID {
				survivor = padID
				field = controlID{window: survivor, kind: "field", id: "text"}
			}
			focus(model, field)
			update(model, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "alive"})
			if got := model.presentations[survivor].values["text"]; got != "alive" {
				t.Fatalf("survivor %s did not accept input: %q", survivor, got)
			}
		})
	}
}

func TestLifecycleEventsAndRenderingIsInert(t *testing.T) {
	model, _, _, envelopes := newFixtureModel(t, ui.Plain)
	if len(*envelopes) != 4 {
		t.Fatalf("open envelopes = %d, want 4", len(*envelopes))
	}
	if _, ok := (*envelopes)[0].Payload.(windows.Opened); !ok {
		t.Fatalf("open payload type = %T", (*envelopes)[0].Payload)
	}
	if (*envelopes)[0].Source != "test/bloom-adapter" {
		t.Fatalf("event source = %q", (*envelopes)[0].Source)
	}
	*envelopes = nil
	for range 3 {
		model.View()
	}
	if len(*envelopes) != 0 {
		t.Fatalf("rendering published lifecycle events: %#v", *envelopes)
	}
	model.activate(screenID, controlID{})
	model.close(screenID)
	if len(*envelopes) != 3 {
		t.Fatalf("focus/close envelopes = %d, want 3: %#v", len(*envelopes), *envelopes)
	}
	if got := (*envelopes)[0].Payload.(windows.FocusChanged); got.Previous != padID || got.Current != screenID {
		t.Fatalf("focus event = %+v", got)
	}
	if got := (*envelopes)[1].Payload.(windows.Closed); got.ID != screenID {
		t.Fatalf("close event = %+v", got)
	}
	if got := (*envelopes)[2].Payload.(windows.FocusChanged); got.Previous != screenID || got.Current != padID {
		t.Fatalf("survivor focus event = %+v", got)
	}
}

func TestResizeClampsWindowsWithoutLosingDrafts(t *testing.T) {
	model, service, _, envelopes := newFixtureModel(t, ui.Plain)
	field := controlID{window: padID, kind: "field", id: "text"}
	focus(model, field)
	update(model, tea.KeyPressMsg{Code: tea.KeyExtended, Text: "kept"})
	*envelopes = nil
	update(model, tea.WindowSizeMsg{Width: 30, Height: 10})
	if len(*envelopes) != 2 {
		t.Fatalf("resize clamp published %d events, want two moves", len(*envelopes))
	}
	for i, id := range []windows.ID{screenID, padID} {
		moved, ok := (*envelopes)[i].Payload.(windows.Moved)
		if !ok || moved.ID != id {
			t.Fatalf("resize event %d = %#v", i, (*envelopes)[i].Payload)
		}
	}
	beforeRender := len(*envelopes)
	model.View()
	model.View()
	if len(*envelopes) != beforeRender {
		t.Fatal("repeated rendering after resize published events")
	}
	for _, id := range []windows.ID{screenID, padID} {
		w, ok := service.Get(id)
		if !ok {
			t.Fatalf("%s disappeared on resize", id)
		}
		b := w.Bounds
		if b.X < 0 || b.Y < 0 || b.Width <= 0 || b.Height <= 0 || b.X+b.Width > 30 || b.Y+b.Height > 9 {
			t.Fatalf("%s bounds escaped 30x10 canvas: %+v", id, b)
		}
	}
	if got := model.presentations[padID].values["text"]; got != "kept" {
		t.Fatalf("resize lost draft: %q", got)
	}
	if content := model.View().Content; !strings.Contains(content, "Pad / blocks") || !strings.Contains(content, "[X]") || !strings.Contains(content, "F6 focus") {
		t.Fatalf("30x10 frame lacks focused title, close, or help:\n%s", content)
	}
	update(model, tea.KeyPressMsg{Code: tea.KeyF6})
	if content := model.View().Content; !strings.Contains(content, "Screen / form") {
		t.Fatalf("30x10 keyboard focus did not raise Screen:\n%s", content)
	}
}

func TestPlainSanitizationAndExplicitProfile(t *testing.T) {
	bus := events.NewBus()
	service := windows.NewService(bus, "test/sanitize")
	view := fixtures.ScreenData()
	view.Items[0].Label = "unsafe\x1b]52;c;Zg==\aLABEL"
	if err := service.Open(windows.Window{
		ID: "unsafe", Title: "title\x1b[2J\x1b]0;bad\a", Bounds: windows.Bounds{X: 0, Y: 0, Width: 50, Height: 16}, Presentation: view,
	}); err != nil {
		t.Fatal(err)
	}
	model := New(service, ui.New(theme.Monochrome(), ui.Plain))
	if cmd := model.Init(); cmd != nil {
		t.Fatal("experimental Init scheduled host probing or other work")
	}
	model.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	content := model.View().Content
	if strings.Contains(content, "\x1b") || strings.Contains(content, "52;") || strings.Contains(content, "[2J") {
		t.Fatalf("Plain output contains color or injected controls: %q", content)
	}
	if !strings.Contains(content, "LABEL") {
		t.Fatalf("sanitized content missing ordinary text: %q", content)
	}

	colorModel, _, _, _ := newFixtureModel(t, ui.ANSI256)
	ansi := colorModel.View().Content
	if regexp.MustCompile(`\x1b\[[0-9;:]*(?:38|48)(?::2|;2)[;:]`).MatchString(ansi) {
		t.Fatalf("ANSI256 output leaked truecolor: %q", ansi)
	}
}
