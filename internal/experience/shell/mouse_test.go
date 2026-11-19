package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"bloomfx.dev/bloom"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

// mouseModel is the populated fixture shell at w×h, talking to a
// recording caller.
func mouseModel(t *testing.T, w, h int) (*Model, *recordingCaller) {
	t.Helper()
	rec := &recordingCaller{}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), w, h)
	m.client = rec
	return m, rec
}

// mouse sends msg through the shell's Update on the frame View draws
// first, as Bubble Tea draws a frame before the next event, and runs the
// commands it returns, feeding their messages back. It returns the type of
// every message the commands sent.
func mouse(t *testing.T, m *Model, msg tea.Msg) []string {
	t.Helper()
	m.View()
	_, cmd := m.Update(msg)
	return drive(t, m, cmd)
}

// drive runs cmd as Bubble Tea would, feeding its messages back through
// Update, and returns the type of each message.
func drive(t *testing.T, m *Model, cmd tea.Cmd) []string {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	var sent []string
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			sent = append(sent, drive(t, m, c)...)
		}
	default:
		sent = append(sent, fmt.Sprintf("%T", msg))
		_, next := m.Update(msg)
		// Copies and held clicks answer with messages; process polls and
		// refreshes scheduled by results are not run.
		switch msg.(type) {
		case bloom.SelectionMsg, copyMsg:
			sent = append(sent, drive(t, m, next)...)
		default:
			if strings.HasPrefix(fmt.Sprintf("%T", msg), "bloom.") {
				sent = append(sent, drive(t, m, next)...)
			}
		}
	}
	return sent
}

func clickAt(t *testing.T, m *Model, p image.Point) []string {
	t.Helper()
	sent := mouse(t, m, tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
	return append(sent, mouse(t, m, tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})...)
}

func wheelAt(t *testing.T, m *Model, p image.Point, steps int) {
	t.Helper()
	button := tea.MouseWheelDown
	if steps < 0 {
		button = tea.MouseWheelUp
	}
	for range abs(steps) {
		mouse(t, m, tea.MouseWheelMsg{X: p.X, Y: p.Y, Button: button})
	}
}

func rectOf[K comparable](t *testing.T, m *Model, id K) image.Rectangle {
	t.Helper()
	r, ok := m.solve().Rect(id)
	if !ok {
		t.Fatalf("no %v in the frame", id)
	}
	return r
}

// findText is the cell where s starts on screen within r, the first row
// down that shows it.
func findText(m *Model, r image.Rectangle, s string) (image.Point, bool) {
	lines := screenText(m.View())
	for y := r.Min.Y; y < r.Max.Y && y < len(lines); y++ {
		if i := strings.Index(lines[y], s); i >= 0 {
			if x := ansi.StringWidth(lines[y][:i]); x >= r.Min.X && x < r.Max.X {
				return image.Pt(x, y), true
			}
		}
	}
	return image.Point{}, false
}

func mustFind(t *testing.T, m *Model, r image.Rectangle, s string) image.Point {
	t.Helper()
	p, ok := findText(m, r, s)
	if !ok {
		t.Fatalf("%q not on screen in %v:\n%s", s, r, strings.Join(screenText(m.View()), "\n"))
	}
	return p
}

