package shell

import (
	"fmt"
	"image"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// overlayUnderlays are the six screens the palette and confirmations are
// drawn over, each with state of its own the overlay must not disturb.
func overlayUnderlays() []geometryScreen {
	return []geometryScreen{
		{"home", func(m *Model) { m.setMode(modeHome); m.homeSelected = 1 }},
		{"prompt", func(m *Model) {
			m.setMode(modePrompt)
			for i := range 30 {
				m.append(entry{fmt.Sprintf("transcript line %02d", i), "output"})
			}
			scrollTranscript(m, 3)
			m.input = []rune("echo he\u0301llo 界 world")
			m.cursor = 8
		}},
		{"launcher", func(m *Model) {
			m.setMode(modeLauncher)
			m.launcherQuery = "o"
			m.selected = 2
		}},
		{"app", func(m *Model) {
			m.showWindow(&appView{data: padData(), instance: "launch", version: 2,
				values: map[string]string{"text": "draft"}, focus: 2, cell: 1})
		}},
		{"tasks", func(m *Model) { m.taskView = true; m.tasks.SelectNext() }},
		{"notifications", func(m *Model) { m.notifView = true; m.notifs.SelectNext() }},
	}
}

// underlay is everything about a screen beneath an overlay that the overlay
// must leave exactly as it was.
type underlay struct {
	modelState
	launcherQuery, instance, values string
	notifSel                        int
	shownFocus                      FocusState
}

func underlayOf(m *Model) underlay {
	s := stateOf(m)
	s.paletteOpen, s.paletteSel, s.paletteFilter = false, 0, ""
	u := underlay{modelState: s, launcherQuery: m.launcherQuery, notifSel: m.notifs.SelectedIndex()}
	if v := m.focusedView(); v != nil {
		u.instance, u.values = v.instance, fmt.Sprint(v.values)
	}
	return u
}

// coveredKeys is input that would change or activate each underlay if it
// reached it: typing, navigation, Tab, Enter, Backspace, paging and the
// view shortcuts.
var coveredKeys = []tea.Msg{
	typed("q"), key(tea.KeyUp), key(tea.KeyDown), key(tea.KeyLeft), tabKey, key(tea.KeyBackspace),
	key(tea.KeyPgUp), key(tea.KeyF1), key(tea.KeyF2), key(tea.KeyF3), key(tea.KeyF5), key(tea.KeyF6),
	typed("x"), typed("r"), tea.PasteMsg{Content: "pasted"},
}

func TestPaletteKeepsTheScreenBeneath(t *testing.T) {
	ctrlP := tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl}
	for _, size := range [][2]int{{80, 24}, {30, 10}} {
		for _, s := range overlayUnderlays() {
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, size[0], size[1]), func(t *testing.T) {
				m, rec := recordingFixture(t)
				sized(m, size[0], size[1])
				s.setup(m)
				before, want := m.View(), underlayOf(m)
				want.shownFocus = m.Focus()

				act(t, m, ctrlP)
				if m.Focus() != FocusPalette {
					t.Fatalf("focus = %v, want the palette", m.Focus())
				}
				open := screenText(m.View())
				// At 80×24 the frame's title and tabs stay on screen around
				// the palette; at 30×10 it covers all but the frame's border.
				if size[0] == 80 && !strings.Contains(open[1], "G O S") || !strings.Contains(strings.Join(open, "\n"), "PALETTE") {
					t.Fatalf("palette does not sit over the screen:\n%s", strings.Join(open, "\n"))
				}
				if m.View().Cursor != nil {
					t.Fatal("the palette left the covered prompt's cursor visible")
				}

				for _, k := range coveredKeys {
					act(t, m, k)
				}
				for _, r := range "zzz-no-such" {
					act(t, m, typed(string(r)))
				}
				if !strings.Contains(strings.Join(screenText(m.View()), "\n"), "No matching") {
					t.Fatalf("no-match query lacks the empty state:\n%s", strings.Join(screenText(m.View()), "\n"))
				}
				act(t, m, escKey)

				got := underlayOf(m)
				got.shownFocus = m.Focus()
				if got != want {
					t.Fatalf("palette changed the screen beneath:\n got %+v\nwant %+v", got, want)
				}
				after := m.View()
				if after.Content != before.Content || !sameCursor(after.Cursor, before.Cursor) {
					t.Fatalf("Esc did not restore the screen:\n%s", strings.Join(screenText(after), "\n"))
				}
				if calls := rec.all(); len(calls) != 0 {
					t.Fatalf("filtering and dismissing sent %v", calls)
				}
			})
		}
	}
}

