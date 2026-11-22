package shell

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"bloomfx.dev/bloom/canvas"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
	"gostalgia/sdk"
)

const (
	alphaID windows.ID = "com.test.alpha"
	betaID  windows.ID = "com.test.beta"
)

// openTitled opens a loaded window of data for id, titled title.
func openTitled(m *Model, id windows.ID, title string, data sdk.View) *appView {
	data.Title = title
	return m.showWindow(&appView{id: string(id), data: data, instance: "launch", version: data.Version})
}

// twoWindows opens Alpha's pad, then Beta's screen over it: Alpha fills the
// body and Beta aligns flush-left one row down, inset on the right and bottom
// for the shadow and movement.
func twoWindows(m *Model) *Model {
	openTitled(m, alphaID, "Alpha", padData())
	openTitled(m, betaID, "Beta", screenData())
	return m
}

// floating is the fixture shell at w×h with twoWindows open.
func floating(t *testing.T, th theme.Theme, mode ui.ColorMode, w, h int) (*Model, *recordingCaller) {
	t.Helper()
	rec := &recordingCaller{}
	m := sized(fixtureModel(th, mode), w, h)
	m.client = rec
	return twoWindows(m), rec
}

func boundsOf(t *testing.T, m *Model, id windows.ID) windows.Bounds {
	t.Helper()
	w, ok := m.windows.Get(id)
	if !ok {
		t.Fatalf("no %s window", id)
	}
	return w.Bounds
}

func topWindow(m *Model) windows.ID {
	order := m.windows.Order()
	if len(order) == 0 {
		return ""
	}
	return order[len(order)-1]
}

func alt(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code, Mod: tea.ModAlt} }

// row is the text of screen row y in columns [x0, x1).
func row(m *Model, y, x0, x1 int) string {
	lines := screenText(m.View())
	if y < 0 || y >= len(lines) {
		return ""
	}
	return ansi.Cut(lines[y], x0, x1)
}

func TestWindowsOpenFillingTheBodyThenCascade(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {100, 32}} {
		t.Run(sizeName(size), func(t *testing.T) {
			m, _ := mouseModel(t, size[0], size[1])
			body := rectOf(t, m, slotBody)
			for i := range maxWindows {
				id := windows.ID(fmt.Sprintf("com.test.w%d", i))
				openTitled(m, id, fmt.Sprintf("W%d", i), screenData())
				want := body
				if i > 0 {
					// Flush-left vertical cascade: step down one row per
					// window, inset by windowMarginX on the right and
					// windowShadowY below while keeping the minimum floor.
					y := min(i, body.Dy()-minWindowHeight)
					h := max(minWindowHeight, body.Dy()-y-windowShadowY)
					want = image.Rect(0, 0, body.Dx()-windowMarginX, h).Add(body.Min).Add(image.Pt(0, y))
				}
				if got := rectOf(t, m, windowLayerID(id)); got != want {
					t.Fatalf("window %d opened at %v, want %v", i, got, want)
				}
			}
		})
	}

	// The cascade steps vertically from where the window before it is now,
	// while keeping flush-left alignment.
	m, _ := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	for _, k := range []tea.KeyPressMsg{alt(tea.KeyRight), alt(tea.KeyUp)} {
		act(t, m, k)
	}
	openTitled(m, "com.test.gamma", "Gamma", screenData())
	if b := boundsOf(t, m, "com.test.gamma"); b.X != 0 || b.Y != 1 {
		t.Fatalf("Gamma opened at %d,%d, want 0,1 from Beta at 1,0", b.X, b.Y)
	}
}