func calls(rec *recordingCaller, method string) []ipcCall {
	var out []ipcCall
	for _, c := range rec.all() {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// requests is what rec recorded, but for the workspace saves a finished
// command sends from a goroutine of its own, which may or may not have run
// yet.
func requests(rec *recordingCaller) string {
	var out []ipcCall
	for _, c := range rec.all() {
		if c.method != "session/workspace/set" {
			out = append(out, c)
		}
	}
	return fmt.Sprint(out)
}

// sameOutcome fails unless the mouse and keyboard twins ended in the same
// state having sent the same requests.
func sameOutcome(t *testing.T, byMouse, byKey *Model, mouseRec, keyRec *recordingCaller) {
	t.Helper()
	if got, want := stateOf(byMouse), stateOf(byKey); got != want {
		t.Fatalf("mouse and keyboard differ:\nmouse %+v\n  key %+v", got, want)
	}
	if got, want := requests(mouseRec), requests(keyRec); got != want {
		t.Fatalf("mouse sent %s, keyboard %s", got, want)
	}
}

// copied is the text of each clipboard write rec recorded, which must all
// be to the host clipboard.
func copied(t *testing.T, rec *recordingCaller) []string {
	t.Helper()
	var out []string
	for _, c := range calls(rec, "clipboard/write") {
		var p struct{ Text, Target string }
		if err := json.Unmarshal([]byte(c.params), &p); err != nil {
			t.Fatal(err)
		}
		if p.Target != "host" {
			t.Fatalf("a copy to %q, not the host clipboard", p.Target)
		}
		out = append(out, p.Text)
	}
	return out
}

func sizeName(size [2]int) string { return fmt.Sprintf("%dx%d", size[0], size[1]) }

func TestMouseIsOnByDefaultAndTheCommandTurnsItOff(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	if v := m.View(); v.MouseMode == tea.MouseModeNone || v.OnMouse == nil {
		t.Fatalf("a new shell asks for mouse mode %v, want mouse input on", v.MouseMode)
	}
	run := func(line string) {
		t.Helper()
		m.setMode(modePrompt)
		act(t, m, typed(line))
		act(t, m, enterKey)
	}
	run("mouse off")
	if v := m.View(); v.MouseMode != tea.MouseModeNone || v.OnMouse != nil {
		t.Fatalf("mouse off asks for mouse mode %v", v.MouseMode)
	}
	if !strings.Contains(bodyText(t, m), "Mouse is off") {
		t.Fatal("mouse off was not confirmed")
	}
	// Reports still in flight from the terminal do nothing.
	tabs := rectOf(t, m, slotTabs)
	before := stateOf(m)
	clickAt(t, m, mustFind(t, m, tabs, "Apps"))
	if stateOf(m) != before {
		t.Fatal("a click acted with the mouse off")
	}
	run("mouse")
	if !strings.Contains(bodyText(t, m), "Mouse is off") {
		t.Fatal("mouse did not report the state")
	}
	run("mouse on")
	if v := m.View(); v.MouseMode == tea.MouseModeNone || v.OnMouse == nil {
		t.Fatal("mouse on did not ask for mouse input")
	}
	run("mouse sideways")
	if !strings.Contains(bodyText(t, m), "usage: mouse [on|off]") {
		t.Fatal("a bad argument was not rejected")
	}
	if len(calls(rec, "clipboard/write")) != 0 {
		t.Fatal("the mouse command copied something")
	}
}

func TestMouseConfigSetsTheDefaultWithoutOverridingTheCommand(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	poll := func(eff map[string]any) {
		t.Helper()
		m.Update(m.parseConfigMsg(eff))
	}
	mouseOn := func() bool { return m.View().MouseMode != tea.MouseModeNone }
	set := func(v any) map[string]any {
		return map[string]any{"accessibility": map[string]any{"mouse": v}}
	}
	poll(map[string]any{"theme": "nostalgia"})
	if !mouseOn() {
		t.Fatal("a config without accessibility.mouse turned the mouse off")
	}
	poll(set(false))
	if mouseOn() {
		t.Fatal("accessibility.mouse false left the mouse on")
	}
	poll(set(false))
	if mouseOn() {
		t.Fatal("a repeated poll turned the mouse on")
	}
	m.setMouse(true) // mouse on, for this shell
	for range 3 {
		poll(set(false))
		poll(nil) // a failed read
	}
	if !mouseOn() {
		t.Fatal("a poll repeating the config overrode mouse on")
	}
	poll(set(true))
	poll(set(false))
	if mouseOn() {
		t.Fatal("changing the config to false did not turn the mouse off")
	}
	poll(map[string]any{"accessibility": map[string]any{}})
	if !mouseOn() {
		t.Fatal("removing the setting did not return to the default, on")
	}
	poll(set("off"))
	if !mouseOn() {
		t.Fatal("a setting that is not a boolean turned the mouse off")
	}
}

func TestMouseTabsSwitchScreensAsTheirKeysDo(t *testing.T) {
	starts := []geometryScreen{
		{"home", func(m *Model) { m.setMode(modeHome) }},
		{"prompt", func(m *Model) { m.setMode(modePrompt) }},
		{"launcher", func(m *Model) { m.setMode(modeLauncher) }},
		{"tasks", func(m *Model) { m.taskView = true }},
		{"notifications", func(m *Model) { m.notifView = true }},
	}
	keys := map[string]tea.KeyPressMsg{
		"Home": key(tea.KeyF1), "Apps": key(tea.KeyF2), "Prompt": escKey,
		"Tasks": key(tea.KeyF5), "Notifications": key(tea.KeyF6),
	}
	screens := map[string]bodyScreen{
		"Home": bodyHome, "Apps": bodyLauncher, "Prompt": bodyTranscript,
		"Tasks": bodyTasks, "Notifications": bodyNotifications,
	}
	for _, size := range geometrySizes {
		for _, s := range starts {
			for _, label := range []string{"Home", "Apps", "Prompt", "Tasks", "Notifications"} {
				t.Run(sizeName(size)+"/"+s.name+"/"+label, func(t *testing.T) {
					byMouse, mouseRec := mouseModel(t, size[0], size[1])
					byKey, keyRec := mouseModel(t, size[0], size[1])
					s.setup(byMouse)
					s.setup(byKey)
					tabs, active := byMouse.tabs()
					if tabs[active].Label == label || strings.HasPrefix(tabs[active].Label, label+" ") {
						t.Skip("the active tab")
					}
					p, ok := findText(byMouse, rectOf(t, byMouse, slotTabs), label)
					if !ok {
						t.Skip("the tab is dropped at this width")
					}
					clickAt(t, byMouse, p)
					if got := byMouse.bodyScreen(); got != screens[label] {
						t.Fatalf("the %s tab showed screen %v", label, got)
					}
					// The key goes back from the Task Manager or the
					// Notification Center to the screen beneath it first
					// when that is the key's own; the tab names its screen.
					for range 2 {
						if byKey.bodyScreen() != screens[label] {
							act(t, byKey, keys[label])
						}
					}
					sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
				})
			}
		}
	}
}

func TestMouseTabsOfAnAppViewAndTheirGaps(t *testing.T) {
	m, _ := mouseModel(t, 80, 24)
	m.setMode(modeHome)
	m.presentation = &appView{id: "com.test.screen", data: padData(), instance: "launch", version: 2,
		values: map[string]string{}}
	tabs := rectOf(t, m, slotTabs)
	home := mustFind(t, m, tabs, "Home")
	apps := mustFind(t, m, tabs, "Apps")
	before := stateOf(m)
	// The cell between two tabs, the active tab and the row past the last
	// tab are not targets.
	gap := image.Pt((home.X+len("Home")+apps.X-1)/2, home.Y)
	if line := []rune(screenText(m.View())[gap.Y]); line[gap.X] != ' ' || line[gap.X-1] != ' ' || line[gap.X+1] != ' ' {
		t.Fatalf("no gap between Home and Apps: %q", line)
	}
	for _, p := range []image.Point{gap, mustFind(t, m, tabs, "Pad"), {X: tabs.Max.X - 1, Y: tabs.Min.Y}} {
		clickAt(t, m, p)
		if stateOf(m) != before || m.presentation == nil {
			t.Fatalf("a click at %v acted", p)
		}
	}
	// Home's tab shows Home: the click names the screen, unlike F1, which
	// toggles from the screen beneath the app view.
	clickAt(t, m, image.Pt(home.X+1, home.Y))
	if m.presentation != nil || m.bodyScreen() != bodyHome {
		t.Fatalf("Home's tab showed %v", m.bodyScreen())
	}
}

// launcherFrom is the fixture launcher at size, filtered by query, with
// app j selected, and where card i is on screen, if it is.
func launcherFrom(t *testing.T, size [2]int, query string, i, j int) (*Model, *recordingCaller, image.Rectangle, bool) {
	t.Helper()
	m, rec := mouseModel(t, size[0], size[1])
	m.setMode(modeLauncher)
	m.launcherQuery, m.selected = query, j
	r, ok := m.solve().Rect(launcherCard(m.filteredApps()[i].Manifest.ID))
	return m, rec, r, ok
}

func TestMouseLauncherCardsSelectOnClickAndLaunchOnDoubleClick(t *testing.T) {
	for _, size := range geometrySizes {
		for _, query := range []string{"", "o"} {
			probe, _ := mouseModel(t, size[0], size[1])
			probe.launcherQuery = query
			n := len(probe.filteredApps())
			for i := range n {
				for j := range n {
					t.Run(fmt.Sprintf("%s/%q/card-%d/from-%d", sizeName(size), query, i, j), func(t *testing.T) {
						if _, _, _, ok := launcherFrom(t, size, query, i, j); !ok {
							t.Skip("the card is not on screen")
						}
						id := probe.filteredApps()[i].Manifest.ID
						// A single click on any cell of the card selects
						// it, as the arrow keys do, and launches nothing.
						for _, corner := range []func(image.Rectangle) image.Point{
							func(r image.Rectangle) image.Point { return r.Min },
							func(r image.Rectangle) image.Point { return r.Max.Sub(image.Pt(1, 1)) },
						} {
							m, rec, r, _ := launcherFrom(t, size, query, i, j)
							clickAt(t, m, corner(r))
							if m.selected != i || len(rec.all()) != 0 {
								t.Fatalf("clicking %s at %v selected %d and sent %v", id, corner(r), m.selected, rec.all())
							}
						}
						// A double click launches it once, from wherever
						// the selection was: the card stays under the
						// pointer between the clicks.
						m, rec, r, _ := launcherFrom(t, size, query, i, j)
						clickAt(t, m, r.Min)
						clickAt(t, m, r.Min)
						got := calls(rec, "app/launch")
						if len(got) != 1 || got[0].params != fmt.Sprintf(`{"id":%q}`, id) {
							t.Fatalf("double click launched %v, want %s once", got, id)
						}
						// Enter launches the same.
						byKey, keyRec := mouseModel(t, size[0], size[1])
						byKey.setMode(modeLauncher)
						byKey.launcherQuery, byKey.selected = query, i
						act(t, byKey, enterKey)
						if got, want := fmt.Sprint(calls(rec, "app/launch")), fmt.Sprint(calls(keyRec, "app/launch")); got != want {
							t.Fatalf("double click launched %s, Enter %s", got, want)
						}
					})
				}
			}
		}
	}
}

func TestMouseLauncherTwoCardsAndGates(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modeLauncher)
	echo := rectOf(t, m, launcherCard("com.gostalgia.echo"))
	notes := rectOf(t, m, launcherCard("com.gostalgia.notes"))
	// A click on each of two cards in quick succession is no double click.
	clickAt(t, m, echo.Min)
	clickAt(t, m, notes.Min)
	if n := len(calls(rec, "app/launch")); n != 0 || m.selected != 1 {
		t.Fatalf("two cards clicked: %d launches, selected %d", n, m.selected)
	}
	// A triple click launches once.
	clickAt(t, m, rectOf(t, m, slotPrompt).Min)
	for range 3 {
		clickAt(t, m, notes.Min)
	}
	if got := calls(rec, "app/launch"); len(got) != 1 || got[0].params != `{"id":"com.gostalgia.notes"}` {
		t.Fatalf("triple click launched %v", got)
	}
	// While a command runs, neither the card nor Enter does anything.
	m.busy = true
	for range 2 {
		clickAt(t, m, echo.Min)
	}
	act(t, m, enterKey)
	if n := len(calls(rec, "app/launch")); n != 1 || m.selected != 1 {
		t.Fatalf("busy: %d launches, selected %d", n, m.selected)
	}
	m.busy = false
	// The search line and the description are not cards.
	before := stateOf(m)
	for _, id := range []any{launcherDescription, launcherGrants} {
		r, ok := m.solve().Rect(id)
		if !ok {
			t.Fatalf("no %v", id)
		}
		clickAt(t, m, r.Min)
		clickAt(t, m, r.Min)
	}
	search := rectOf(t, m, slotBody).Min
	clickAt(t, m, search)
	clickAt(t, m, search)
	if stateOf(m) != before || len(calls(rec, "app/launch")) != 1 {
		t.Fatal("a click off the cards acted")
	}
	// Half a double click, then the palette opens over the card: the second
	// click lands on the palette's backdrop, not the card.
	clickAt(t, m, echo.Min)
	m.openPalette()
	clickAt(t, m, image.Pt(echo.Min.X, echo.Min.Y))
	if !m.paletteOpen || len(calls(rec, "app/launch")) != 1 {
		t.Fatal("a click went through the palette to the card")
	}
}

func TestMouseHomeItemsOpenAsEnterDoes(t *testing.T) {
	for _, size := range geometrySizes {
		for i, label := range []string{"Echo", "Clock", "readme.txt", "todo.txt"} {
			t.Run(sizeName(size)+"/"+label, func(t *testing.T) {
				byMouse, mouseRec := mouseModel(t, size[0], size[1])
				byKey, keyRec := mouseModel(t, size[0], size[1])
				byMouse.setMode(modeHome)
				byKey.setMode(modeHome)
				body := rectOf(t, byMouse, slotBody)
				p, ok := findText(byMouse, body, label)
				if !ok {
					t.Skip("not on screen at this size")
				}
				clickAt(t, byMouse, p)
				for range i {
					act(t, byKey, key(tea.KeyDown))
				}
				act(t, byKey, enterKey)
				sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
				if len(mouseRec.all()) == 0 {
					t.Fatal("the click opened nothing")
				}
			})
		}
	}
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modeHome)
	before := stateOf(m)
	body := rectOf(t, m, slotBody)
	for _, heading := range []string{"SYSTEM ACTIVITY", "RUNNING APPLICATIONS", "DOCUMENT SHORTCUTS", "Uptime"} {
		clickAt(t, m, mustFind(t, m, body, heading))
	}
	if stateOf(m) != before || len(rec.all()) != 0 {
		t.Fatal("a click on a heading acted")
	}
}

// paletteRows is the screen row of each item row the palette shows, top
// to bottom.
func paletteRows(t *testing.T, m *Model) []int {
	t.Helper()
	box := rectOf(t, m, layerPalette)
	tag := regexp.MustCompile(`^. \[[A-Z]+\] `)
	var rows []int
	for y, line := range screenText(m.View()) {
		if y <= box.Min.Y || y >= box.Max.Y-1 {
			continue
		}
		// The row inside the border and the selection marker's column.
		cells := []rune(line)
		if inside := string(cells[box.Min.X+2 : box.Max.X-1]); tag.MatchString(inside) {
			rows = append(rows, y)
		}
	}
	return rows
}