// Esc dismisses only the top layer: a Task Manager subview or an app under
// the palette or a dialog is still there afterwards.
func TestEscClosesOnlyTheTopLayer(t *testing.T) {
	for _, s := range []geometryScreen{
		{"tasks-logs", func(m *Model) {
			m.taskView = true
			m.tasks.ShowLogs(7, "com.gostalgia.echo", "boot\nhello")
		}},
		{"app", overlayUnderlays()[3].setup},
	} {
		for _, overlay := range []string{"palette", "dialog"} {
			t.Run(s.name+"/"+overlay, func(t *testing.T) {
				m, _ := recordingFixture(t)
				s.setup(m)
				want := underlayOf(m)
				if overlay == "palette" {
					act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
				} else {
					m.openConfirm(confirmation{kind: confirmShutdown})
				}
				act(t, m, escKey)
				if m.modalOpen() || underlayOf(m) != want {
					t.Fatalf("Esc closed more than the %s: %+v", overlay, underlayOf(m))
				}
			})
		}
	}
}

// Under a confirmation nothing reaches the screen beneath: no key, paste or
// shortcut edits or activates it, and Ctrl-C still exits.
func TestDialogIsolatesInputAndCursor(t *testing.T) {
	for _, s := range overlayUnderlays() {
		t.Run(s.name, func(t *testing.T) {
			m, rec := recordingFixture(t)
			s.setup(m)
			before, want := m.View(), underlayOf(m)
			m.openConfirm(confirmation{kind: confirmShutdown})
			if m.View().Cursor != nil {
				t.Fatal("the dialog left the covered prompt's cursor visible")
			}
			for _, k := range coveredKeys {
				act(t, m, k)
			}
			act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
			if m.Focus() != FocusDialog || m.paletteOpen {
				t.Fatalf("a key escaped the dialog: focus %v, palette %v", m.Focus(), m.paletteOpen)
			}
			if got := underlayOf(m); got != want {
				t.Fatalf("dialog let input through:\n got %+v\nwant %+v", got, want)
			}
			if calls := rec.all(); len(calls) != 0 {
				t.Fatalf("covered keys sent %v", calls)
			}
			if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
				t.Fatal("Ctrl-C under a dialog did not exit")
			} else if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("Ctrl-C under a dialog did not exit")
			}
			act(t, m, escKey)
			after := m.View()
			if after.Content != before.Content || !sameCursor(after.Cursor, before.Cursor) {
				t.Fatal("closing the dialog did not restore the screen and its cursor")
			}
		})
	}
	m, _ := recordingFixture(t)
	m.openPalette()
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("Ctrl-C under the palette did not exit")
	}
}

// Enter on an enabled palette result activates it once; a held Enter's
// repeats do not activate it again.
func TestPaletteActivatesOnce(t *testing.T) {
	m, rec := recordingFixture(t)
	m.setMode(modeHome)
	act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	act(t, m, typed("launch notes"))
	if n := len(rec.all()); n != 0 {
		t.Fatalf("filtering sent %d requests", n)
	}
	act(t, m, enterKey)
	act(t, m, repeatEnter)
	launches := 0
	for _, c := range rec.all() {
		if c.method == "app/launch" {
			launches++
		}
	}
	if launches != 1 || m.paletteOpen {
		t.Fatalf("launch requests = %d (palette open %v): %v", launches, m.paletteOpen, rec.all())
	}
}

var (
	sgrRE       = regexp.MustCompile("\x1b\\[([0-9;:]*)m")
	truecolorRE = regexp.MustCompile("[34]8[;:]2[;:]")
)

// basicSGR reports whether every SGR in s uses only attributes and the
// basic black and white colors.
func basicSGR(s string) bool {
	for _, m := range sgrRE.FindAllStringSubmatch(s, -1) {
		params := strings.Split(m[1], ";")
		for i := 0; i < len(params); i++ {
			switch p := params[i]; p {
			case "38", "48", "58":
				return false
			case "30", "37", "40", "47", "90", "97", "100", "107", "39", "49":
			default:
				if len(p) == 2 && (p[0] == '3' || p[0] == '4' || p[0] == '9') {
					return false
				}
				if len(p) == 3 && p[0] == '1' && p[1] == '0' {
					return false
				}
			}
		}
	}
	return true
}