func TestWindowsStayInsideAResizedBody(t *testing.T) {
	m, _ := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	before := map[windows.ID]image.Rectangle{
		alphaID: rectOf(t, m, windowLayerID(alphaID)),
		betaID:  rectOf(t, m, windowLayerID(betaID)),
	}
	// Below the floor no frame shows and nothing moves, so the windows
	// come back as they were.
	for _, size := range [][2]int{{40, 18}, {30, 10}} {
		sized(m, size[0], size[1])
		if m.floatingWindows() {
			t.Fatalf("%s floats windows", sizeName(size))
		}
	}
	sized(m, 80, 24)
	for id, r := range before {
		if got := rectOf(t, m, windowLayerID(id)); got != r {
			t.Fatalf("%s came back at %v, want %v", id, got, r)
		}
	}
	// A smaller body moves and shrinks them in, never below the floor.
	for _, size := range [][2]int{{60, 22}, {36, 21}, {120, 40}, {80, 24}} {
		sized(m, size[0], size[1])
		if !m.floatingWindows() {
			t.Fatalf("%s does not float windows", sizeName(size))
		}
		body := rectOf(t, m, slotBody)
		for _, id := range []windows.ID{alphaID, betaID} {
			r := rectOf(t, m, windowLayerID(id))
			if !r.In(body) || r.Dx() < minWindowWidth || r.Dy() < minWindowHeight {
				t.Fatalf("%s: %s at %v in body %v", sizeName(size), id, r, body)
			}
		}
	}
}

func TestWindowsAreOpaqueWithTheirTitleAndCloseOnTheBorder(t *testing.T) {
	for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256, ui.TrueColor} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			m, _ := floating(t, theme.Nostalgia(), mode, 80, 24)
			content := m.View().Content
			if mode == ui.Plain && strings.Contains(content, "\x1b") {
				t.Fatal("Plain windows emitted escapes")
			}
			cells := canvas.Parse(content, ansi.GraphemeWidth)
			beta := rectOf(t, m, windowLayerID(betaID))
			// Alpha's art and meters lie beneath Beta; none shows through,
			// not even in Beta's blank rows.
			for y := beta.Min.Y; y < beta.Max.Y; y++ {
				r := row(m, y, beta.Min.X, beta.Max.X)
				for _, under := range []string{`_/`, "harge", "unger", "Alpha"} {
					if strings.Contains(r, under) {
						t.Fatalf("Alpha's %q shows through Beta on row %d: %q", under, y, r)
					}
				}
				for x := beta.Min.X; mode != ui.Plain && x < beta.Max.X; x++ {
					if c := cells.CellAt(x, y); c == nil || c.Style.Bg == nil {
						t.Fatalf("Beta's cell %d,%d has no background of its own", x, y)
					}
				}
			}
			title := rectOf(t, m, windowPart{betaID, windowTitle})
			closeRect := rectOf(t, m, windowPart{betaID, windowClose})
			if closeRect.Min.Y != beta.Min.Y || closeRect.Dx() != len(closeLabel) || closeRect.Max.X > beta.Max.X-1 {
				t.Fatalf("close control %v is not on Beta's top border %v", closeRect, beta)
			}
			if title.Min.Y != beta.Min.Y || title.Min.X <= beta.Min.X || title.Max.X > closeRect.Min.X {
				t.Fatalf("title %v collides with close %v on %v", title, closeRect, beta)
			}
			if got := row(m, beta.Min.Y, closeRect.Min.X, closeRect.Max.X); got != closeLabel {
				t.Fatalf("close control reads %q", got)
			}
			if got := row(m, beta.Min.Y, title.Min.X, title.Max.X); !strings.Contains(got, m.kit.Theme().Focus.Marker+" Beta") {
				t.Fatalf("focused title reads %q", got)
			}
			if got := row(m, 0, 0, 80); strings.Contains(got, "Alpha") {
				t.Fatalf("the frame shows a window title: %q", got)
			}
			alphaTitle := rectOf(t, m, windowPart{alphaID, windowTitle})
			if got := row(m, alphaTitle.Min.Y, alphaTitle.Min.X, alphaTitle.Max.X); !strings.Contains(got, "  Alpha") {
				t.Fatalf("unfocused title reads %q", got)
			}
		})
	}
}