func TestMousePaletteItemsRunAsEnterDoes(t *testing.T) {
	for _, size := range geometrySizes {
		// From the top of the list, and scrolled to its last item.
		for _, down := range []int{0, -1} {
			probe, _ := mouseModel(t, size[0], size[1])
			probe.openPalette()
			n := len(probe.filteredPaletteItems())
			if down < 0 {
				down = n - 1
				probe.paletteSelected = down
			}
			for k := range paletteRows(t, probe) {
				t.Run(fmt.Sprintf("%s/from-%d/row-%d", sizeName(size), down, k), func(t *testing.T) {
					byMouse, mouseRec := mouseModel(t, size[0], size[1])
					byKey, keyRec := mouseModel(t, size[0], size[1])
					for _, m := range []*Model{byMouse, byKey} {
						act(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
						for range down {
							act(t, m, key(tea.KeyDown))
						}
					}
					rows := paletteRows(t, byMouse)
					// The item on row k is the one the keyboard reaches
					// from the selected row by as many rows.
					sel := slices.IndexFunc(rows, func(y int) bool {
						return strings.Contains(screenText(byMouse.View())[y], byMouse.kit.Theme().Focus.Marker+" [")
					})
					if sel < 0 {
						t.Fatal("no row is selected")
					}
					box := rectOf(t, byMouse, layerPalette)
					clickAt(t, byMouse, image.Pt(box.Max.X-3, rows[k]))
					for range abs(k - sel) {
						if k < sel {
							act(t, byKey, key(tea.KeyUp))
						} else {
							act(t, byKey, key(tea.KeyDown))
						}
					}
					act(t, byKey, enterKey)
					sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
					if byMouse.paletteOpen {
						t.Fatal("the click ran nothing")
					}
				})
			}
		}
	}
}

func TestMousePaletteOwnsTheMouse(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	m.openPalette()
	box := rectOf(t, m, layerPalette)
	before := stateOf(m)
	// Its border, title and query row, and the screen beneath it.
	for _, p := range []image.Point{box.Min, {X: box.Min.X + 2, Y: box.Min.Y + 1}, {X: box.Min.X + 3, Y: box.Min.Y + 2},
		{X: 1, Y: box.Min.Y + 3}, rectOf(t, m, slotTabs).Min.Add(image.Pt(1, 0))} {
		clickAt(t, m, p)
	}
	if stateOf(m) != before || len(rec.all()) != 0 {
		t.Fatalf("a click off the palette's items acted: %+v", stateOf(m))
	}
	// The wheel moves its selection, within its items, and runs nothing.
	n := len(m.filteredPaletteItems())
	inside := box.Min.Add(image.Pt(4, 4))
	wheelAt(t, m, inside, 3)
	if m.paletteSelected != min(3, n-1) || !m.paletteOpen {
		t.Fatalf("wheel down 3: selected %d", m.paletteSelected)
	}
	wheelAt(t, m, inside, -10)
	if m.paletteSelected != 0 || len(rec.all()) != 0 {
		t.Fatalf("wheel up: selected %d, %d requests", m.paletteSelected, len(rec.all()))
	}
	wheelAt(t, m, image.Pt(1, box.Min.Y+3), 5)
	if m.paletteSelected != 0 || m.transcriptPos != bloom.ViewportEnd {
		t.Fatal("the wheel beneath the palette moved something")
	}
}

func TestMouseTaskRowsOpenAsEnterDoes(t *testing.T) {
	for _, size := range geometrySizes {
		for _, pid := range []int32{1, 7, 12, 31} {
			t.Run(fmt.Sprintf("%s/pid-%d", sizeName(size), pid), func(t *testing.T) {
				byMouse, mouseRec := mouseModel(t, size[0], size[1])
				byKey, keyRec := mouseModel(t, size[0], size[1])
				byMouse.taskView, byKey.taskView = true, true
				// PID 31 has a crash receipt, which opens in place of its
				// logs.
				byMouse.receipts.Add(fixtureReceipt())
				byKey.receipts.Add(fixtureReceipt())
				selectPID(t, byKey, pid)
				body := rectOf(t, byMouse, slotBody)
				rowOf := func() *taskmanager.RowSpan {
					for _, r := range byMouse.tasks.TableRows(byMouse.kit, ui.Bounds{Width: body.Dx(), Height: body.Dy()}) {
						if r.PID == pid {
							return &r
						}
					}
					return nil
				}
				// Select another row whose window shows PID's, so the
				// click is what selects it, or PID's own where the table
				// shows only the selected row.
				selectPID(t, byMouse, pid)
				for _, step := range []func(){byMouse.tasks.SelectPrev, byMouse.tasks.SelectNext, byMouse.tasks.SelectNext} {
					step()
					if sel := byMouse.tasks.SelectedProcess(); sel.ID != pid && rowOf() != nil {
						break
					}
				}
				if rowOf() == nil {
					selectPID(t, byMouse, pid)
				}
				row := rowOf()
				if row == nil {
					t.Fatal("the selected row is not in the table")
				}
				line := screenText(byMouse.View())[body.Min.Y+row.Y]
				if !regexp.MustCompile(`\b` + strconv.Itoa(int(pid)) + `\b`).MatchString(line) {
					t.Fatalf("row %d is %q, not PID %d's", row.Y, line, pid)
				}
				clickAt(t, byMouse, body.Min.Add(image.Pt(row.X+row.Width-1, row.Y)))
				act(t, byKey, enterKey)
				sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
				if sel := byMouse.tasks.SelectedProcess(); sel == nil || sel.ID != pid {
					t.Fatalf("the click opened %+v", sel)
				}
				if pid == 31 && (byMouse.tasks.Mode() != taskmanager.ModeReceipt || len(mouseRec.all()) != 0) {
					t.Fatalf("PID 31 opened mode %v, sending %v; want its receipt", byMouse.tasks.Mode(), mouseRec.all())
				}
				if pid != 31 && (byMouse.tasks.Mode() != taskmanager.ModeLogs || len(mouseRec.all()) == 0) {
					t.Fatalf("PID %d opened mode %v; want its logs fetched", pid, byMouse.tasks.Mode())
				}
			})
		}
	}
	m, rec := mouseModel(t, 80, 24)
	m.taskView = true
	body := rectOf(t, m, slotBody)
	before := stateOf(m)
	for _, p := range []image.Point{body.Min, mustFind(t, m, body, "PID"), {X: body.Min.X, Y: body.Max.Y - 1}} {
		clickAt(t, m, p)
	}
	if stateOf(m) != before || len(rec.all()) != 0 {
		t.Fatal("a click off the rows acted")
	}
}

func TestMouseNotificationsOpenAsEnterDoes(t *testing.T) {
	for _, size := range geometrySizes {
		for i, title := range []string{"indexer Backoff", "clock Crashed"} {
			t.Run(fmt.Sprintf("%s/%s", sizeName(size), title), func(t *testing.T) {
				byMouse, mouseRec := mouseModel(t, size[0], size[1])
				byKey, keyRec := mouseModel(t, size[0], size[1])
				byMouse.notifView, byKey.notifView = true, true
				for range i {
					act(t, byKey, key(tea.KeyDown))
				}
				body := rectOf(t, byMouse, slotBody)
				var span *notifications.EntrySpan
				for _, s := range byMouse.notifs.EntrySpans(byMouse.kit, ui.Bounds{Width: body.Dx(), Height: body.Dy()}) {
					if s.ID == []string{"n2", "n1"}[i] {
						span = &s
					}
				}
				if span == nil {
					t.Skip("not on screen at this size")
				}
				if line := screenText(byMouse.View())[body.Min.Y+span.Y]; !strings.Contains(line, title[:5]) {
					t.Fatalf("row %d is %q, not %q", span.Y, line, title)
				}
				clickAt(t, byMouse, body.Min.Add(image.Pt(span.X+1, span.Y+span.Height-1)))
				act(t, byKey, enterKey)
				sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
				if byMouse.notifs.UnseenCount() != 1 {
					t.Fatalf("the click left %d unseen", byMouse.notifs.UnseenCount())
				}
				// The crash opens its receipt; the backoff, which has none,
				// stays in the Notification Center.
				receipt := byMouse.taskView && byMouse.tasks.Mode() == taskmanager.ModeReceipt
				if crashed := title == "clock Crashed"; receipt != crashed || byMouse.notifView == crashed {
					t.Fatalf("%s: receipt open %v, notifications open %v", title, receipt, byMouse.notifView)
				}
			})
		}
	}
}

// appModel is the fixture shell at w×h with an app view of data open.
func appModel(t *testing.T, w, h int, data sdk.View) (*Model, *recordingCaller) {
	t.Helper()
	m, rec := mouseModel(t, w, h)
	m.presentation = &appView{id: "com.test.screen", data: data, instance: "launch", version: data.Version,
		values: map[string]string{"text": "draft"}}
	return m, rec
}

// actions decodes the action requests rec recorded, without their random
// request IDs.
func actions(t *testing.T, rec *recordingCaller) []sdk.ActionRequest {
	t.Helper()
	var out []sdk.ActionRequest
	for _, c := range calls(rec, "app/com.test.screen/action") {
		var p sdk.ActionRequest
		if err := json.Unmarshal([]byte(c.params), &p); err != nil {
			t.Fatal(err)
		}
		p.RequestID = ""
		out = append(out, p)
	}
	return out
}

func wideData() sdk.View {
	data := padData()
	data.Actions = []sdk.Action{{ID: "send", Label: "送信 é"}, {ID: "keep", Label: "Keep"}}
	data.Grid.Cells[0].Label = "界e\u0301"
	return data
}

func TestMouseAppViewActionsAndCellsRunAsEnterDoes(t *testing.T) {
	type target struct {
		label string
		keys  []tea.KeyPressMsg // focus it from the first field and press Enter
	}
	tab, right, down := key(tea.KeyTab), key(tea.KeyRight), key(tea.KeyDown)
	for _, size := range [][2]int{{80, 24}, {200, 60}} {
		for _, tg := range []target{
			{"[送信 é]", []tea.KeyPressMsg{tab, enterKey}},
			{"[Keep]", []tea.KeyPressMsg{tab, tab, enterKey}},
			{"[界e\u0301]", []tea.KeyPressMsg{tab, tab, tab, enterKey}},
			{"[NE]", []tea.KeyPressMsg{tab, tab, tab, right, enterKey}},
			{"[SW]", []tea.KeyPressMsg{tab, tab, tab, down, enterKey}},
			{"[SE]", []tea.KeyPressMsg{tab, tab, tab, down, right, enterKey}},
		} {
			t.Run(sizeName(size)+"/"+tg.label, func(t *testing.T) {
				byMouse, mouseRec := appModel(t, size[0], size[1], wideData())
				byKey, keyRec := appModel(t, size[0], size[1], wideData())
				p, ok := findText(byMouse, rectOf(t, byMouse, slotBody), tg.label)
				if !ok {
					t.Skip("the grid's window does not show it at this size")
				}
				// The label's last cell is still the target.
				clickAt(t, byMouse, p.Add(image.Pt(ansi.StringWidth(tg.label)-1, 0)))
				for _, k := range tg.keys {
					act(t, byKey, k)
				}
				if got, want := fmt.Sprint(actions(t, mouseRec)), fmt.Sprint(actions(t, keyRec)); got != want {
					t.Fatalf("mouse ran %s, keyboard %s", got, want)
				}
				if got, want := stateOf(byMouse), stateOf(byKey); got != want {
					t.Fatalf("mouse and keyboard differ:\nmouse %+v\n  key %+v", got, want)
				}
			})
		}
	}
	m, rec := appModel(t, 200, 60, wideData())
	for _, label := range []string{"[SW]", "[SE]"} {
		clickAt(t, m, mustFind(t, m, rectOf(t, m, slotBody), label))
	}
	if got := actions(t, rec); len(got) != 0 {
		t.Fatalf("a cell with no action or a disabled one ran %v", got)
	}
	p := mustFind(t, m, rectOf(t, m, slotBody), "[Keep]")
	clickAt(t, m, p.Add(image.Pt(len("[Keep]"), 0)))
	clickAt(t, m, p.Sub(image.Pt(1, 0)))
	if got := actions(t, rec); len(got) != 0 {
		t.Fatalf("a click beside an action ran %v", got)
	}
}

func TestMouseAppViewGatesMatchTheKeyboard(t *testing.T) {
	type target struct {
		label string
		focus int // the focus Enter runs it from
	}
	adopt, nw := target{"[Adopt]", 1}, target{"[NW]", 2}
	for _, tc := range []struct {
		name    string
		gate    func(*Model)
		targets []target
	}{
		{"action in flight", func(m *Model) { m.presentation.busy = true }, []target{adopt, nw}},
		{"loading", func(m *Model) { m.presentation.data.State = sdk.ViewLoading }, []target{adopt, nw}},
		{"no instance", func(m *Model) { m.presentation.instance = "" }, []target{adopt, nw}},
		{"shell busy", func(m *Model) { m.busy = true }, []target{adopt, nw}},
		{"disabled", func(m *Model) {
			m.presentation.data.Actions[0].Disabled = true
			m.presentation.data.Grid.Cells[0].Disabled = true
		}, []target{adopt, nw}},
		{"vetoed by a same-named disabled action", func(m *Model) {
			m.presentation.data.Actions = append(m.presentation.data.Actions, sdk.Action{ID: "press", Label: "Press", Disabled: true})
		}, []target{nw}},
		{"gone", func(m *Model) {
			m.presentation.data.Actions, m.presentation.data.Grid = nil, nil
		}, []target{adopt, nw}},
	} {
		for _, tg := range tc.targets {
			// Gated before the frame is drawn, and between the frame the
			// click lands on and the click: the click checks the state when
			// it arrives, not what was drawn.
			for _, late := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/late=%v", tc.name, tg.label, late), func(t *testing.T) {
					m, rec := appModel(t, 80, 24, padData())
					p := mustFind(t, m, rectOf(t, m, slotBody), tg.label)
					if late {
						m.View()
						tc.gate(m)
						drive(t, m, updateCmd(m, tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft}))
						drive(t, m, updateCmd(m, tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft}))
					} else {
						tc.gate(m)
						clickAt(t, m, p)
					}
					if got := actions(t, rec); len(got) != 0 {
						t.Fatalf("the click ran %v past the gate", got)
					}
					if m.presentation.focus = tg.focus; true {
						act(t, m, enterKey)
					}
					if got := actions(t, rec); len(got) != 0 {
						t.Fatalf("Enter ran %v past the gate", got)
					}
				})
			}
		}
	}
	// Once the gate clears, the same click runs.
	m, rec := appModel(t, 80, 24, padData())
	m.busy = true
	p := mustFind(t, m, rectOf(t, m, slotBody), "[NW]")
	clickAt(t, m, p)
	m.busy = false
	clickAt(t, m, p)
	if got := actions(t, rec); len(got) != 1 || got[0].Action != "press" || got[0].CellID != "nw" {
		t.Fatalf("after the gate cleared the click ran %v", got)
	}
}