// openOverlay opens the palette or a confirmation over m, settled: these
// tests assert the static overlay composition, not its entrance frames.
func openOverlay(m *Model, kind string) {
	if kind == "palette" {
		m.openPalette()
	} else {
		m.openConfirm(confirmation{kind: confirmShutdown})
	}
	settleFiniteMotion(m)
}

// overlayRect is the box an overlay draws on the screen.
func overlayRect(t *testing.T, m *Model, kind string) image.Rectangle {
	t.Helper()
	id := layerPalette
	if kind == "dialog" {
		id = layerDialog
	}
	r, ok := m.solve().Rect(id)
	if !ok || r.Empty() {
		t.Fatalf("no %s layer in the frame", kind)
	}
	return r
}

// The backdrop dims the screen beneath a modal layer only in color themes
// at 256 colors or more: Plain, ANSI16 and Monochrome get no backdrop, Plain
// no escapes at all, and ANSI256 stays in its palette even where the dim
// blends colors.
func TestOverlayBackdropFollowsThemeAndProfile(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI16, ui.ANSI256, ui.TrueColor} {
			for _, kind := range []string{"palette", "dialog"} {
				t.Run(fmt.Sprintf("%s/%d/%s", th.Name, mode, kind), func(t *testing.T) {
					m := sized(fixtureModel(th, mode), 80, 24)
					m.setMode(modeHome)
					closed := strings.Split(m.View().Content, "\n")
					openOverlay(m, kind)
					content := m.View().Content
					open := strings.Split(content, "\n")
					box := overlayRect(t, m, kind)
					// Row 0 is the frame's top border and title, outside every layer.
					dimmed := open[0] != closed[0]
					if ansi.Strip(open[0]) != ansi.Strip(closed[0]) {
						t.Fatalf("the backdrop changed text beneath it: %q", ansi.Strip(open[0]))
					}
					wantDim := (mode == ui.ANSI256 || mode == ui.TrueColor) && th.Name != "monochrome"
					if dimmed != wantDim {
						t.Fatalf("underlay dimmed = %v, want %v", dimmed, wantDim)
					}
					switch {
					case mode == ui.Plain && strings.Contains(content, "\x1b"):
						t.Fatalf("plain overlay has escapes: %q", content)
					case mode == ui.ANSI256 && truecolorRE.MatchString(content):
						t.Fatal("ANSI256 overlay leaks truecolor")
					case mode == ui.ANSI16 && (truecolorRE.MatchString(content) || strings.Contains(content, "8;5;")):
						t.Fatal("ANSI16 overlay leaves the 16 colors")
					case th.Name == "monochrome" && !basicSGR(content):
						t.Fatalf("monochrome overlay uses more than black and white: %q", content)
					}
					if box.Min.Y < 1 || box.Max.Y > 23 {
						t.Fatalf("%s box %v covers the frame title or help bar", kind, box)
					}
				})
			}
		}
	}
}

// Names and targets from the environment reach the dialog only through
// ui.Sanitize.
func TestDialogSanitizesExternalText(t *testing.T) {
	m, _ := recordingFixture(t)
	evil := "Ev\x1b]52;c;aGk=\x07il\x1b[31m\u202e\x00"
	m.apps[2].Manifest.Name = evil
	m.apps[2].Manifest.ID = "com.\x1b[2Jevil"
	m.setMode(modeLauncher)
	m.selected = 2
	act(t, m, key(tea.KeyF3))
	content := m.View().Content
	if strings.Contains(content, "\x1b") || strings.Contains(content, "\x07") || strings.Contains(content, "\x00") {
		t.Fatalf("dialog let control text through: %q", content)
	}
	if !strings.Contains(dialogWords(m), "Evil") || !strings.Contains(dialogWords(m), "com.evil") {
		t.Fatalf("dialog lost the sanitized target:\n%s", dialogText(m))
	}
}