func TestWindowTitlesTruncateAndKeepTheirCloseControl(t *testing.T) {
	long := "A deliberately long window title that must never displace the close control"
	for _, tc := range []struct{ name, title string }{
		{"long", long},
		{"wide glyphs", "ｗｉｄｅ　ｔｉｔｌｅ　ｗｉｔｈ　ｉｄｅｏｇｒａｐｈｓ"},
		{"hostile controls", "safe\x1b[31mRED\x1b[0m\u009b2J" + long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A 36-column terminal leaves a 32-column body: the narrowest
			// window there is.
			m, _ := mouseModel(t, 36, 24)
			openTitled(m, alphaID, tc.title, screenData())
			win := rectOf(t, m, windowLayerID(alphaID))
			if win.Dx() != minWindowWidth {
				t.Fatalf("window is %d wide, want the %d floor", win.Dx(), minWindowWidth)
			}
			if content := m.View().Content; strings.ContainsAny(content, "\x1b\u009b") {
				t.Fatalf("the title injected terminal controls:\n%q", content)
			}
			title := rectOf(t, m, windowPart{alphaID, windowTitle})
			closeRect := rectOf(t, m, windowPart{alphaID, windowClose})
			if got := row(m, win.Min.Y, closeRect.Min.X, closeRect.Max.X); got != closeLabel {
				t.Fatalf("close control reads %q, want %q", got, closeLabel)
			}
			if budget := win.Dx() - topBorderSpan - len(closeLabel) - 1; title.Dx() > budget || title.Max.X > closeRect.Min.X {
				t.Fatalf("title %v overruns its %d cells before close %v", title, budget, closeRect)
			}
			if got := strings.TrimRight(row(m, win.Min.Y, title.Min.X, title.Max.X), " "); !strings.HasSuffix(got, "…") {
				t.Fatalf("title %q is not cut with an ellipsis", got)
			}
			if got := ansi.StringWidth(screenText(m.View())[win.Min.Y]); got != 36 {
				t.Fatalf("the title row is %d cells wide, want 36", got)
			}
		})
	}
}

func TestWindowShadowsTintWhatIsBeneathAndNotTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		th     theme.Theme
		mode   ui.ColorMode
		shadow bool
	}{
		{"nostalgia ansi256", theme.Nostalgia(), ui.ANSI256, true},
		{"midnight truecolor", theme.Midnight(), ui.TrueColor, true},
		{"nostalgia ansi16", theme.Nostalgia(), ui.ANSI16, false},
		{"monochrome ansi256", theme.Monochrome(), ui.ANSI256, false},
		{"nostalgia plain", theme.Nostalgia(), ui.Plain, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := floating(t, tc.th, tc.mode, 80, 24)
			content := m.View().Content
			cells := canvas.Parse(content, ansi.GraphemeWidth)
			alpha, beta := rectOf(t, m, windowLayerID(alphaID)), rectOf(t, m, windowLayerID(betaID))
			// Alpha's right border beside Beta, above the shadow band and
			// inside it.
			x := alpha.Max.X - 1
			clear, shaded := cells.CellAt(x, beta.Min.Y), cells.CellAt(x, beta.Min.Y+2)
			if clear == nil || shaded == nil || x < beta.Max.X || x >= beta.Max.X+2 {
				t.Fatalf("no shadow sample at column %d beside %v", x, beta)
			}
			if clear.Content != shaded.Content || fmt.Sprint(clear.Style.Fg) != fmt.Sprint(shaded.Style.Fg) {
				t.Fatalf("the shadow changed what it falls on: %q %v -> %q %v", clear.Content, clear.Style.Fg, shaded.Content, shaded.Style.Fg)
			}
			if tinted := fmt.Sprint(clear.Style.Bg) != fmt.Sprint(shaded.Style.Bg); tinted != tc.shadow {
				t.Fatalf("shadow tint = %v, want %v (bg %v -> %v)", tinted, tc.shadow, clear.Style.Bg, shaded.Style.Bg)
			}
			// The window that casts it stays untouched: its close control
			// has the background of its own border.
			closeRect := rectOf(t, m, windowPart{betaID, windowClose})
			corner, closeCell := cells.CellAt(beta.Min.X, beta.Min.Y), cells.CellAt(closeRect.Min.X, closeRect.Min.Y)
			if closeCell.Content != "[" || fmt.Sprint(closeCell.Style.Bg) != fmt.Sprint(corner.Style.Bg) {
				t.Fatalf("close control %q bg %v, border bg %v", closeCell.Content, closeCell.Style.Bg, corner.Style.Bg)
			}
			// Static: the same frame again, and no frames scheduled.
			if m.View().Content != content {
				t.Fatal("the shadow is not static")
			}
			if _, cmd := m.Update(struct{}{}); cmd != nil {
				t.Fatal("idle windows scheduled work")
			}
		})
	}
}