// updateCmd is Update's command for msg.
func updateCmd(m *Model, msg tea.Msg) tea.Cmd {
	_, cmd := m.Update(msg)
	return cmd
}

func TestMouseConfirmationOwnsTheMouse(t *testing.T) {
	m, rec := appModel(t, 80, 24, padData())
	adopt := mustFind(t, m, rectOf(t, m, slotBody), "[Adopt]")
	tabs := rectOf(t, m, slotTabs)
	home := mustFind(t, m, tabs, "Home")
	m.openConfirm(confirmation{kind: confirmReap})
	before := stateOf(m)
	for _, p := range []image.Point{adopt, home, {X: 1, Y: 1}} {
		clickAt(t, m, p)
		wheelAt(t, m, p, 2)
	}
	if m.confirm == nil || m.confirm.kind != confirmReap || stateOf(m) != before || len(rec.all()) != 0 {
		t.Fatalf("a click under the confirmation acted: confirm %v, %v", m.confirm, rec.all())
	}
	// Cancelling it sends nothing either.
	act(t, m, escKey)
	if m.confirm != nil || len(rec.all()) != 0 {
		t.Fatalf("cancelling sent %v", rec.all())
	}
	// And the click it held off now runs.
	clickAt(t, m, adopt)
	if got := actions(t, rec); len(got) != 1 || got[0].Action != "adopt" {
		t.Fatalf("with the confirmation gone the click ran %v", got)
	}
}

func TestMouseWheelScrollsTheTranscriptAndFollowsAtItsEnd(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	for i := range 100 {
		m.append(entry{fmt.Sprintf("line %03d", i), "output"})
	}
	center := rectOf(t, m, slotBody).Min.Add(image.Pt(4, 4))
	end := transcriptView(t, m)
	wheelAt(t, m, center, -2)
	if v := transcriptView(t, m); m.transcriptPos.AtEnd() || v.Row != end.Row-6 {
		t.Fatalf("two steps up: row %d, want %d", v.Row, end.Row-6)
	}
	m.append(entry{"new output", "output"})
	if strings.Contains(bodyText(t, m), "new output") {
		t.Fatal("scrolled up, the transcript still followed")
	}
	wheelAt(t, m, center, 10)
	if !m.transcriptPos.AtEnd() {
		t.Fatalf("scrolled to the end, the transcript does not follow: %+v", m.transcriptPos)
	}
	m.append(entry{"newer output", "output"})
	if !strings.Contains(bodyText(t, m), "newer output") {
		t.Fatal("back at the end, the transcript did not follow")
	}
	if len(rec.all()) != 0 {
		t.Fatal("scrolling sent requests")
	}
}

func TestMouseWheelScrollsTheLogTailWithoutFollowing(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.taskView = true
	m.tasks.ShowLogs(7, "com.gostalgia.echo", numberedLog("log", 1, 90))
	center := rectOf(t, m, slotBody).Min.Add(image.Pt(6, 5))
	wheelAt(t, m, center, 2)
	if pos := m.tasks.LogsPos(); pos.Line == 0 || m.tasks.Following() {
		t.Fatalf("two steps down reached %+v", pos)
	}
	// Even busy, as the log tail's keys work then too.
	m.busy = true
	wheelAt(t, m, center, 40)
	if m.tasks.Following() || !strings.Contains(bodyText(t, m), "log 090") {
		t.Fatal("scrolling to the end did not reach it, or started following; only f does")
	}
	m.busy = false
	act(t, m, typed("f"))
	if !m.tasks.Following() {
		t.Fatal("f did not follow")
	}
	wheelAt(t, m, center, -1)
	if m.tasks.Following() {
		t.Fatal("scrolling up kept following")
	}
	if len(rec.all()) != 0 {
		t.Fatalf("scrolling sent %v", rec.all())
	}
}

func TestMouseWheelMovesListSelectionsWithoutActivating(t *testing.T) {
	for _, size := range geometrySizes {
		t.Run(sizeName(size), func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				setup    func(*Model)
				selected func(*Model) int
				n        int
			}{
				{"launcher", func(m *Model) { m.setMode(modeLauncher) }, func(m *Model) int { return m.selected }, 3},
				{"home", func(m *Model) { m.setMode(modeHome) }, func(m *Model) int { return m.homeView().selected }, -1},
				{"tasks", func(m *Model) { m.taskView = true }, func(m *Model) int { return m.tasks.SelectedIndex() }, 4},
				{"notifications", func(m *Model) { m.notifView = true }, func(m *Model) int {
					for i, n := range m.notifs.History() {
						if sel := m.notifs.Selected(); sel != nil && sel.ID == n.ID {
							return i
						}
					}
					return -1
				}, 2},
			} {
				m, rec := mouseModel(t, size[0], size[1])
				tc.setup(m)
				n := tc.n
				if n < 0 {
					n = m.homeView().visible
				}
				body := rectOf(t, m, slotBody)
				center := image.Pt((body.Min.X+body.Max.X)/2, (body.Min.Y+body.Max.Y)/2)
				wheelAt(t, m, center, 1)
				if got := tc.selected(m); got != min(1, n-1) {
					t.Fatalf("%s: a step down selected %d", tc.name, got)
				}
				wheelAt(t, m, center, 20)
				if got := tc.selected(m); got != n-1 {
					t.Fatalf("%s: wheel down past the end selected %d, want %d", tc.name, got, n-1)
				}
				wheelAt(t, m, center, -20)
				if got := tc.selected(m); got != 0 {
					t.Fatalf("%s: wheel up past the top selected %d", tc.name, got)
				}
				if len(rec.all()) != 0 || m.presentation != nil || m.tasks.Mode() != taskmanager.ModeTable {
					t.Fatalf("%s: the wheel activated something", tc.name)
				}
				if m.notifs.UnseenCount() != 2 {
					t.Fatalf("%s: the wheel marked notifications seen", tc.name)
				}
			}
		})
	}
}

// colorSGR reports whether s sets a color: an SGR with a foreground or
// background parameter.
func colorSGR(s string) bool {
	for _, seq := range regexp.MustCompile(`\x1b\[([0-9;:]*)m`).FindAllStringSubmatch(s, -1) {
		for _, p := range strings.FieldsFunc(seq[1], func(r rune) bool { return r == ';' || r == ':' }) {
			if n, _ := strconv.Atoi(p); (n >= 30 && n <= 49) || (n >= 90 && n <= 107) {
				return true
			}
		}
	}
	return false
}