// bodyOutside is the body's rendered rows with r blanked out: everything a
// layer at r leaves visible.
func bodyOutside(lines []string, body, r image.Rectangle) []string {
	var out []string
	for y := body.Min.Y; y < body.Max.Y; y++ {
		cells := []rune(lines[y])
		row := string(cells[body.Min.X:body.Max.X])
		if y >= r.Min.Y && y < r.Max.Y {
			row = ansi.Truncate(row, r.Min.X-body.Min.X, "")
		}
		out = append(out, row)
	}
	return out
}

// Toasts are a layer over the body: appearing, stacking, flooding and
// expiring changes only the layer, never the body's rectangle, the
// transcript's scroll, its visible lines, the draft or focus.
func TestToastsOverlayWithoutMovingTheTranscript(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {30, 10}} {
		for _, scroll := range []int{0, 7} {
			t.Run(fmt.Sprintf("%dx%d/scroll%d", size[0], size[1], scroll), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), size[0], size[1])
				m.setMode(modePrompt)
				for i := range 40 {
					m.append(entry{fmt.Sprintf("line %02d", i), "output"})
				}
				scrollTranscript(m, scroll)
				m.input, m.cursor = []rune("draft"), 3
				body := checkFrame(t, m)[slotBody]
				base := screenText(m.View())
				want := stateOf(m)
				for _, step := range []struct {
					name   string
					change func()
					toasts int
				}{
					{"one toast", func() { withToasts(m, 1) }, 1},
					{"three toasts", func() { withToasts(m, 2) }, 3},
					{"flood", func() { withToasts(m, 3) }, 3},
					{"half expired", func() {
						for range 4 {
							m.Update(tickMsg(fixtureTime))
						}
					}, 3},
					{"expired", func() {
						for range 4 {
							m.Update(tickMsg(fixtureTime))
						}
					}, 0},
				} {
					step.change()
					settleFiniteMotion(m)
					if n := len(m.notifs.ActiveToasts()); n != step.toasts {
						t.Fatalf("%s: %d active toasts, want %d", step.name, n, step.toasts)
					}
					if got := checkFrame(t, m)[slotBody]; got != body {
						t.Fatalf("%s: body %v, was %v", step.name, got, body)
					}
					lines := screenText(m.View())
					stack, ok := m.solve().Rect(layerToasts)
					if step.toasts == 0 {
						if ok {
							t.Fatalf("%s: toast layer %v with no toasts", step.name, stack)
						}
						stack = image.Rectangle{}
					} else {
						if !ok || !stack.In(body) || stack.Max != body.Max || stack.Dy() > max(1, body.Dy()/3) {
							t.Fatalf("%s: toast stack %v not bounded at the bottom right of %v", step.name, stack, body)
						}
						if !strings.Contains(strings.Join(lines[stack.Min.Y:stack.Max.Y], "\n"), "ERROR") {
							t.Fatalf("%s: toast stack not drawn:\n%s", step.name, strings.Join(lines, "\n"))
						}
					}
					if got, was := bodyOutside(lines, body, stack), bodyOutside(base, body, stack); strings.Join(got, "\n") != strings.Join(was, "\n") {
						t.Fatalf("%s: the transcript moved:\n%s\nwas\n%s", step.name, strings.Join(got, "\n"), strings.Join(was, "\n"))
					}
					if lines[body.Max.Y+1] != base[body.Max.Y+1] || lines[len(lines)-1] != base[len(base)-1] {
						t.Fatalf("%s: toasts changed the prompt or help row", step.name)
					}
					if got := stateOf(m); got != want || m.Focus() != FocusPrompt {
						t.Fatalf("%s: toasts changed state:\n got %+v\nwant %+v", step.name, got, want)
					}
				}
			})
		}
	}
}

// toastLife is the manager's active toasts as IDs and remaining lifetimes.
func toastLife(n *notifications.Manager) string {
	var out []string
	for _, t := range n.ActiveToasts() {
		out = append(out, fmt.Sprintf("%s:%d", t.ID, t.TicksLeft))
	}
	return strings.Join(out, " ")
}

