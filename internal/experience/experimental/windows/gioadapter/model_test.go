package gioadapter

import (
	"image"
	"strings"
	"testing"
	"time"

	"gioui.org/f32"
	"gioui.org/io/event"
	"gioui.org/io/input"
	"gioui.org/io/key"
	"gioui.org/io/pointer"
	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"
	"gostalgia/internal/events"
	"gostalgia/internal/experience/experimental/windows"
	"gostalgia/internal/experience/experimental/windows/fixtures"
	"gostalgia/internal/experience/theme"
	"gostalgia/sdk"
)

const (
	screenID windows.ID = "screen"
	padID    windows.ID = "pad"
)

// harness drives one adapter through a private input.Router. No gioui.org/app
// window, GPU, or display connection exists: frames are pure op lists and all
// input is synthetic, so tests are headless and deterministic.
type harness struct {
	router *input.Router
	ops    op.Ops
	model  *Model
	size   image.Point
	now    time.Time
}

func newFixtureModel(t *testing.T) (*harness, *windows.Service, *[]events.Envelope) {
	t.Helper()
	bus := events.NewBus()
	envelopes := new([]events.Envelope)
	cancel := bus.Subscribe("*", func(envelope events.Envelope) {
		*envelopes = append(*envelopes, envelope)
	})
	t.Cleanup(cancel)
	service := windows.NewService(bus, "test/gio-adapter")
	if err := service.Open(windows.Window{
		ID: screenID, Title: "Screen / form", Bounds: windows.Bounds{X: 8, Y: 8, Width: 280, Height: 240}, Presentation: fixtures.ScreenData(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Open(windows.Window{
		ID: padID, Title: "Pad / blocks and grid", Bounds: windows.Bounds{X: 320, Y: 8, Width: 280, Height: 240}, Presentation: fixtures.PadData(),
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{
		router: new(input.Router),
		model:  New(service, theme.Nostalgia()),
		size:   image.Pt(640, 400),
	}
	h.frame()
	return h, service, envelopes
}

// frame renders one frame and hands its ops to the router, the way a window
// would. A changed constraint is the adapter's resize input, like the bloom
// adapter's WindowSizeMsg.
func (h *harness) frame() {
	h.now = h.now.Add(16 * time.Millisecond)
	h.ops.Reset()
	gtx := layout.Context{
		Ops:         &h.ops,
		Source:      h.router.Source(),
		Constraints: layout.Exact(h.size),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Now:         h.now,
	}
	h.model.Layout(gtx)
	h.router.Frame(&h.ops)
}

// do runs an adapter call inside one frame so it can issue router commands.
func (h *harness) do(fn func(layout.Context)) {
	h.now = h.now.Add(16 * time.Millisecond)
	h.ops.Reset()
	gtx := layout.Context{
		Ops:         &h.ops,
		Source:      h.router.Source(),
		Constraints: layout.Exact(h.size),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Now:         h.now,
	}
	h.model.Layout(gtx)
	fn(gtx)
	h.router.Frame(&h.ops)
}

func (h *harness) focus(c controlID) {
	h.do(func(gtx layout.Context) {
		h.model.activate(gtx, c.window, c)
	})
}

func (h *harness) inject(evts ...event.Event) {
	h.router.Queue(evts...)
	h.frame()
}

func (h *harness) press(p image.Point) {
	h.inject(pointer.Event{Kind: pointer.Press, Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Position: f32.Pt(float32(p.X), float32(p.Y))})
}

func (h *harness) release(p image.Point) {
	h.inject(pointer.Event{Kind: pointer.Release, Source: pointer.Mouse, Position: f32.Pt(float32(p.X), float32(p.Y))})
}

func (h *harness) click(p image.Point) {
	h.press(p)
	h.release(p)
}

func (h *harness) move(p image.Point) {
	h.inject(pointer.Event{Kind: pointer.Move, Source: pointer.Mouse, Buttons: pointer.ButtonPrimary, Position: f32.Pt(float32(p.X), float32(p.Y))})
}

func (h *harness) keys(evts ...event.Event) {
	h.inject(evts...)
}

func keyPress(name key.Name, mods key.Modifiers) event.Event {
	return key.Event{Name: name, Modifiers: mods, State: key.Press}
}

// systemKey wraps a key the way gioui.org/app does for Tab, so the adapter's
// named filters see the same event a real window would deliver.
func systemKey(name key.Name, mods key.Modifiers) event.Event {
	return input.SystemEvent{Event: key.Event{Name: name, Modifiers: mods, State: key.Press}}
}

// semantics returns the flattened semantic tree the router collected for the
// last frame: the headless equivalent of bloom's solved frame rects.
func (h *harness) semantics() []input.SemanticNode {
	return h.router.AppendSemantics(nil)
}

func findNode(nodes []input.SemanticNode, label, desc string) (input.SemanticNode, bool) {
	for _, n := range nodes {
		if n.Desc.Label == label && n.Desc.Description == desc {
			return n, true
		}
		for _, c := range n.Children {
			if found, ok := findNode([]input.SemanticNode{c}, label, desc); ok {
				return found, true
			}
		}
	}
	return input.SemanticNode{}, false
}

func mustFind(t *testing.T, nodes []input.SemanticNode, label, desc string) input.SemanticNode {
	t.Helper()
	n, ok := findNode(nodes, label, desc)
	if !ok {
		var labels []string
		for _, n := range nodes {
			labels = append(labels, n.Desc.Label+"/"+n.Desc.Description)
		}
		t.Fatalf("no semantic node %q/%q; roots: %v", label, desc, labels)
	}
	return n
}

func TestTitledWindowsWithCloseControls(t *testing.T) {
	h, _, _ := newFixtureModel(t)
	nodes := h.semantics()
	screen := mustFind(t, nodes, "Screen / form", "window")
	pad := mustFind(t, nodes, "Pad / blocks and grid", "window")
	if screen.Desc.Bounds.Min != image.Pt(8, 8) || screen.Desc.Bounds.Dx() != 280 || screen.Desc.Bounds.Dy() != 240 {
		t.Fatalf("screen bounds = %v", screen.Desc.Bounds)
	}
	if pad.Desc.Bounds.Min != image.Pt(320, 8) {
		t.Fatalf("pad bounds = %v", pad.Desc.Bounds)
	}
	for _, title := range []string{"Screen / form", "Pad / blocks and grid"} {
		mustFind(t, nodes, title, "title")
		mustFind(t, nodes, "Close "+title, "close")
	}
	for _, want := range []string{"One", "Mascot", "Text", "Send", "Adopt", "NW"} {
		found := false
		var walk func(ns []input.SemanticNode)
		walk = func(ns []input.SemanticNode) {
			for _, n := range ns {
				if strings.Contains(n.Desc.Label, want) {
					found = true
				}
				walk(n.Children)
			}
		}
		walk(nodes)
		if !found {
			t.Fatalf("no semantic node containing %q", want)
		}
	}
}

func TestTitleDragMovesOnlyThatWindow(t *testing.T) {
	h, service, _ := newFixtureModel(t)
	title := mustFind(t, h.semantics(), "Screen / form", "title")
	start := title.Desc.Bounds.Min.Add(image.Pt(4, 4))
	end := start.Add(image.Pt(40, 16))
	screenBefore, _ := service.Get(screenID)
	padBefore, _ := service.Get(padID)
	h.press(start)
	h.move(end)
	h.release(end)
	screenAfter, _ := service.Get(screenID)
	padAfter, _ := service.Get(padID)
	want := screenBefore.Bounds
	want.X += 40
	want.Y += 16
	if screenAfter.Bounds != want {
		t.Fatalf("dragged bounds = %+v, want %+v", screenAfter.Bounds, want)
	}
	if padAfter.Bounds != padBefore.Bounds {
		t.Fatalf("other window moved: %+v", padAfter.Bounds)
	}
	if focused, _ := service.Focused(); focused != screenID {
		t.Fatalf("drag press did not focus screen")
	}
	// The semantic area follows the moved bounds next frame.
	moved := mustFind(t, h.semantics(), "Screen / form", "window")
	if moved.Desc.Bounds.Min != image.Pt(want.X, want.Y) {
		t.Fatalf("moved semantic bounds = %v", moved.Desc.Bounds)
	}
}

func TestClickToRaiseAndF6Cycle(t *testing.T) {
	h, service, _ := newFixtureModel(t)
	// Overlap pad under screen, then click pad's exposed body.
	service.Move(padID, windows.Bounds{X: 40, Y: 40, Width: 280, Height: 240})
	service.Focus(screenID)
	h.frame()
	// Click pad's exposed right edge, outside screen's frame.
	h.click(image.Pt(310, 200))
	if focused, _ := service.Focused(); focused != padID {
		t.Fatalf("clicked window focus = %q, want pad", focused)
	}
	if order := service.Order(); len(order) != 2 || order[1] != padID {
		t.Fatalf("z-order after raise = %v", order)
	}
	h.keys(keyPress(key.NameF6, 0))
	if focused, _ := service.Focused(); focused != screenID {
		t.Fatalf("F6 focus = %q, want screen", focused)
	}
	h.keys(keyPress(key.NameF6, 0))
	if focused, _ := service.Focused(); focused != padID {
		t.Fatalf("F6 cycle focus = %q, want pad", focused)
	}
}

func TestFocusScopesKeepDraftsAndActionsIndependent(t *testing.T) {
	h, service, _ := newFixtureModel(t)
	padField := controlID{window: padID, kind: "field", id: "text"}
	screenField := controlID{window: screenID, kind: "field", id: "text"}
	h.focus(padField)
	h.inject(key.EditEvent{Text: "pad"})
	h.keys(keyPress(key.NameF6, 0))
	h.focus(screenField)
	h.inject(key.EditEvent{Text: "screen"})
	h.inject(key.Event{Name: key.NameDownArrow, State: key.Press})
	if got := h.model.presentations[padID].values["text"]; got != "pad" {
		t.Fatalf("pad draft changed through screen scope: %q", got)
	}
	if got := h.model.presentations[screenID].values["text"]; got != "screen" {
		t.Fatalf("screen draft = %q", got)
	}
	// Tab stays inside the focused window's scope.
	for range 4 {
		h.keys(systemKey(key.NameTab, 0))
		if h.model.current == nil || h.model.current.id.window != screenID {
			t.Fatalf("Tab escaped screen scope: %+v", h.model.current)
		}
	}
	h.keys(systemKey(key.NameTab, key.ModShift))
	if h.model.current == nil || h.model.current.id.window != screenID {
		t.Fatalf("Shift-Tab escaped screen scope: %+v", h.model.current)
	}
	h.focus(controlID{window: screenID, kind: "action", id: "send"})
	h.keys(keyPress(key.NameReturn, 0))
	actions := h.model.Actions(screenID)
	if len(actions) != 1 {
		t.Fatalf("screen emitted %d actions, want 1", len(actions))
	}
	action := actions[0]
	if action.Action != "send" || action.Values["text"] != "screen" || action.ItemID != "two" || action.Instance != "launch" || action.Version != 1 || action.RequestID == "" {
		t.Fatalf("screen semantic request = %+v", action)
	}
	if len(h.model.Actions(padID)) != 0 {
		t.Fatal("screen action leaked into pad")
	}
	h.keys(keyPress(key.NameF6, 0))
	if focused, _ := service.Focused(); focused != padID {
		t.Fatalf("F6 focus = %q", focused)
	}
	if got := h.model.presentations[padID].values["text"]; got != "pad" {
		t.Fatalf("pad draft was not retained: %q", got)
	}
	h.focus(controlID{window: padID, kind: "cell", id: "nw"})
	h.keys(keyPress(key.NameReturn, 0))
	padActions := h.model.Actions(padID)
	if len(padActions) != 1 || padActions[0].Action != "press" || padActions[0].CellID != "nw" || padActions[0].Values["text"] != "pad" || padActions[0].Version != 2 {
		t.Fatalf("pad semantic requests = %+v", padActions)
	}
	h.focus(controlID{window: padID, kind: "cell", id: "ne"})
	h.focus(controlID{window: padID, kind: "action", id: "adopt"})
	h.keys(keyPress(key.NameReturn, 0))
	padActions = h.model.Actions(padID)
	if len(padActions) != 2 || padActions[1].Action != "adopt" || padActions[1].CellID != "ne" || padActions[1].Values["text"] != "pad" {
		t.Fatalf("top-level action lost selected grid cell: %+v", padActions)
	}
	h.model.activateCell(padID, sdk.Cell{ID: "se", Label: "SE", Action: "press", Disabled: true})
	if len(h.model.Actions(padID)) != 2 {
		t.Fatal("disabled cell emitted an action")
	}
}

func TestKeyboardMoveUsesSameBoundsTransition(t *testing.T) {
	h, service, envelopes := newFixtureModel(t)
	h.focus(controlID{window: screenID, kind: "close"})
	before, _ := service.Get(screenID)
	otherBefore, _ := service.Get(padID)
	*envelopes = nil
	h.keys(keyPress(key.NameRightArrow, key.ModAlt))
	after, _ := service.Get(screenID)
	otherAfter, _ := service.Get(padID)
	want := before.Bounds
	want.X += keyMoveStep
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

func TestGridArrowNavigationTracksFocusAndDisabledCells(t *testing.T) {
	h, _, _ := newFixtureModel(t)
	h.focus(controlID{window: padID, kind: "cell", id: "nw"})
	h.keys(systemKey(key.NameTab, 0))
	if h.model.current == nil || h.model.current.id.id != "ne" || h.model.presentations[padID].cell != 1 {
		t.Fatalf("Tab did not synchronize NE: control %+v, index %d", h.model.current, h.model.presentations[padID].cell)
	}
	h.keys(keyPress(key.NameRightArrow, 0))
	if h.model.current.id.id != "sw" || h.model.presentations[padID].cell != 2 {
		t.Fatalf("Right from NE did not select SW: control %+v, index %d", h.model.current, h.model.presentations[padID].cell)
	}
	h.keys(keyPress(key.NameRightArrow, 0))
	if h.model.current.id.id != "sw" || h.model.presentations[padID].cell != 2 {
		t.Fatalf("disabled SE desynchronized selection: control %+v, index %d", h.model.current, h.model.presentations[padID].cell)
	}
	h.keys(keyPress(key.NameLeftArrow, 0))
	if h.model.current.id.id != "ne" || h.model.presentations[padID].cell != 1 {
		t.Fatalf("Left after disabled target did not return to NE: control %+v, index %d", h.model.current, h.model.presentations[padID].cell)
	}
}

func TestKeyboardAndBorderCloseLeaveSurvivorUsable(t *testing.T) {
	for _, closeID := range []windows.ID{screenID, padID} {
		t.Run(string(closeID)+"-keyboard", func(t *testing.T) {
			h, service, _ := newFixtureModel(t)
			h.focus(controlID{window: closeID, kind: "close"})
			h.keys(keyPress("W", key.ModCtrl))
			assertSurvivorUsable(t, h, service, closeID)
		})
		t.Run(string(closeID)+"-border", func(t *testing.T) {
			h, service, _ := newFixtureModel(t)
			title := "Screen / form"
			if closeID == padID {
				title = "Pad / blocks and grid"
			}
			closeNode := mustFind(t, h.semantics(), "Close "+title, "close")
			h.click(closeNode.Desc.Bounds.Min.Add(image.Pt(2, 2)))
			assertSurvivorUsable(t, h, service, closeID)
		})
	}
}

func assertSurvivorUsable(t *testing.T, h *harness, service *windows.Service, closed windows.ID) {
	t.Helper()
	if _, ok := service.Get(closed); ok {
		t.Fatalf("close left %s open", closed)
	}
	survivor := screenID
	if closed == screenID {
		survivor = padID
	}
	if _, ok := service.Get(survivor); !ok {
		t.Fatal("close removed the other window")
	}
	if focused, _ := service.Focused(); focused != survivor {
		t.Fatalf("survivor focus = %q", focused)
	}
	// A pointer close lands mid-layout, so the frame it fired in still paints
	// the closing window; the next frame is the surviving one.
	h.frame()
	nodes := h.semantics()
	if _, ok := findNode(nodes, closedTitle(closed), "window"); ok {
		t.Fatal("closed window still in semantic tree")
	}
	mustFind(t, nodes, closedTitle(survivor), "window")
	h.focus(controlID{window: survivor, kind: "field", id: "text"})
	h.inject(key.EditEvent{Text: "alive"})
	if got := h.model.presentations[survivor].values["text"]; got != "alive" {
		t.Fatalf("survivor %s did not accept input: %q", survivor, got)
	}
}

func closedTitle(id windows.ID) string {
	if id == padID {
		return "Pad / blocks and grid"
	}
	return "Screen / form"
}

func TestLifecycleEventsAndRenderingIsInert(t *testing.T) {
	h, _, envelopes := newFixtureModel(t)
	if len(*envelopes) != 4 {
		t.Fatalf("open envelopes = %d, want 4", len(*envelopes))
	}
	if _, ok := (*envelopes)[0].Payload.(windows.Opened); !ok {
		t.Fatalf("open payload type = %T", (*envelopes)[0].Payload)
	}
	if (*envelopes)[0].Source != "test/gio-adapter" {
		t.Fatalf("event source = %q", (*envelopes)[0].Source)
	}
	*envelopes = nil
	for range 3 {
		h.frame()
	}
	if len(*envelopes) != 0 {
		t.Fatalf("rendering published lifecycle events: %#v", *envelopes)
	}
	h.focus(controlID{window: screenID, kind: "close"})
	h.do(func(gtx layout.Context) {
		h.model.close(gtx, screenID)
	})
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
	h, service, envelopes := newFixtureModel(t)
	h.focus(controlID{window: padID, kind: "field", id: "text"})
	h.inject(key.EditEvent{Text: "kept"})
	*envelopes = nil
	h.size = image.Pt(200, 160)
	h.frame()
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
	h.frame()
	h.frame()
	if len(*envelopes) != beforeRender {
		t.Fatal("repeated rendering after resize published events")
	}
	for _, id := range []windows.ID{screenID, padID} {
		w, ok := service.Get(id)
		if !ok {
			t.Fatalf("%s disappeared on resize", id)
		}
		b := w.Bounds
		if b.X < 0 || b.Y < 0 || b.Width <= 0 || b.Height <= 0 || b.X+b.Width > 200 || b.Y+b.Height > 160-statusBand {
			t.Fatalf("%s bounds escaped 200x160 canvas: %+v", id, b)
		}
	}
	if got := h.model.presentations[padID].values["text"]; got != "kept" {
		t.Fatalf("resize lost draft: %q", got)
	}
	h.keys(keyPress(key.NameF6, 0))
	if focused, _ := service.Focused(); focused != screenID {
		t.Fatalf("post-resize F6 focus = %q", focused)
	}
}

func TestSanitizedSemanticLabels(t *testing.T) {
	bus := events.NewBus()
	service := windows.NewService(bus, "test/sanitize")
	view := fixtures.ScreenData()
	view.Items[0].Label = "unsafe\x1b]52;c;Zg==\aLABEL"
	if err := service.Open(windows.Window{
		ID: "unsafe", Title: "title\x1b[2J\x1b]0;bad\a", Bounds: windows.Bounds{X: 0, Y: 0, Width: 300, Height: 240}, Presentation: view,
	}); err != nil {
		t.Fatal(err)
	}
	h := &harness{router: new(input.Router), model: New(service, theme.Monochrome()), size: image.Pt(400, 300)}
	h.frame()
	nodes := h.semantics()
	var dump func(ns []input.SemanticNode, out *strings.Builder)
	dump = func(ns []input.SemanticNode, out *strings.Builder) {
		for _, n := range ns {
			out.WriteString(n.Desc.Label)
			out.WriteByte('\n')
			dump(n.Children, out)
		}
	}
	var all strings.Builder
	dump(nodes, &all)
	labels := all.String()
	if strings.Contains(labels, "\x1b") || strings.Contains(labels, "52;") || strings.Contains(labels, "[2J") {
		t.Fatalf("semantic labels carry injected controls: %q", labels)
	}
	if !strings.Contains(labels, "LABEL") {
		t.Fatalf("sanitized label missing ordinary text: %q", labels)
	}
}

func TestRealShapingProducesTitleGlyphs(t *testing.T) {
	// The spike claims real text, not just hit areas. The title's semantic
	// area cannot show that: its inset and flexed slot give it extent even
	// for an empty title. Measure the label itself under loose constraints so
	// its width comes only from shaped glyph advances.
	h, _, _ := newFixtureModel(t)
	title := measureLabel(h.model, "Screen / form")
	if title.X <= 0 || title.Y <= 0 {
		t.Fatalf("title shaped to %v, want positive extent", title)
	}
	if longer := measureLabel(h.model, "Screen / form / and more"); longer.X <= title.X {
		t.Fatalf("longer title is %dpx wide, want more than %dpx", longer.X, title.X)
	}
}

func measureLabel(m *Model, s string) image.Point {
	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Constraints: layout.Constraints{Max: image.Pt(1000, 100)},
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
	}
	return m.label(gtx, s, m.colors.text).Size
}