func TestMouseTooltipsShowTheSanitizedCutText(t *testing.T) {
	// OSC 8, OSC 52, DCS and APC payloads, an SGR, and wide and combining
	// text.
	const hostile = "\x1b]8;;https://evil.example\x1b\\link\x1b]8;;\x1b\\ \x1b]52;c;ZXZpbA==\a\x1bP1$r0m\x1b\\" +
		"\x1b_Gf=24;AAAA\x1b\\\x1b[31m\u754Ce\u0301\x1b[0m"
	desc := "Fixture app " + hostile + " whose description is long enough to be cut at forty cells, the end."
	perms := []string{"ipc", "files", "network:" + hostile, "clipboard", "the-last-permission"}
	for _, tc := range []struct {
		name string
		id   launcherLine
		want string
	}{
		{"description", launcherDescription, safe(desc)},
		{"grants", launcherGrants, "GRANT  " + safe(strings.Join(perms, " \u00B7 "))},
	} {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.TrueColor} {
			for _, th := range []theme.Theme{theme.Nostalgia(), theme.Monochrome()} {
				t.Run(fmt.Sprintf("%s/%v/%s", tc.name, mode, th.Name), func(t *testing.T) {
					m := sized(fixtureModel(th, mode), 40, 24)
					m.apps[0].Manifest.Description = desc
					m.apps[0].Manifest.Permissions = perms
					m.setMode(modeLauncher)
					if th.Name == theme.Monochrome().Name {
						if r := rectOf(t, m, tc.id); ui.GraphemeWidth(tc.want) <= r.Dx() {
							t.Fatal("the line fits; the test needs it cut")
						}
					} else if !m.solve().Truncated(tc.id) {
						t.Fatal("the line fits; the test needs it cut")
					}
					if v := m.View(); v.MouseMode != tea.MouseModeAllMotion {
						t.Fatalf("cut text asks for mouse mode %v, want all motion for its tooltip", v.MouseMode)
					}
					r := rectOf(t, m, tc.id)
					mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y})
					tip, ok := m.root().Tooltip()
					want := tc.want
					if th.Name == theme.Monochrome().Name {
						want = strings.ReplaceAll(want, " · ", " | ")
					}
					if !ok || tip != want {
						t.Fatalf("tooltip %q (%v), want %q", tip, ok, want)
					}
					content := m.View().Content
					if !strings.Contains(ansi.Strip(content), "the-last-permission") && !strings.Contains(ansi.Strip(content), "the end.") {
						t.Fatal("the tooltip is not drawn")
					}
					for _, bad := range []string{"\x1b]", "\x1bP", "\x1b_", "\a", "\x1b[31m"} {
						if strings.Contains(content, bad) {
							t.Fatalf("the tooltip drew %q from the text", bad)
						}
					}
					if mode == ui.Plain && colorSGR(content) {
						t.Fatal("Plain drew a color")
					}
					for _, line := range screenText(m.View()) {
						if ansi.StringWidth(line) > 40 {
							t.Fatalf("a row is %d cells wide on a 40-cell screen: %q", ansi.StringWidth(line), line)
						}
					}
					// The pointer leaving takes it away, and acts on nothing.
					before := stateOf(m)
					mouse(t, m, tea.MouseMotionMsg{X: 1, Y: 1})
					if _, ok := m.root().Tooltip(); ok || stateOf(m) != before {
						t.Fatal("leaving the line kept its tooltip, or acted")
					}
					// The palette over it owns the pointer.
					m.openPalette()
					mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y})
					if _, ok := m.root().Tooltip(); ok {
						t.Fatal("a tooltip showed under the palette")
					}
					m.closePalette()
					// With the mouse off there is no hover at all.
					m.setMouse(false)
					mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y})
					if v := m.View(); v.MouseMode != tea.MouseModeNone {
						t.Fatalf("mouse off asks for %v", v.MouseMode)
					}
					if _, ok := m.root().Tooltip(); ok {
						t.Fatal("a tooltip showed with the mouse off")
					}
				})
			}
		}
	}
	// Text that fits has no tooltip, and nothing then asks for motion.
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 200, 60)
	m.setMode(modeLauncher)
	for _, id := range []launcherLine{launcherDescription, launcherGrants} {
		if m.solve().Truncated(id) {
			t.Fatalf("%v is cut at 200 cells", id)
		}
		r := rectOf(t, m, id)
		mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y})
		if tip, ok := m.root().Tooltip(); ok {
			t.Fatalf("text that fits shows tooltip %q", tip)
		}
	}
	if v := m.View(); v.MouseMode == tea.MouseModeAllMotion || v.MouseMode == tea.MouseModeNone {
		t.Fatalf("with nothing cut the shell asks for %v", v.MouseMode)
	}
}

// selectText drags over the n cells from p, as a terminal user selects
// text.
func selectText(t *testing.T, m *Model, p image.Point, n int) []string {
	t.Helper()
	q := p.Add(image.Pt(n-1, 0))
	sent := mouse(t, m, tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
	sent = append(sent, mouse(t, m, tea.MouseMotionMsg{X: q.X, Y: q.Y, Button: tea.MouseLeft})...)
	return append(sent, mouse(t, m, tea.MouseReleaseMsg{X: q.X, Y: q.Y, Button: tea.MouseLeft})...)
}

// toastText is the toasts on screen, their rows joined.
func toastText(m *Model) string {
	return strings.Join(strings.Fields(ansi.Strip(m.notifs.RenderToasts(m.kit, toastWidth))), " ")
}

func TestMouseSelectionCopiesToTheHostClipboardThroughPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		toast []string
	}{
		{"allowed", nil, []string{"Copied", "5 characters to the host clipboard."}},
		{"integration denied", errors.New("ipc: policy_denied: integration is disabled by operator policy: host clipboard integration is disabled"),
			[]string{"Copy blocked", "clipboard.enabled", "clipboard.allow_host_write", "sys/policy/update"}},
		{"writing denied", errors.New("ipc: policy_denied: integration is disabled by operator policy: host clipboard writing is disabled"),
			[]string{"Copy blocked", "clipboard.allow_host_write", "sys/policy/update"}},
		{"unsupported", errors.New("ipc: platform: host clipboard integration is unsupported"),
			[]string{"Copy failed", "no clipboard"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
			m.client = callerFunc(func(_ context.Context, method string, params, _ any) error {
				if method == "clipboard/write" {
					b, _ := json.Marshal(params)
					writes = append(writes, string(b))
					return tc.err
				}
				return nil
			})
			m.setMode(modePrompt)
			p := mustFind(t, m, rectOf(t, m, slotBody), "Echo, Notes")
			sent := selectText(t, m, p, 5)
			if len(writes) != 1 || writes[0] != `{"target":"host","text":"Echo,"}` {
				t.Fatalf("copied %v, want Echo, to the host clipboard once", writes)
			}
			for _, s := range sent {
				if strings.Contains(strings.ToLower(s), "clipboard") {
					t.Fatalf("the shell asked the terminal to copy: %s", s)
				}
			}
			for _, want := range tc.toast {
				if !strings.Contains(toastText(m), want) {
					t.Fatalf("toast %q lacks %q", toastText(m), want)
				}
			}
			if content := m.View().Content; strings.Contains(content, "\x1b]52") {
				t.Fatal("OSC 52 on screen")
			}
			if strings.Contains(toastText(m), "BACKOFF") || !strings.Contains(toastText(m), "COPY") {
				t.Fatalf("the copy's toast is badged as something else: %q", toastText(m))
			}
			// A failure stays in the Notification Center; a routine copy
			// that worked does not crowd it.
			var kept []notifications.Notification
			for _, n := range m.notifs.History() {
				if n.Source == "clipboard" {
					kept = append(kept, n)
				}
			}
			if tc.err == nil && len(kept) != 0 {
				t.Fatalf("a copy that worked went to the history: %+v", kept)
			}
			if tc.err != nil && (len(kept) != 1 || kept[0].Badge != "COPY" || strings.Contains(kept[0].Message, "policy_denied:")) {
				t.Fatalf("the failure's history entry is %+v", kept)
			}
		})
	}
}

func TestCtrlYCopiesTheSelectionOrTheLinesInView(t *testing.T) {
	ctrlY := tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}
	for _, mouseOn := range []bool{true, false} {
		t.Run(fmt.Sprint("mouse ", mouseOn), func(t *testing.T) {
			rec := &recordingCaller{}
			m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
			m.client = rec
			m.setMouse(mouseOn)
			m.setMode(modePrompt)
			act(t, m, ctrlY)
			want := "Welcome home. A familiar prompt. A whole new environment.\nType help to explore, or F2 to open your app shelf.\n" +
				"> apps\nEcho, Notes and Clock are installed.\nUnicode e\u0301 界 👩‍💻 survives wrapping."
			if got := copied(t, rec); len(got) != 1 || got[0] != want {
				t.Fatalf("Ctrl-Y copied %q, want %q", got, want)
			}
			// Scrolled up, only the lines in view.
			for i := range 40 {
				m.append(entry{fmt.Sprintf("line %03d", i), "output"})
			}
			act(t, m, key(tea.KeyPgUp))
			v := transcriptView(t, m)
			act(t, m, ctrlY)
			lines := strings.Split(copied(t, rec)[1], "\n")
			if len(lines) != v.Rows || !strings.Contains(bodyText(t, m), lines[0]) || !strings.Contains(bodyText(t, m), lines[len(lines)-1]) {
				t.Fatalf("Ctrl-Y scrolled copied %q, not the %d rows in view", lines, v.Rows)
			}
			m.taskView = true
			m.tasks.ShowLogs(7, "com.gostalgia.echo", "boot\nlistening on ipc\nhello 界\nbye")
			act(t, m, ctrlY)
			if got := copied(t, rec); len(got) != 3 || got[2] != "boot\nlistening on ipc\nhello 界\nbye" {
				t.Fatalf("Ctrl-Y in the log tail copied %q", got)
			}
			m.taskView = false
			m.setMode(modeHome)
			act(t, m, ctrlY)
			if len(copied(t, rec)) != 3 || !strings.Contains(toastText(m), "Nothing to copy") {
				t.Fatal("Ctrl-Y on Home copied something")
			}
		})
	}
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	selectText(t, m, mustFind(t, m, rectOf(t, m, slotBody), "Notes"), 5)
	act(t, m, ctrlY)
	if got := copied(t, rec); len(got) != 2 || got[1] != "Notes" {
		t.Fatalf("Ctrl-Y after a selection copied %q", got)
	}
}