// The notification manager alone decides when toasts expire, how many show
// and what DND suppresses; drawing them, and opening and closing other
// layers, changes none of it.
func TestToastLifetimeBelongsToTheManager(t *testing.T) {
	for _, cfg := range []struct {
		name                 string
		history, active, ttl int
	}{
		{"defaults", notifications.DefaultMaxHistory, notifications.DefaultMaxActiveToasts, notifications.DefaultToastTicks},
		{"configured", 5, 2, 3},
	} {
		t.Run(cfg.name, func(t *testing.T) {
			notifs := notifications.NewManager(cfg.history, cfg.active, cfg.ttl)
			m := sized(NewWithThemeAndNotifs(t.Context(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain, notifs), 80, 24)
			flood := cfg.active + 2
			withToasts(m, flood)
			if n := len(notifs.ActiveToasts()); n != cfg.active || notifs.DroppedToasts() != 2 {
				t.Fatalf("active %d dropped %d, want %d and 2", n, notifs.DroppedToasts(), cfg.active)
			}
			if got := notifs.ActiveToasts()[0].ID; got != fmt.Sprintf("toast-%d", flood-cfg.active) {
				t.Fatalf("flood kept %s, not the newest", got)
			}
			for tick := 1; tick <= cfg.ttl; tick++ {
				life := toastLife(notifs)
				// Rendering, and the palette and a dialog opening and
				// closing, leave every countdown where it was.
				for range 3 {
					m.View()
				}
				m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
				m.View()
				m.Update(escKey)
				m.openConfirm(confirmation{kind: confirmShutdown})
				m.View()
				m.Update(escKey)
				m.View()
				if got := toastLife(notifs); got != life {
					t.Fatalf("tick %d: drawing changed toasts from %q to %q", tick, life, got)
				}
				m.Update(tickMsg(fixtureTime))
				if left := len(notifs.ActiveToasts()); (tick < cfg.ttl) != (left == cfg.active) {
					t.Fatalf("after %d of %d ticks %d toasts remain", tick, cfg.ttl, left)
				}
			}
			for i := range cfg.history + 3 {
				withToasts(m, 1)
				_ = i
			}
			if n := len(notifs.History()); n != cfg.history {
				t.Fatalf("history holds %d, want the cap %d", n, cfg.history)
			}

			// DND clears popups at once, keeps new events as unread history
			// without popups, and never replays them when it ends.
			notifs.SetDND(true)
			if n := len(notifs.ActiveToasts()); n != 0 {
				t.Fatalf("DND left %d toasts", n)
			}
			unseen := notifs.UnseenCount()
			notifs.Record(notifications.Notification{ID: "quiet", Title: "quiet", Level: notifications.LevelError})
			if notifs.UnseenCount() != min(cfg.history, unseen+1) || len(notifs.ActiveToasts()) != 0 {
				t.Fatal("DND event did not stay quiet unread history")
			}
			if _, ok := m.solve().Rect(layerToasts); ok {
				t.Fatal("DND frame still draws a toast layer")
			}
			notifs.SetDND(false)
			if n := len(notifs.ActiveToasts()); n != 0 {
				t.Fatalf("leaving DND replayed %d toasts", n)
			}
		})
	}
}

// overlayCase is one of the 60 reviewed overlay compositions.
type overlayCase struct {
	theme    theme.Theme
	underlay geometryScreen
	kind     string
}

// overlayDialog is the confirmation each underlay shows in its golden: the
// one its own keys open where it has one, otherwise shutdown composed over
// it directly.
func overlayDialog(t *testing.T, m *Model, underlay string) {
	switch underlay {
	case "launcher":
		act(t, m, key(tea.KeyF3))
	case "tasks":
		act(t, m, typed("x"))
	case "prompt":
		m.input = []rune("stop com.gostalgia.echo")
		m.cursor = len(m.input)
		act(t, m, enterKey)
	default:
		m.openConfirm(confirmation{kind: confirmShutdown})
		settleFiniteMotion(m)
	}
}

func TestOverlaySnapshots(t *testing.T) {
	var cases []overlayCase
	for _, th := range builtinThemes() {
		for _, u := range overlayUnderlays() {
			for _, kind := range []string{"palette", "dialog"} {
				cases = append(cases, overlayCase{th, u, kind})
			}
		}
	}
	if len(cases) != 60 {
		t.Fatalf("%d overlay snapshots, want 60", len(cases))
	}
	for _, c := range cases {
		name := fmt.Sprintf("%s-%s-%s", c.theme.Name, c.underlay.name, c.kind)
		t.Run(name, func(t *testing.T) {
			mode := ui.ANSI256
			if c.theme.Name == "monochrome" {
				mode = ui.Plain
			}
			m := sized(fixtureModel(c.theme, mode), 80, 24)
			m.client = &recordingCaller{}
			c.underlay.setup(m)
			if c.kind == "palette" {
				act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
			} else {
				overlayDialog(t, m, c.underlay.name)
			}
			if !m.modalOpen() {
				t.Fatalf("no %s open", c.kind)
			}
			content := m.View().Content
			if mode == ui.Plain && strings.Contains(content, "\x1b") {
				t.Fatal("plain snapshot has escapes")
			}
			if mode == ui.ANSI256 && truecolorRE.MatchString(content) {
				t.Fatal("ANSI256 snapshot leaks truecolor")
			}
			lines := strings.Split(ansi.Strip(content), "\n")
			want := "[Cancel]"
			if c.kind == "palette" {
				want = "COMMAND PALETTE"
			}
			if !strings.Contains(ansi.Strip(content), want) || !strings.Contains(lines[2], "Home") {
				t.Fatalf("overlay or underlay missing:\n%s", ansi.Strip(content))
			}
			rendered := strings.Split(content, "\n")
			for i := range rendered {
				rendered[i] = strings.TrimRight(rendered[i], " ")
			}
			got := strings.Join(rendered, "\n") + "\n"
			path := filepath.Join("testdata", "overlay", name+".golden")
			if *updateVisual {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(golden) {
				t.Fatalf("overlay snapshot %s changed:\n%s", path, ansi.Strip(got))
			}
		})
	}
}

// A toast lands in the same corner over every screen and leaves the rest
// of the screen, its state and its focus as they were. The Notification
// Center lists the same events itself and shows no toasts.
func TestToastsOverlayEveryScreen(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {30, 10}} {
		for _, s := range overlayUnderlays() {
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), size[0], size[1])
				s.setup(m)
				body := checkFrame(t, m)[slotBody]
				base, want, focus := screenText(m.View()), underlayOf(m), m.Focus()
				withToasts(m, 1)
				stack, ok := m.solve().Rect(layerToasts)
				if s.name == "notifications" {
					if ok {
						t.Fatalf("toast layer %v over the Notification Center", stack)
					}
					return
				}
				if !ok || !stack.In(body) || stack.Max != body.Max {
					t.Fatalf("toast %v (ok %v) not at the bottom right of %v", stack, ok, body)
				}
				lines := screenText(m.View())
				if !strings.Contains(strings.Join(lines[stack.Min.Y:stack.Max.Y], "\n"), "ERROR") {
					t.Fatalf("toast not drawn:\n%s", strings.Join(lines, "\n"))
				}
				if got, was := bodyOutside(lines, body, stack), bodyOutside(base, body, stack); strings.Join(got, "\n") != strings.Join(was, "\n") {
					t.Fatalf("the screen moved:\n%s\nwas\n%s", strings.Join(got, "\n"), strings.Join(was, "\n"))
				}
				if got := underlayOf(m); got != want || m.Focus() != focus {
					t.Fatalf("the toast changed the screen's state:\n got %+v\nwant %+v", got, want)
				}
			})
		}
	}
}