func TestWindowsDragByTheirTitle(t *testing.T) {
	m, _ := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	press := rectOf(t, m, windowPart{betaID, windowTitle}).Min.Add(image.Pt(3, 0))
	to := func(p image.Point) {
		mouse(t, m, tea.MouseMotionMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
	}
	want := func(x, y int) {
		t.Helper()
		if b := boundsOf(t, m, betaID); b.X != x || b.Y != y {
			t.Fatalf("Beta at %d,%d, want %d,%d", b.X, b.Y, x, y)
		}
	}
	mouse(t, m, tea.MouseClickMsg{X: press.X, Y: press.Y, Button: tea.MouseLeft})
	to(press.Add(image.Pt(1, 1)))
	want(1, 2)
	// Past the body's edges the window stops at them.
	to(press.Add(image.Pt(30, 30)))
	want(2, 2)
	to(press.Add(image.Pt(-30, -30)))
	want(0, 0)
	mouse(t, m, tea.MouseReleaseMsg{X: press.X - 30, Y: press.Y - 30, Button: tea.MouseLeft})
	mouse(t, m, tea.MouseMotionMsg{X: press.X + 5, Y: press.Y + 5})
	want(0, 0)
	if b := boundsOf(t, m, betaID); b.Width != 74 || b.Height != boundsOf(t, m, alphaID).Height-2 {
		t.Fatalf("dragging resized Beta to %d×%d", b.Width, b.Height)
	}

	// Pressing a lower window's title raises and focuses it at once.
	m, _ = floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	p := rectOf(t, m, windowPart{alphaID, windowTitle}).Min.Add(image.Pt(3, 0))
	mouse(t, m, tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
	if m.FocusedWindow() != string(alphaID) || topWindow(m) != alphaID {
		t.Fatalf("the press did not raise Alpha: focused %q, top %q", m.FocusedWindow(), topWindow(m))
	}
	mouse(t, m, tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
}

func TestWindowsRaiseOnAClickAndCloseOnTheirX(t *testing.T) {
	// Beta's window shows all of its pad at this size.
	m, rec := mouseModel(t, 100, 30)
	openTitled(m, alphaID, "Alpha", screenData())
	openTitled(m, betaID, "Beta", padData())
	// The focused window's content takes clicks where it is drawn.
	beta := rectOf(t, m, windowLayerID(betaID))
	clickAt(t, m, mustFind(t, m, beta, "[Adopt]"))
	if got := calls(rec, "app/com.test.beta/action"); len(got) != 1 {
		t.Fatalf("a click on Beta's action sent %v", rec.all())
	}
	m.focusedView().busy, m.focusedView().cancel = false, nil

	// An obscured lower window shows no clipped control fragments, and
	// clicking its exposed chrome raises it without invoking any action.
	m, rec = mouseModel(t, 80, 24)
	openTitled(m, alphaID, "Alpha", screenData())
	openTitled(m, betaID, "Beta", padData())
	alpha := rectOf(t, m, windowLayerID(alphaID))
	beta = rectOf(t, m, windowLayerID(betaID))
	if _, found := findText(m, image.Rect(beta.Max.X, beta.Min.Y, alpha.Max.X, alpha.Max.Y), "["); found {
		t.Fatal("obscured Alpha leaked control fragments into the margin beside Beta")
	}
	clickAt(t, m, image.Pt(alpha.Max.X-1, beta.Min.Y+2))
	if m.FocusedWindow() != string(alphaID) || topWindow(m) != alphaID || len(rec.all()) != 0 {
		t.Fatalf("the click did not just raise Alpha: focused %q, sent %v", m.FocusedWindow(), rec.all())
	}

	// When two windows sit side by side without overlapping, clicking a
	// visible control in the unfocused window still only raises it.
	m, rec = mouseModel(t, 100, 32)
	openTitled(m, alphaID, "Alpha", screenData())
	openTitled(m, betaID, "Beta", padData())
	m.windows.Move(alphaID, windows.Bounds{X: 0, Y: 0, Width: 44, Height: 16})
	m.windows.Move(betaID, windows.Bounds{X: 48, Y: 0, Width: 44, Height: 16})
	alpha = rectOf(t, m, windowLayerID(alphaID))
	clickAt(t, m, mustFind(t, m, alpha, "[Send]"))
	if m.FocusedWindow() != string(alphaID) || topWindow(m) != alphaID || len(rec.all()) != 0 {
		t.Fatalf("clicking unobscured Alpha's [Send] did not just raise it: focused %q, sent %v", m.FocusedWindow(), rec.all())
	}

	// [X] closes its window, raised or not, as Ctrl-W closes the focused one.
	m, rec = floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	clickAt(t, m, rectOf(t, m, windowPart{alphaID, windowClose}).Min.Add(image.Pt(1, 0)))
	if m.windows.Len() != 1 || m.views[alphaID] != nil || m.FocusedWindow() != string(betaID) || !m.onWindows() {
		t.Fatalf("[X] on Alpha: open %v, focused %q", m.windows.Order(), m.FocusedWindow())
	}
	clickAt(t, m, rectOf(t, m, windowPart{betaID, windowClose}).Min)
	byKey, keyRec := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	act(t, byKey, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	act(t, byKey, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	sameOutcome(t, m, byKey, rec, keyRec)
	if m.onWindows() || m.windows.Len() != 0 {
		t.Fatal("closing the last window by its [X] stayed on the Windows screen")
	}
}

func TestWindowsMoveWithAltArrowsAndMoveMode(t *testing.T) {
	m, _ := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	want := func(x, y int) {
		t.Helper()
		if b := boundsOf(t, m, betaID); b.X != x || b.Y != y {
			t.Fatalf("Beta at %d,%d, want %d,%d", b.X, b.Y, x, y)
		}
	}
	inside := stateOf(m)
	for _, step := range []struct {
		key  rune
		x, y int
	}{{tea.KeyRight, 1, 1}, {tea.KeyRight, 2, 1}, {tea.KeyRight, 2, 1}, {tea.KeyDown, 2, 2}, {tea.KeyDown, 2, 2}, {tea.KeyLeft, 1, 2}, {tea.KeyUp, 1, 1}} {
		act(t, m, alt(step.key))
		want(step.x, step.y)
	}
	if stateOf(m) != inside {
		t.Fatalf("Alt-arrows moved focus inside the window: %+v", stateOf(m))
	}
	// Alpha fills the body, so it has nowhere to go.
	act(t, m, key(tea.KeyF8))
	act(t, m, alt(tea.KeyRight))
	if b := boundsOf(t, m, alphaID); b.X != 0 || b.Y != 0 {
		t.Fatalf("a window filling the body moved to %d,%d", b.X, b.Y)
	}
	act(t, m, key(tea.KeyF8))

	// Move window is the same for terminals without Alt-arrows.
	m.openPalette()
	for _, r := range "move window" {
		act(t, m, typed(string(r)))
	}
	act(t, m, enterKey)
	if !m.movingWindow() || m.paletteOpen {
		t.Fatal("Move window did not start move mode")
	}
	help := screenText(m.View())[23]
	if !strings.Contains(help, "MOVE") || !strings.Contains(help, "DONE") {
		t.Fatalf("the help bar lacks the move keys: %q", help)
	}
	act(t, m, key(tea.KeyRight))
	act(t, m, key(tea.KeyUp))
	want(2, 0)
	draft := m.focusedView().values["text"]
	act(t, m, typed("x"))
	act(t, m, tabKey)
	if m.focusedView().values["text"] != draft || stateOf(m).focus != inside.focus {
		t.Fatal("move mode passed keys to the window")
	}
	act(t, m, enterKey)
	if m.movingWindow() || !m.onWindows() || strings.Contains(screenText(m.View())[23], "DONE") {
		t.Fatal("Enter did not end move mode on the Windows screen")
	}
	act(t, m, key(tea.KeyRight))
	want(2, 0)
	// Esc ends it too, and leaves the Windows screen only when pressed again.
	m.startMovingWindow()
	act(t, m, escKey)
	if m.movingWindow() || !m.onWindows() {
		t.Fatal("Esc did not end move mode on the Windows screen")
	}
	act(t, m, escKey)
	if m.onWindows() {
		t.Fatal("Esc after move mode stayed on the Windows screen")
	}
}

func TestWindowsTakeNoInputUnderAModal(t *testing.T) {
	for _, modal := range []struct {
		name string
		open func(m *Model)
	}{
		{"palette", func(m *Model) { m.openPalette(); settleFiniteMotion(m) }},
		{"confirmation", func(m *Model) { m.openConfirm(confirmation{kind: confirmReap}); settleFiniteMotion(m) }},
	} {
		t.Run(modal.name, func(t *testing.T) {
			m, rec := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
			closeAlpha := rectOf(t, m, windowPart{alphaID, windowClose}).Min.Add(image.Pt(1, 0))
			titleAlpha := rectOf(t, m, windowPart{alphaID, windowTitle}).Min.Add(image.Pt(3, 0))
			titleBeta := rectOf(t, m, windowPart{betaID, windowTitle}).Min.Add(image.Pt(3, 0))
			stacking := func() string {
				return fmt.Sprint(m.FocusedWindow(), m.windows.Order(), boundsOf(t, m, alphaID), boundsOf(t, m, betaID))
			}
			before := stacking()
			// A frame drawn before the modal opened, whose clicks arrive after.
			stale := m.View()
			modal.open(m)
			onFrame(t, m, stale, click(closeAlpha)...)
			onFrame(t, m, stale, click(titleAlpha)...)
			onFrame(t, m, stale, tea.MouseMotionMsg{X: closeAlpha.X, Y: closeAlpha.Y})
			for _, p := range []image.Point{closeAlpha, titleAlpha} {
				clickAt(t, m, p)
			}
			mouse(t, m, tea.MouseClickMsg{X: titleBeta.X, Y: titleBeta.Y, Button: tea.MouseLeft})
			mouse(t, m, tea.MouseMotionMsg{X: titleBeta.X - 2, Y: titleBeta.Y + 1, Button: tea.MouseLeft})
			mouse(t, m, tea.MouseReleaseMsg{X: titleBeta.X - 2, Y: titleBeta.Y + 1, Button: tea.MouseLeft})
			mouse(t, m, tea.MouseMotionMsg{X: closeAlpha.X, Y: closeAlpha.Y})
			act(t, m, alt(tea.KeyLeft))
			act(t, m, key(tea.KeyF8))
			if got := stacking(); got != before || len(rec.all()) != 0 {
				t.Fatalf("windows under the %s changed: %s, want %s; sent %v", modal.name, got, before, rec.all())
			}
			if strings.Contains(strings.Join(screenText(m.View()), "\n"), closeHoverLabel) {
				t.Fatalf("a window under the %s shows a hover cue", modal.name)
			}
			if !m.modalOpen() {
				t.Fatalf("the %s closed", modal.name)
			}
		})
	}
}

func TestWindowsWithTheMouseOffTakeOnlyKeys(t *testing.T) {
	m, rec := floating(t, theme.Nostalgia(), ui.Plain, 80, 24)
	closeAlpha := rectOf(t, m, windowPart{alphaID, windowClose}).Min.Add(image.Pt(1, 0))
	titleBeta := rectOf(t, m, windowPart{betaID, windowTitle}).Min.Add(image.Pt(3, 0))
	m.setMouse(false)
	before := fmt.Sprint(m.FocusedWindow(), m.windows.Order(), boundsOf(t, m, betaID))
	clickAt(t, m, closeAlpha)
	alpha := rectOf(t, m, windowLayerID(alphaID))
	clickAt(t, m, image.Pt(alpha.Max.X-1, alpha.Min.Y+4))
	dragBetween(t, m, titleBeta, titleBeta.Add(image.Pt(2, 1)))
	mouse(t, m, tea.MouseMotionMsg{X: closeAlpha.X, Y: closeAlpha.Y})
	if got := fmt.Sprint(m.FocusedWindow(), m.windows.Order(), boundsOf(t, m, betaID)); got != before || len(rec.all()) != 0 {
		t.Fatalf("the mouse acted while off: %s, want %s", got, before)
	}
	if strings.Contains(strings.Join(screenText(m.View()), "\n"), closeHoverLabel) {
		t.Fatal("a hover cue showed with the mouse off")
	}
	// Every window operation still has its key.
	act(t, m, alt(tea.KeyRight))
	act(t, m, key(tea.KeyF8))
	if boundsOf(t, m, betaID).X != 1 || m.FocusedWindow() != string(alphaID) {
		t.Fatal("Alt-arrows or F8 did nothing with the mouse off")
	}
	act(t, m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
	if m.windows.Len() != 1 || m.FocusedWindow() != string(betaID) {
		t.Fatal("Ctrl-W did nothing with the mouse off")
	}
}

func TestWindowsBelowTheFloorShowOnlyTheFocusedOne(t *testing.T) {
	for _, size := range [][2]int{{30, 10}, {40, 18}} {
		t.Run(sizeName(size), func(t *testing.T) {
			m, _ := floating(t, theme.Nostalgia(), ui.Plain, size[0], size[1])
			screen := func() string { return strings.Join(screenText(m.View()), "\n") }
			if s := screen(); strings.Contains(s, closeLabel) || strings.Contains(s, "Alpha") || !strings.Contains(s, "Windows (2)") {
				t.Fatalf("below the floor:\n%s", s)
			}
			for _, it := range m.paletteItems() {
				if it.Title == "Move window" {
					t.Fatal("the palette offers Move window with no window frames")
				}
			}
			before := boundsOf(t, m, betaID)
			act(t, m, alt(tea.KeyLeft))
			m.startMovingWindow()
			act(t, m, key(tea.KeyLeft))
			if boundsOf(t, m, betaID) != before || m.movingWindow() {
				t.Fatal("a window moved below the floor")
			}
			act(t, m, key(tea.KeyF8))
			if m.FocusedWindow() != string(alphaID) || !m.onWindows() {
				t.Fatal("F8 did not focus Alpha below the floor")
			}
			act(t, m, tea.KeyPressMsg{Code: 'w', Mod: tea.ModCtrl})
			if m.windows.Len() != 1 || m.FocusedWindow() != string(betaID) || !strings.Contains(screen(), "Windows (1)") {
				t.Fatalf("Ctrl-W below the floor:\n%s", screen())
			}
		})
	}
}

func TestOverlappingWindowsEliminateEdgeFragmentsAndKeepDiscoverability(t *testing.T) {
	firstData := sdk.View{
		Title:  "Dogcalc",
		State:  sdk.ViewReady,
		Status: "DOGCALC_STATUS_TAIL",
		Items: []sdk.Item{
			{ID: "i1", Label: "QQQ_LEFT_EDGE", Detail: strings.Repeat("Z", 120) + "ZZZ_RIGHT_EDGE"},
			{ID: "i2", Label: "WWW_LEFT_EDGE", Detail: strings.Repeat("Y", 120) + "YYY_RIGHT_EDGE"},
		},
		Actions: []sdk.Action{{ID: "eq", Label: "BOOP ="}},
	}
	secondData := sdk.View{
		Title:  "Compendium",
		State:  sdk.ViewReady,
		Status: "Active vault ready",
		Items:  []sdk.Item{{ID: "n1", Label: "Field Notes", Detail: "Clean readable body"}},
	}

	themes := []struct {
		name      string
		th        theme.Theme
		mode      ui.ColorMode
		focusedTL string
		unfocusTL string
		ascii     bool
	}{
		{"nostalgia truecolor", theme.Nostalgia(), ui.TrueColor, "┏", "┌", false},
		{"nostalgia ansi256", theme.Nostalgia(), ui.ANSI256, "┏", "┌", false},
		{"nostalgia ansi16", theme.Nostalgia(), ui.ANSI16, "┏", "┌", false},
		{"nostalgia plain", theme.Nostalgia(), ui.Plain, "┏", "┌", false},
		{"midnight truecolor", theme.Midnight(), ui.TrueColor, "┏", "┌", false},
		{"high-contrast ansi256", theme.HighContrast(), ui.ANSI256, "┏", "┌", false},
		{"high-contrast-light ansi256", theme.HighContrastLight(), ui.ANSI256, "┏", "┌", false},
		{"monochrome ansi256", theme.Monochrome(), ui.ANSI256, "+", "+", true},
		{"monochrome plain", theme.Monochrome(), ui.Plain, "+", "+", true},
	}

	for _, size := range [][2]int{{80, 24}, {100, 32}} {
		for _, tc := range themes {
			t.Run(fmt.Sprintf("%s/%s", sizeName(size), tc.name), func(t *testing.T) {
				m := sized(fixtureModel(tc.th, tc.mode), size[0], size[1])
				openTitled(m, alphaID, "Dogcalc", firstData)
				openTitled(m, betaID, "Compendium", secondData)

				content := m.View().Content
				if tc.mode == ui.Plain && strings.Contains(content, "\x1b") {
					t.Fatal("Plain windows emitted ANSI escapes")
				}
				lines := screenText(m.View())
				if tc.ascii {
					requireMonochromeChromeASCII(t, strings.Join(lines, "\n"))
				}

				body := rectOf(t, m, slotBody)
				alpha := rectOf(t, m, windowLayerID(alphaID))
				beta := rectOf(t, m, windowLayerID(betaID))
				if beta.Min.X != body.Min.X {
					t.Fatalf("focused window left edge %d != body left edge %d", beta.Min.X, body.Min.X)
				}
				if got := row(m, alpha.Min.Y, alpha.Min.X, alpha.Min.X+1); got != tc.unfocusTL {
					t.Fatalf("unfocused window corner = %q, want %q", got, tc.unfocusTL)
				}
				if got := row(m, beta.Min.Y, beta.Min.X, beta.Min.X+1); got != tc.focusedTL {
					t.Fatalf("focused window corner = %q, want %q", got, tc.focusedTL)
				}
				// Unfocused Dogcalc's title and [X] stay visible above Compendium.
				if top := row(m, alpha.Min.Y, alpha.Min.X, alpha.Max.X); !strings.Contains(top, "Dogcalc") || !strings.Contains(top, closeLabel) {
					t.Fatalf("background title bar missing title or close: %q", top)
				}
				// Every interior row of obscured Dogcalc exposed beside Compendium
				// must be blank (no clipped text fragments), even after moving
				// Compendium right and down.
				checkNoFragments := func(stage string) {
					t.Helper()
					b := rectOf(t, m, windowLayerID(betaID))
					for y := alpha.Min.Y + 1; y < alpha.Max.Y-1; y++ {
						if b.Min.X > alpha.Min.X+1 {
							left := row(m, y, alpha.Min.X+1, b.Min.X)
							if strings.TrimSpace(left) != "" {
								t.Fatalf("%s: left edge leaked %q on row %d", stage, left, y)
							}
						}
						if b.Max.X < alpha.Max.X-1 {
							right := row(m, y, b.Max.X, alpha.Max.X-1)
							if strings.TrimSpace(right) != "" {
								t.Fatalf("%s: right edge leaked %q on row %d", stage, right, y)
							}
						}
						if y < b.Min.Y || y >= b.Max.Y {
							strip := row(m, y, alpha.Min.X+1, alpha.Max.X-1)
							if strings.TrimSpace(strip) != "" {
								t.Fatalf("%s: vertical strip leaked %q on row %d", stage, strip, y)
							}
						}
					}
				}
				checkNoFragments("default")
				act(t, m, alt(tea.KeyRight))
				act(t, m, alt(tea.KeyRight))
				act(t, m, alt(tea.KeyDown))
				checkNoFragments("moved")

				// Raising Dogcalc covers Compendium in the body, yet the prompt
				// row and Windows (2) tab keep Compendium discoverable.
				act(t, m, key(tea.KeyF8))
				if m.FocusedWindow() != string(alphaID) {
					t.Fatalf("F8 did not focus Dogcalc: %q", m.FocusedWindow())
				}
				promptRect := rectOf(t, m, slotPrompt)
				promptRow := row(m, promptRect.Min.Y, promptRect.Min.X, promptRect.Max.X)
				if !strings.Contains(promptRow, "F8 next: Compendium") {
					t.Fatalf("prompt row did not surface background window: %q", promptRow)
				}
				if !strings.Contains(strings.Join(screenText(m.View()), "\n"), "Windows (2)") {
					t.Fatal("Windows (2) tab missing after F8 raise")
				}
			})
		}
	}
}

func TestUnobscuredUnfocusedWindowRendersContent(t *testing.T) {
	m, _ := mouseModel(t, 100, 32)
	openTitled(m, alphaID, "Alpha", screenData())
	openTitled(m, betaID, "Beta", padData())
	m.windows.Move(alphaID, windows.Bounds{X: 0, Y: 0, Width: 44, Height: 16})
	m.windows.Move(betaID, windows.Bounds{X: 48, Y: 0, Width: 44, Height: 16})

	alpha := rectOf(t, m, windowLayerID(alphaID))
	if got := row(m, alpha.Min.Y, alpha.Min.X, alpha.Min.X+1); got != "┌" {
		t.Fatalf("unfocused side-by-side window corner = %q, want light border", got)
	}
	if _, ok := findText(m, alpha, "One: first"); !ok {
		t.Fatalf("unobscured unfocused window did not render its content:\n%s", strings.Join(screenText(m.View()), "\n"))
	}
}