func TestMouseWheelRevealsRowsThatClicksThenFind(t *testing.T) {
	// The Task Manager at the smallest size shows few rows: the wheel
	// brings PID 31's into view, and a click on it opens it, as selecting
	// it and Enter do.
	byMouse, mouseRec := mouseModel(t, 30, 10)
	byKey, keyRec := mouseModel(t, 30, 10)
	byMouse.taskView, byKey.taskView = true, true
	body := rectOf(t, byMouse, slotBody)
	if strings.Contains(bodyText(t, byMouse), "31") {
		t.Fatal("PID 31 is on screen before scrolling")
	}
	wheelAt(t, byMouse, body.Min.Add(image.Pt(2, body.Dy()/2)), 3)
	p, ok := findText(byMouse, body, "31")
	if !ok {
		t.Fatalf("the wheel did not bring PID 31 into view:\n%s", bodyText(t, byMouse))
	}
	clickAt(t, byMouse, p)
	selectPID(t, byKey, 31)
	act(t, byKey, enterKey)
	sameOutcome(t, byMouse, byKey, mouseRec, keyRec)
	if sel := byMouse.tasks.SelectedProcess(); sel == nil || sel.ID != 31 || byMouse.tasks.Mode() != taskmanager.ModeLogs {
		t.Fatalf("the click opened %+v in mode %v", sel, byMouse.tasks.Mode())
	}

	// The palette's last item, brought into view with the wheel.
	m, rec := mouseModel(t, 80, 24)
	m.openPalette()
	items := m.filteredPaletteItems()
	last := items[len(items)-1]
	if strings.Contains(ansi.Strip(m.View().Content), "["+last.Tag+"] "+last.Title) {
		t.Fatal("the last item is on screen before scrolling")
	}
	box := rectOf(t, m, layerPalette)
	wheelAt(t, m, box.Min.Add(image.Pt(4, 4)), len(items)+5)
	rows := paletteRows(t, m)
	y := rows[len(rows)-1]
	if line := screenText(m.View())[y]; !strings.Contains(line, "["+last.Tag+"] "+last.Title) {
		t.Fatalf("the last row is %q, not the last item", line)
	}
	clickAt(t, m, image.Pt(box.Min.X+6, y))
	byKey, keyRec = mouseModel(t, 80, 24)
	act(t, byKey, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	for range len(items) - 1 {
		act(t, byKey, key(tea.KeyDown))
	}
	act(t, byKey, enterKey)
	sameOutcome(t, m, byKey, rec, keyRec)
}

// blockingCaller answers clipboard writes only once release is closed.
type blockingCaller struct {
	release chan struct{}
	called  chan struct{}
}

func (c blockingCaller) Call(ctx context.Context, method string, _, _ any) error {
	if method != "clipboard/write" {
		return nil
	}
	close(c.called)
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestMouseCopyDoesNotHoldTheEventLoop(t *testing.T) {
	c := blockingCaller{release: make(chan struct{}), called: make(chan struct{})}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.client = c
	m.setMode(modePrompt)
	// Update hands the copy back as a command: it runs no IPC itself.
	cmd := updateCmd(m, bloom.SelectionMsg{Text: "Echo,"})
	if cmd == nil {
		t.Fatal("a selection asked for no copy")
	}
	select {
	case <-c.called:
		t.Fatal("Update wrote the clipboard itself")
	default:
	}
	reply := make(chan tea.Msg, 1)
	go func() {
		var msgs []tea.Msg
		var run func(tea.Cmd)
		run = func(cmd tea.Cmd) {
			if cmd == nil {
				return
			}
			switch msg := cmd().(type) {
			case tea.BatchMsg:
				for _, c := range msg {
					run(c)
				}
			case nil:
			default:
				msgs = append(msgs, msg)
			}
		}
		run(cmd)
		for _, msg := range msgs {
			if _, ok := msg.(copyMsg); ok {
				reply <- msg
			}
		}
	}()
	<-c.called
	// While the copy waits, the shell resizes, draws and takes keys.
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	act(t, m, typed("x"))
	if v := m.View(); !strings.Contains(ansi.Strip(v.Content), "> x") && !strings.Contains(ansi.Strip(v.Content), ">x") {
		t.Fatalf("the shell stalled while the copy waited:\n%s", ansi.Strip(v.Content))
	}
	close(c.release)
	m.Update(<-reply)
	if !strings.Contains(toastText(m), "Copied") {
		t.Fatalf("the late answer shows %q", toastText(m))
	}
}

func TestMouseOffDropsAGestureInProgress(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modeLauncher)
	card := rectOf(t, m, launcherCard("com.gostalgia.notes")).Min
	// Half a double click, then the mouse goes off and on: the next click
	// is a first click again.
	clickAt(t, m, card)
	for _, on := range []bool{false, false, true, true} {
		m.setMouse(on)
		m.View()
	}
	clickAt(t, m, card)
	if n := len(calls(rec, "app/launch")); n != 0 {
		t.Fatalf("a double click across mouse off and on launched %d times", n)
	}
	// A press before mouse off and its release after: nothing.
	m.selected = 0
	mouse(t, m, tea.MouseClickMsg{X: card.X, Y: card.Y, Button: tea.MouseLeft})
	m.setMouse(false)
	mouse(t, m, tea.MouseReleaseMsg{X: card.X, Y: card.Y, Button: tea.MouseLeft})
	m.setMouse(true)
	mouse(t, m, tea.MouseReleaseMsg{X: card.X, Y: card.Y, Button: tea.MouseLeft})
	if m.selected != 0 || len(rec.all()) != 0 {
		t.Fatalf("a click split by mouse off selected %d, sent %v", m.selected, rec.all())
	}
	// Mouse on again works.
	clickAt(t, m, card)
	clickAt(t, m, card)
	if n := len(calls(rec, "app/launch")); n != 1 {
		t.Fatalf("a double click with the mouse back on launched %d times", n)
	}
}

// onFrame delivers msgs as Bubble Tea does to the frame v drew, whatever
// the model has done since: to v's OnMouse, then to Update.
func onFrame(t *testing.T, m *Model, v tea.View, msgs ...tea.Msg) []string {
	t.Helper()
	var sent []string
	for _, msg := range msgs {
		if v.OnMouse != nil {
			sent = append(sent, drive(t, m, v.OnMouse(msg.(tea.MouseMsg)))...)
		}
		_, cmd := m.Update(msg)
		sent = append(sent, drive(t, m, cmd)...)
	}
	return sent
}

func click(p image.Point) []tea.Msg {
	return []tea.Msg{tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft}, tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft}}
}

// A click acts on what its target is when the click arrives, as Enter
// would then, not on what the frame under the pointer showed.
func TestMouseTargetsThatChangeAfterTheFrameAreRechecked(t *testing.T) {
	for _, size := range geometrySizes {
		t.Run(sizeName(size), func(t *testing.T) {
			// A process that exits after the frame showed its row.
			m, rec := mouseModel(t, size[0], size[1])
			m.taskView = true
			body := rectOf(t, m, slotBody)
			rows := m.tasks.TableRows(m.kit, ui.Bounds{Width: body.Dx(), Height: body.Dy()})
			if len(rows) == 0 {
				t.Fatal("no process rows on screen")
			}
			row := rows[len(rows)-1]
			at := body.Min.Add(image.Pt(row.X+1, row.Y))
			v := m.View()
			var left []taskmanager.ProcessRow
			for _, p := range m.tasks.Processes() {
				if p.ID != row.PID {
					left = append(left, p)
				}
			}
			m.tasks.SetProcesses(left)
			before := stateOf(m)
			onFrame(t, m, v, click(at)...)
			if stateOf(m) != before || len(rec.all()) != 0 {
				t.Fatalf("a click on exited PID %d acted: %v", row.PID, rec.all())
			}

			// A row clicked as a command starts waits for it, as Enter does.
			m, rec = mouseModel(t, size[0], size[1])
			m.taskView = true
			v = m.View()
			m.busy = true
			onFrame(t, m, v, click(at)...)
			if m.tasks.Mode() != taskmanager.ModeTable || len(rec.all()) != 0 {
				t.Fatal("a click acted while a command ran")
			}
			m.busy = false
			clickAt(t, m, at)
			if m.tasks.Mode() == taskmanager.ModeTable {
				t.Fatal("the click did nothing once the command finished")
			}

			// A notification dismissed after the frame.
			m, rec = mouseModel(t, size[0], size[1])
			m.notifView = true
			spans := m.notifs.EntrySpans(m.kit, ui.Bounds{Width: body.Dx(), Height: body.Dy()})
			if len(spans) == 0 {
				t.Fatal("no notifications on screen")
			}
			s := spans[0]
			v = m.View()
			m.notifs.Dismiss(s.ID)
			before, unseen := stateOf(m), m.notifs.UnseenCount()
			onFrame(t, m, v, click(body.Min.Add(image.Pt(s.X+1, s.Y)))...)
			if stateOf(m) != before || m.notifs.UnseenCount() != unseen || len(rec.all()) != 0 {
				t.Fatal("a click on a dismissed notification acted")
			}

			// An app uninstalled after the frame showed its card.
			m, rec = mouseModel(t, size[0], size[1])
			m.setMode(modeLauncher)
			gone := m.apps[0].Manifest.ID
			r, ok := m.solve().Rect(launcherCard(gone))
			if !ok {
				return
			}
			v = m.View()
			m.apps = slices.Clone(m.apps[1:])
			before = stateOf(m)
			onFrame(t, m, v, append(click(r.Min), click(r.Min)...)...)
			if stateOf(m) != before || len(rec.all()) != 0 {
				t.Fatalf("a double click on uninstalled %s acted: %v", gone, rec.all())
			}
		})
	}
}