// Up and Down move the palette's selection, and Enter runs the selected
// result, not the first.
func TestPaletteArrowsChooseTheResult(t *testing.T) {
	m, rec := recordingFixture(t)
	m.setMode(modeHome)
	act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	act(t, m, typed("launch"))
	act(t, m, key(tea.KeyDown))
	act(t, m, key(tea.KeyDown))
	act(t, m, key(tea.KeyUp))
	items := m.filteredPaletteItems()
	if m.paletteSelected != 1 || len(items) < 3 || items[1].Title != "Launch Notes" {
		t.Fatalf("selection %d over %d results", m.paletteSelected, len(items))
	}
	marker := m.kit.Theme().Focus.Marker
	if !strings.Contains(lineWith(strings.Join(screenText(m.View()), "\n"), "Launch Notes"), marker) {
		t.Fatalf("the selected result is not marked:\n%s", strings.Join(screenText(m.View()), "\n"))
	}
	act(t, m, enterKey)
	var launches []ipcCall
	for _, c := range rec.all() {
		if c.method == "app/launch" {
			launches = append(launches, c)
		}
	}
	if len(launches) != 1 || launches[0].params != `{"id":"com.gostalgia.notes"}` {
		t.Fatalf("Enter sent %v, want one launch of Notes", rec.all())
	}
}