// Bubble Tea routes a report through the last frame drawn, before Update:
// one that arrives after mouse off, before the next frame, does nothing.
func TestMouseOffStopsReportsAimedAtTheLastFrame(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	for i := range 100 {
		m.append(entry{fmt.Sprintf("line %03d", i), "output"})
	}
	center := rectOf(t, m, slotBody).Min.Add(image.Pt(4, 4))
	up := tea.MouseWheelMsg{X: center.X, Y: center.Y, Button: tea.MouseWheelUp}
	onFrame(t, m, m.View(), up)
	if m.transcriptPos.AtEnd() {
		t.Fatal("the wheel did not scroll the transcript with the mouse on")
	}
	m.transcriptPos = bloom.ViewportEnd
	v := m.View()
	m.setMouse(false)
	onFrame(t, m, v, up)
	if !m.transcriptPos.AtEnd() {
		t.Fatal("a wheel report after mouse off scrolled the transcript")
	}

	m.setMouse(true)
	m.taskView = true
	m.tasks.ShowLogs(7, "com.gostalgia.echo", numberedLog("log", 1, 90))
	down := tea.MouseWheelMsg{X: center.X, Y: center.Y, Button: tea.MouseWheelDown}
	v = m.View()
	before := m.tasks.LogsPos()
	m.setMouse(false)
	onFrame(t, m, v, down, down)
	if m.tasks.LogsPos() != before {
		t.Fatal("a wheel report after mouse off scrolled the log tail")
	}

	m.taskView = false
	m.setMouse(true)
	tab := mustFind(t, m, rectOf(t, m, slotTabs), "Apps")
	v = m.View()
	m.setMouse(false)
	was := stateOf(m)
	onFrame(t, m, v, click(tab)...)
	if stateOf(m) != was || len(rec.all()) != 0 {
		t.Fatal("a click report after mouse off switched screens")
	}
}

// Each shell has its own mouse: its gestures, selection and mode.
func TestMouseStateBelongsToEachShell(t *testing.T) {
	rec := &recordingCaller{}
	a := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	b := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	a.client, b.client = rec, rec
	a.setMode(modeLauncher)
	b.setMode(modeLauncher)
	id := a.apps[1].Manifest.ID
	// A click in each shell is a single click in each, not a double click.
	clickAt(t, a, rectOf(t, a, launcherCard(id)).Min)
	clickAt(t, b, rectOf(t, b, launcherCard(id)).Min)
	if a.selected != 1 || b.selected != 1 || len(calls(rec, "app/launch")) != 0 {
		t.Fatalf("selected %d and %d, launched %v", a.selected, b.selected, calls(rec, "app/launch"))
	}
	// Text selected in one is not selected in the other.
	a.setMode(modePrompt)
	b.setMode(modePrompt)
	selectText(t, a, mustFind(t, a, rectOf(t, a, slotBody), "Notes"), 5)
	if got := copied(t, rec); len(got) != 1 || got[0] != "Notes" {
		t.Fatalf("the selection copied %q", got)
	}
	if text, ok := b.root().Selection(); ok {
		t.Fatalf("the other shell has selection %q", text)
	}
	// Mouse off in one leaves the other's on.
	act(t, a, typed("mouse off"))
	act(t, a, enterKey)
	if va, vb := a.View(), b.View(); va.MouseMode != tea.MouseModeNone || vb.MouseMode == tea.MouseModeNone {
		t.Fatalf("mouse modes %v and %v after mouse off in the first", va.MouseMode, vb.MouseMode)
	}
	b.setMode(modeHome)
	clickAt(t, b, mustFind(t, b, rectOf(t, b, slotTabs), "Apps"))
	if b.currentMode() != modeLauncher {
		t.Fatal("the other shell's tab click did nothing")
	}
}

// A tooltip follows the line it belongs to as the launcher is resized,
// filtered and scrolled, and shows the text that line then holds.
func TestMouseTooltipsFollowResizeFilterAndScroll(t *testing.T) {
	long := func(name string) string {
		return name + " has a description long enough to be cut in a narrow launcher, which ends here."
	}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 40, 24)
	for i := range m.apps {
		m.apps[i].Manifest.Description = long(m.apps[i].Manifest.Name)
	}
	m.setMode(modeLauncher)
	hover := func(step, want string) {
		t.Helper()
		r := rectOf(t, m, launcherDescription)
		mouse(t, m, tea.MouseMotionMsg{X: r.Min.X + 2, Y: r.Min.Y})
		tip, ok := m.root().Tooltip()
		if want == "" && ok {
			t.Fatalf("%s: text that fits shows tooltip %q", step, tip)
		}
		if want != "" && (!ok || tip != want) {
			t.Fatalf("%s: tooltip %q (%v), want %q", step, tip, ok, want)
		}
		mouse(t, m, tea.MouseMotionMsg{X: 0, Y: 0})
		if _, ok := m.root().Tooltip(); ok {
			t.Fatalf("%s: the tooltip outlived the pointer leaving", step)
		}
	}
	hover("40x24", long(m.apps[0].Manifest.Name))
	m.Update(tea.WindowSizeMsg{Width: 64, Height: 20})
	hover("64x20", long(m.apps[0].Manifest.Name))
	body := rectOf(t, m, slotBody)
	wheelAt(t, m, image.Pt(body.Min.X+2, body.Min.Y+2), 1)
	if m.selected != 1 {
		t.Fatalf("the wheel selected %d", m.selected)
	}
	hover("scrolled", long(m.apps[1].Manifest.Name))
	act(t, m, typed(strings.ToLower(m.apps[2].Manifest.Name)))
	if f := m.filteredApps(); len(f) != 1 || f[0].Manifest.ID != m.apps[2].Manifest.ID {
		t.Fatalf("the filter left %d apps", len(f))
	}
	hover("filtered", long(m.apps[2].Manifest.Name))
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 60})
	hover("200x60", "")
}

// The Notification Center draws no toasts, so a copy's notice there goes
// straight into its list.
func TestCopyNoticesShowOnTheNotificationCenter(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.notifView = true
	n := len(m.notifs.History())
	act(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if len(copied(t, rec)) != 0 || len(m.notifs.History()) != n+1 || !strings.Contains(bodyText(t, m), "Nothing to copy") {
		t.Fatalf("Ctrl-Y on the Notification Center shows:\n%s", bodyText(t, m))
	}
	m.Update(copyMsg{chars: 3})
	if !strings.Contains(bodyText(t, m), "Copied") {
		t.Fatalf("a copy answered on the Notification Center shows:\n%s", bodyText(t, m))
	}
	m.notifView = false
	m.Update(copyMsg{chars: 3})
	if len(m.notifs.History()) != n+2 {
		t.Fatal("a copy answered elsewhere went to the history")
	}
}

func TestCtrlYGivesWayToAShortcutOnIt(t *testing.T) {
	byKey, _ := mouseModel(t, 80, 24)
	m, rec := mouseModel(t, 80, 24)
	m.shortcuts = map[string]string{"launcher": "ctrl+y"}
	act(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	act(t, byKey, key(tea.KeyF2))
	if m.currentMode() != byKey.currentMode() || len(copied(t, rec)) != 0 {
		t.Fatalf("Ctrl-Y bound to the launcher went to mode %v and copied %q", m.currentMode(), copied(t, rec))
	}
}

// A blocked copy's toast keeps its title and where to read how to allow it
// even in the smallest terminal, and gives the whole guidance where there
// is room; the mouse command gives it at any size.
func TestMouseCopyDenialStaysReadableInASmallTerminal(t *testing.T) {
	denied := copyMsg{chars: 5, err: errors.New("ipc: policy_denied: integration is disabled by operator policy: host clipboard integration is disabled")}
	guidance := []string{"clipboard.enabled", "clipboard.allow_host_write", "sys/policy/update"}
	for _, size := range [][2]int{{30, 10}, {40, 12}, {80, 24}} {
		t.Run(sizeName(size), func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), size[0], size[1])
			// Older toasts give way to it.
			m.notifs.Record(notifications.Notification{Title: "older", Message: "an older toast"})
			m.Update(denied)
			lines := screenText(m.View())
			stack := rectOf(t, m, layerToasts)
			var rows []string
			for _, line := range lines[stack.Min.Y:stack.Max.Y] {
				rows = append(rows, ansi.Cut(line, stack.Min.X, stack.Max.X))
			}
			shown := strings.Join(strings.Fields(strings.Join(rows, " ")), " ")
			for _, want := range []string{"COPY Copy blocked", "Type mouse for how to allow it"} {
				if !strings.Contains(shown, want) {
					t.Fatalf("the toast shows %q, without %q", shown, want)
				}
			}
			if size[0] >= toastWidth {
				for _, want := range guidance {
					if !strings.Contains(shown, want) {
						t.Fatalf("the toast shows %q, without %q", shown, want)
					}
				}
			}
			if body := rectOf(t, m, slotBody); stack.Dy() >= body.Dy() {
				t.Fatalf("the toast covers all %d rows of the body", body.Dy())
			}
			m.setMode(modePrompt)
			act(t, m, typed("mouse"))
			act(t, m, enterKey)
			said := m.transcript[len(m.transcript)-1].text
			for _, want := range guidance {
				if !strings.Contains(said, want) {
					t.Fatalf("mouse says %q, without %q", said, want)
				}
			}
		})
	}
}

// A modal opened after a frame was drawn owns input before that frame's
// mouse callbacks run. Neither a transcript nor a log tail may scroll from
// the stale frame; after the modal closes and a new frame is drawn, the same
// wheel works again.
func TestMouseModalStopsStaleFrameWheel(t *testing.T) {
	for _, surface := range []string{"transcript", "logs"} {
		for _, modal := range []string{"palette", "confirmation"} {
			t.Run(surface+"/"+modal, func(t *testing.T) {
				m, rec := mouseModel(t, 80, 24)
				m.setMode(modePrompt)
				center := rectOf(t, m, slotBody).Min.Add(image.Pt(4, 2))
				wheel := tea.MouseWheelMsg{X: center.X, Y: center.Y, Button: tea.MouseWheelUp}
				if surface == "logs" {
					m.taskView = true
					m.tasks.ShowLogs(7, "com.gostalgia.echo", numberedLog("log", 1, 90))
					center = rectOf(t, m, slotBody).Min.Add(image.Pt(6, 5))
					wheel.X, wheel.Y = center.X, center.Y
					wheel.Button = tea.MouseWheelDown
				} else {
					for i := range 100 {
						m.append(entry{fmt.Sprintf("line %03d", i), "output"})
					}
				}
				v := m.View()
				beforeTranscript := m.transcriptPos
				beforeLogs, beforeFollow := m.tasks.LogsPos(), m.tasks.Following()
				if modal == "palette" {
					m.openPalette()
				} else {
					m.openConfirm(confirmation{kind: confirmReap})
				}
				onFrame(t, m, v, wheel)
				if m.transcriptPos != beforeTranscript || m.tasks.LogsPos() != beforeLogs || m.tasks.Following() != beforeFollow {
					t.Fatalf("stale wheel scrolled under %s: transcript %+v -> %+v, logs %+v -> %+v, follow %v -> %v",
						modal, beforeTranscript, m.transcriptPos, beforeLogs, m.tasks.LogsPos(), beforeFollow, m.tasks.Following())
				}
				if len(rec.all()) != 0 {
					t.Fatalf("stale wheel sent requests: %v", rec.all())
				}

				act(t, m, escKey)
				v = m.View()
				if surface == "logs" {
					// A running command does not disable the log tail's wheel.
					m.busy = true
				}
				onFrame(t, m, v, wheel)
				if surface == "logs" {
					if m.tasks.LogsPos() == beforeLogs || m.tasks.Following() {
						t.Fatalf("wheel after closing %s did not scroll the busy log tail: %+v, follow %v", modal, m.tasks.LogsPos(), m.tasks.Following())
					}
				} else if m.transcriptPos == beforeTranscript || m.transcriptPos.AtEnd() {
					t.Fatalf("wheel after closing %s did not scroll the transcript: %+v", modal, m.transcriptPos)
				}
			})
		}
	}
}

// Selection completion is mouse input too. A drag release, or the delayed
// completion of a double click, cannot start a copy after a modal opens on
// the pre-modal frame. A new nonmodal frame still copies the same text.
func TestMouseModalStopsSelectionCompletion(t *testing.T) {
	for _, completion := range []string{"drag release", "double click"} {
		for _, modal := range []string{"palette", "confirmation"} {
			t.Run(completion+"/"+modal, func(t *testing.T) {
				m, rec := mouseModel(t, 80, 24)
				m.setMode(modePrompt)
				p := mustFind(t, m, rectOf(t, m, slotBody), "Echo, Notes")
				q := p.Add(image.Pt(len("Echo,")-1, 0))
				openModal := func() {
					if modal == "palette" {
						m.openPalette()
					} else {
						m.openConfirm(confirmation{kind: confirmReap})
					}
				}
				beforeHistory := len(m.notifs.History())
				beforeToasts := toastText(m)
				var sent []string
				if completion == "drag release" {
					v := m.View()
					onFrame(t, m, v,
						tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft},
						tea.MouseMotionMsg{X: q.X, Y: q.Y, Button: tea.MouseLeft})
					// Settle the dragged range in one last nonmodal frame,
					// then open the modal without drawing its layer.
					v = m.View()
					openModal()
					sent = onFrame(t, m, v, tea.MouseReleaseMsg{X: q.X, Y: q.Y, Button: tea.MouseLeft})
				} else {
					v := m.View()
					onFrame(t, m, v, click(p)...)
					v = m.View()
					onFrame(t, m, v, tea.MouseClickMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft})
					v = m.View()
					release := tea.MouseReleaseMsg{X: p.X, Y: p.Y, Button: tea.MouseLeft}
					var routeCmd tea.Cmd
					if v.OnMouse != nil {
						routeCmd = v.OnMouse(release)
					}
					_, updateCmd := m.Update(release)
					delayed := tea.Batch(routeCmd, updateCmd)
					openModal()
					sent = drive(t, m, delayed)
				}
				if !slices.Contains(sent, "bloom.SelectionMsg") {
					t.Fatalf("%s did not complete a selection: %v", completion, sent)
				}
				if got := copied(t, rec); len(got) != 0 {
					t.Fatalf("%s under %s copied %q", completion, modal, got)
				}
				if slices.Contains(sent, "shell.copyMsg") || toastText(m) != beforeToasts || len(m.notifs.History()) != beforeHistory {
					t.Fatalf("%s under %s produced copy feedback: messages %v, toast %q", completion, modal, sent, toastText(m))
				}

				act(t, m, escKey)
				m.View()
				want := "Echo,"
				if completion == "drag release" {
					selectText(t, m, p, len("Echo,"))
				} else {
					clickAt(t, m, p)
					clickAt(t, m, p)
					want = "Echo"
				}
				if got := copied(t, rec); len(got) != 1 || got[0] != want {
					t.Fatalf("after closing %s, %s copied %q, want %q", modal, completion, got, want)
				}
			})
		}
	}
}

// A copy that legitimately started before a modal opened still completes
// and reports its own result. Only selection completion is gated.
func TestMouseCopyStartedBeforeModalStillReports(t *testing.T) {
	m, rec := mouseModel(t, 80, 24)
	m.setMode(modePrompt)
	cmd := updateCmd(m, bloom.SelectionMsg{Text: "Echo,"})
	if cmd == nil {
		t.Fatal("nonmodal selection returned no copy command")
	}
	m.openPalette()
	drive(t, m, cmd)
	if got := copied(t, rec); len(got) != 1 || got[0] != "Echo," {
		t.Fatalf("pre-modal copy wrote %q", got)
	}
	if !strings.Contains(toastText(m), "Copied") {
		t.Fatalf("pre-modal copy result was suppressed: %q", toastText(m))
	}
}

// dragBetween drags from one solved text cell to another, including over
// soft-wrap and logical-line boundaries.
func dragBetween(t *testing.T, m *Model, from, to image.Point) {
	t.Helper()
	mouse(t, m, tea.MouseClickMsg{X: from.X, Y: from.Y, Button: tea.MouseLeft})
	mouse(t, m, tea.MouseMotionMsg{X: to.X, Y: to.Y, Button: tea.MouseLeft})
	mouse(t, m, tea.MouseReleaseMsg{X: to.X, Y: to.Y, Button: tea.MouseLeft})
}

// Mouse selection reads the buffer's original text, not its displayed wrap
// rows or UTF-8 fragments. Coordinates come from the solved viewport's own
// byte-position-to-cell mapping; the authenticated write must match exactly.
func TestMouseSelectionCopiesExactBufferText(t *testing.T) {
	wrapped := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	for _, tc := range []struct {
		name       string
		size       [2]int
		text, want string
		from, to   bloom.Pos
		fromRight  bool
		toRight    bool
		logs       bool
	}{
		{name: "soft wrapped rows", size: [2]int{30, 10}, text: wrapped, want: wrapped[20:41], from: bloom.Pos{Col: 20}, to: bloom.Pos{Col: 40}},
		{name: "multiple logical lines", size: [2]int{40, 12}, text: "first logical\nsecond logical\nthird logical", want: "logical\nsecond", from: bloom.Pos{Col: 6}, to: bloom.Pos{Line: 1, Col: 5}},
		{name: "multiple logical log lines", size: [2]int{40, 12}, text: "first logical\nsecond logical\nthird logical", want: "logical\nsecond", from: bloom.Pos{Col: 6}, to: bloom.Pos{Line: 1, Col: 5}, logs: true},
		{name: "combining character", size: [2]int{40, 12}, text: "cafe\u0301 au lait", want: "cafe\u0301 au", from: bloom.Pos{}, to: bloom.Pos{Col: 8}},
		{name: "CJK right halves", size: [2]int{40, 12}, text: "界界", want: "界界", from: bloom.Pos{}, to: bloom.Pos{Col: 3}, fromRight: true, toRight: true},
		{name: "ZWJ emoji", size: [2]int{40, 12}, text: "👩\u200d💻 code", want: "👩\u200d💻 code", from: bloom.Pos{}, to: bloom.Pos{Col: 15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, rec := mouseModel(t, tc.size[0], tc.size[1])
			m.setMode(modePrompt)
			var id any = viewTranscript
			if tc.logs {
				m.taskView = true
				m.tasks.ShowLogs(7, "com.gostalgia.echo", tc.text)
				id = taskmanager.LogsViewport
			} else {
				m.clearTranscript()
				m.append(entry{tc.text, "output"})
			}
			frame := m.solve()
			v, ok := frame.Viewport(id)
			if !ok {
				t.Fatal("selection viewport is not on screen")
			}
			r, ok := frame.Rect(id)
			if !ok {
				t.Fatal("selection viewport has no rectangle")
			}
			fromCell, ok := v.CellOf(tc.from)
			if !ok {
				t.Fatalf("start %+v is not visible at %v", tc.from, r)
			}
			toCell, ok := v.CellOf(tc.to)
			if !ok {
				t.Fatalf("end %+v is not visible at %v", tc.to, r)
			}
			if tc.fromRight {
				fromCell.X++
			}
			if tc.toRight {
				toCell.X++
			}
			dragBetween(t, m, r.Min.Add(fromCell), r.Min.Add(toCell))
			if got := copied(t, rec); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("clipboard write = %q (%d calls), want byte-for-byte %q", got, len(got), tc.want)
			}
		})
	}
}

// The shell never writes the terminal's clipboard: bloom's copy, which
// sends OSC 52 whatever the clipboard function answers, is never wired, and
// nothing in the experience emits OSC 52 itself.
func TestNoTerminalClipboardWrites(t *testing.T) {
	forbidden := []string{"WithClipboard", "CopySelection", "SetClipboard", "ReadClipboard", "]52;", "x1b]52", ".Trusted("}
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, f := range forbidden {
			if strings.Contains(string(src), f) {
				t.Errorf("%s uses %q", path, f)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
