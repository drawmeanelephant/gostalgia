package shell

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func builtinThemes() []theme.Theme {
	return []theme.Theme{theme.Nostalgia(), theme.Midnight(), theme.Monochrome(), theme.HighContrast(), theme.HighContrastLight()}
}

var geometrySizes = [][2]int{{30, 10}, {80, 24}, {200, 60}}

// activation is text each screen must keep on screen at every approved
// size: the thing the keyboard currently acts on and, for the diagnostic
// views, what identifies it. The fixture selects the newest notification.
var activation = map[string][]string{
	"home":          {"Echo"},
	"prompt":        {"llo"},
	"launcher":      {"Clock"},
	"app":           {"[NE]"},
	"tasks-table":   {"com.gos"},
	"tasks-logs":    {"LOGS · PID 7", "listening on ipc"},
	"tasks-receipt": {"clock (PID 31)", "Reason: exit status 2"},
	"notifications": {"indexer Backoff", "PID 12 backoff"},
}

func sized(m *Model, w, h int) *Model {
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

func screenText(v tea.View) []string {
	return strings.Split(ansi.Strip(v.Content), "\n")
}

// checkFrame asserts the solved frame: panel above help, every panel slot
// inside the kit's panel content rectangle at its full width, the slots
// tiling that rectangle top to bottom without overlapping.
func checkFrame(t *testing.T, m *Model) map[frameSlot]image.Rectangle {
	t.Helper()
	w, h := m.width, m.height
	f := m.solve()
	panel, ok := f.Rect(slotPanel)
	if !ok || panel != image.Rect(0, 0, w, h-1) {
		t.Fatalf("panel = %v (%v), want %v", panel, ok, image.Rect(0, 0, w, h-1))
	}
	if help, ok := f.Rect(slotHelp); !ok || help != image.Rect(0, h-1, w, h) {
		t.Fatalf("help = %v (%v), want the last row", help, ok)
	}
	x, y, content := m.kit.PanelContentRect(ui.Bounds{Width: w, Height: h - 1})
	inner := image.Rect(x, y, x+content.Width, y+content.Height)
	if want := m.kit.PanelContentBounds(ui.Bounds{Width: w, Height: h - 1}); inner.Dx() != want.Width || inner.Dy() != want.Height {
		t.Fatalf("content rect %v disagrees with PanelContentBounds %+v", inner, want)
	}
	rects := map[frameSlot]image.Rectangle{}
	next := inner.Min.Y
	for _, id := range []frameSlot{slotTabs, slotBadge, slotBody, slotPrompt} {
		r, ok := f.Rect(id)
		if !ok {
			t.Fatalf("slot %d missing", id)
		}
		if r.Dx() != inner.Dx() || !r.In(inner) || r.Dy() <= 0 {
			t.Fatalf("slot %d = %v, want a nonempty full-width row inside %v", id, r, inner)
		}
		if gap := r.Min.Y - next; gap < 0 || gap > 1 {
			t.Fatalf("slot %d starts at row %d after row %d: overlapping or untiled", id, r.Min.Y, next)
		}
		next = r.Max.Y
		rects[id] = r
	}
	if next != inner.Max.Y {
		t.Fatalf("slots end at row %d, panel content at %d", next, inner.Max.Y)
	}
	return rects
}

// checkRendered asserts the rendered content is exactly the terminal
// rectangle in cells, valid UTF-8, with the fixture's grapheme clusters
// whole.
func checkRendered(t *testing.T, v tea.View, w, h int) []string {
	t.Helper()
	if !utf8.ValidString(v.Content) {
		t.Fatal("rendered invalid UTF-8")
	}
	lines := screenText(v)
	if len(lines) != h {
		t.Fatalf("rendered %d rows, want %d", len(lines), h)
	}
	for i, line := range lines {
		if got := ansi.StringWidth(line); got != w {
			t.Fatalf("row %d is %d cells, want %d: %q", i, got, w, line)
		}
		for j := strings.Index(line, "👩"); j >= 0; j = strings.Index(line[j+1:], "👩") + j + 1 {
			if !strings.HasPrefix(line[j:], "👩\u200d💻") {
				t.Fatalf("row %d splits a ZWJ cluster: %q", i, line)
			}
			if strings.Index(line[j+1:], "👩") < 0 {
				break
			}
		}
		if j := strings.Index(line, "\u0301"); j >= 0 && (j == 0 || line[j-1] != 'e') {
			t.Fatalf("row %d splits a combining cluster: %q", i, line)
		}
	}
	return lines
}

func TestFrameGeometryEveryScreenThemeAndSize(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, size := range geometrySizes {
			for _, s := range geometryScreens() {
				t.Run(fmt.Sprintf("%s/%s/%dx%d", th.Name, s.name, size[0], size[1]), func(t *testing.T) {
					m := sized(fixtureModel(th, ui.TrueColor), size[0], size[1])
					s.setup(m)
					rects := checkFrame(t, m)
					v := m.View()
					lines := checkRendered(t, v, size[0], size[1])
					screen := strings.Join(lines, "\n")
					want := append([]string(nil), activation[s.name]...)
					if th.Name == theme.Monochrome().Name {
						for i := range want {
							want[i] = strings.ReplaceAll(want[i], " · ", " | ")
						}
					}
					if strings.HasPrefix(s.name, "palette-from-") {
						want = []string{"PALETTE>"}
					}
					if s.name == "home" {
						f := m.solve()
						for _, id := range []homeSection{homeSystemID, homeAppsID, homeDocsID} {
							if r, ok := f.Rect(id); ok && !r.In(rects[slotBody]) {
								t.Fatalf("home section %d at %v outside the body %v", id, r, rects[slotBody])
							}
						}
					}
					// The prompt draft and the palette's prompt live on the
					// prompt row; everything else must be in the body itself.
					where := strings.Join(lines[rects[slotBody].Min.Y:rects[slotBody].Max.Y], "\n")
					if s.name == "prompt" || strings.HasPrefix(s.name, "palette-from-") {
						where = screen
					}
					for _, want := range want {
						if !strings.Contains(where, want) {
							t.Fatalf("%q not on screen where expected:\n%s", want, screen)
						}
					}
					prompt := lines[rects[slotPrompt].Min.Y]
					if s.name == "prompt" != (v.Cursor != nil) {
						t.Fatalf("cursor = %+v on %s; prompt row %q", v.Cursor, s.name, prompt)
					}
				})
			}
		}
	}
}

// A custom theme with wider padding and a different border exposes any
// inset the shell computes itself instead of asking the kit.
func TestFrameFollowsKitPanelInsets(t *testing.T) {
	th := theme.Nostalgia()
	th.Name = "wide-pad"
	th.Spacing.PanelX = 4
	th.Borders.Panel = lipgloss.DoubleBorder()
	for _, size := range geometrySizes {
		for _, s := range geometryScreens() {
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(th, ui.TrueColor), size[0], size[1])
				s.setup(m)
				rects := checkFrame(t, m)
				if got := rects[slotBody].Min.X; got != 1+4 {
					t.Fatalf("body starts at column %d, want border+padding 5", got)
				}
				lines := checkRendered(t, m.View(), size[0], size[1])
				body := rects[slotBody]
				row := []rune(lines[body.Min.Y])
				if row[0] != '║' || row[len(row)-1] != '║' {
					t.Fatalf("body row is not inside the theme's border: %q", lines[body.Min.Y])
				}
			})
		}
	}
}

type modelState struct {
	mode                                 viewMode
	input                                string
	cursor, scroll, selected, homeSel    int
	paletteOpen                          bool
	paletteSel                           int
	paletteFilter                        string
	taskView, notifView, busy            bool
	taskSel                              int
	taskMode                             taskmanager.Mode
	focus, cell, regionTop, regionWindow int
	transcript                           int
	transcriptPos                        bloom.ViewportPos
}

func stateOf(m *Model) modelState {
	s := modelState{
		mode: m.currentMode(), input: string(m.input), cursor: m.cursor, scroll: m.scroll,
		selected: m.selected, homeSel: m.homeSelected, paletteOpen: m.paletteOpen,
		paletteSel: m.paletteSelected, paletteFilter: m.paletteQuery,
		taskView: m.taskView, notifView: m.notifView, busy: m.busy,
		taskSel: m.tasks.SelectedIndex(), taskMode: m.tasks.Mode(), transcript: len(m.transcript),
		transcriptPos: m.transcriptPos,
	}
	if v := m.presentation; v != nil {
		s.focus, s.cell, s.regionTop, s.regionWindow = v.focus, v.cell, v.regionTop, v.regionWindow
	}
	return s
}

func TestRepeatedRenderKeepsState(t *testing.T) {
	for _, size := range geometrySizes {
		for _, s := range geometryScreens() {
			t.Run(fmt.Sprintf("%s/%dx%d", s.name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
				s.setup(m)
				first := m.View()
				want := stateOf(m)
				for range 3 {
					m.solve()
					v := m.View()
					if v.Content != first.Content || !sameCursor(v.Cursor, first.Cursor) {
						t.Fatal("re-rendering unchanged state changed the frame")
					}
					if !v.AltScreen || !v.KeyboardEnhancements.ReportEventTypes {
						t.Fatal("view dropped its terminal modes")
					}
				}
				if got := stateOf(m); got != want {
					t.Fatalf("rendering changed state:\n got %+v\nwant %+v", got, want)
				}
			})
		}
	}
}

func sameCursor(a, b *tea.Cursor) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Position == b.Position
}

// Home: system activity collapses first, then document shortcuts; running
// applications always stay. Growing back restores both.
func TestHomeCollapsesSecondarySectionsInPriorityOrder(t *testing.T) {
	m := fixtureModel(theme.Nostalgia(), ui.TrueColor)
	m.setMode(modeHome)
	var walk [][2]int
	for h := 60; h >= minFrameHeight; h-- {
		walk = append(walk, [2]int{max(minFrameWidth, h*200/60), h})
	}
	for h := minFrameHeight; h <= 60; h++ {
		walk = append(walk, [2]int{80, h})
	}
	seen := map[[2]bool]bool{}
	for _, size := range walk {
		sized(m, size[0], size[1])
		f := m.solve()
		_, system := f.Rect(homeSystemID)
		_, docs := f.Rect(homeDocsID)
		apps, ok := f.Rect(homeAppsID)
		if !ok || apps.Dy() < 1 {
			t.Fatalf("%dx%d: running applications collapsed", size[0], size[1])
		}
		if system && !docs {
			t.Fatalf("%dx%d: documents collapsed before system activity", size[0], size[1])
		}
		seen[[2]bool{system, docs}] = true
		screen := strings.Join(screenText(m.View()), "\n")
		if strings.Contains(screen, "SYSTEM ACTIVITY") != system || strings.Contains(screen, "DOCUMENT SHORTCUTS") != docs {
			t.Fatalf("%dx%d: rendered sections disagree with the solved frame:\n%s", size[0], size[1], screen)
		}
		if !strings.Contains(screen, "Echo") {
			t.Fatalf("%dx%d: selected app not on screen:\n%s", size[0], size[1], screen)
		}
	}
	for _, want := range [][2]bool{{true, true}, {false, true}, {false, false}} {
		if !seen[want] {
			t.Fatalf("walk never reached system=%v docs=%v; saw %v", want[0], want[1], seen)
		}
	}
	sized(m, 200, 60)
	f := m.solve()
	if _, ok := f.Rect(homeSystemID); !ok {
		t.Fatal("growing did not restore system activity")
	}
}

// homeHeightWithoutDocs finds an 80-column Home height at which document
// shortcuts have collapsed.
func homeHeightWithoutDocs(t *testing.T, m *Model) int {
	t.Helper()
	for h := 24; h >= minFrameHeight; h-- {
		sized(m, 80, h)
		if _, ok := m.solve().Rect(homeDocsID); !ok {
			return h
		}
	}
	t.Fatal("documents never collapse")
	return 0
}

func TestHomeSelectionActsOnVisibleItems(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
	m.setMode(modeHome)
	for range 3 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if hv := m.homeView(); hv.selected != 3 || hv.items[3].doc.Name != "todo.txt" {
		t.Fatalf("80x24 selection = %d, want the last document", hv.selected)
	}

	h := homeHeightWithoutDocs(t, m)
	hv := m.homeView()
	if hv.visible != 2 || hv.selected != 1 {
		t.Fatalf("collapsed home: visible %d selected %d, want the last running app", hv.visible, hv.selected)
	}
	screen := strings.Join(screenText(m.View()), "\n")
	if !strings.Contains(screen, m.kit.Theme().Focus.Marker+" Clock") {
		t.Fatalf("80x%d: focus marker not on Clock:\n%s", h, screen)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if hv := m.homeView(); hv.selected != 1 {
		t.Fatalf("Down moved onto a hidden document: %d", hv.selected)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.presentation == nil || m.presentation.id != "com.gostalgia.clock" {
		t.Fatalf("Enter did not open the visible Clock app: %+v", m.presentation)
	}
	if m.busy {
		t.Fatal("Enter submitted the hidden document's open command")
	}

	// Growing restores the documents; the selection is the one the keys left.
	m.presentation = nil
	sized(m, 80, 24)
	if hv := m.homeView(); hv.visible != 4 || hv.selected != 1 {
		t.Fatalf("grown home: visible %d selected %d", hv.visible, hv.selected)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.busy || m.presentation != nil {
		t.Fatal("Enter on a visible document did not run its open command")
	}
}

// A document list longer than the body shrinks to a window that follows the
// selection instead of hiding every document and system activity.
func TestHomeLongDocumentListStaysReachable(t *testing.T) {
	m := fixtureModel(theme.Nostalgia(), ui.TrueColor)
	m.homeData.Documents = nil
	for i := range 12 {
		name := fmt.Sprintf("doc-%02d.txt", i)
		m.homeData.Documents = append(m.homeData.Documents, docShortcut{Name: name, Path: "/users/guest/documents/" + name, Size: 10})
	}
	m.setMode(modeHome)
	for _, size := range [][2]int{{80, 24}, {200, 60}} {
		sized(m, size[0], size[1])
		screen := strings.Join(screenText(m.View()), "\n")
		for _, want := range []string{"SYSTEM ACTIVITY", "DOCUMENT SHORTCUTS", "doc-00.txt"} {
			if !strings.Contains(screen, want) {
				t.Fatalf("%dx%d: %q not on screen:\n%s", size[0], size[1], want, screen)
			}
		}
		if hv := m.homeView(); hv.visible != 14 {
			t.Fatalf("%dx%d: %d of 14 items reachable", size[0], size[1], hv.visible)
		}
	}

	sized(m, 80, 24)
	marker := m.kit.Theme().Focus.Marker
	for i := range 13 {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		hv := m.homeView()
		if hv.selected != i+1 {
			t.Fatalf("Down %d: selection %d", i+1, hv.selected)
		}
		item := hv.items[hv.selected]
		name := item.app.Manifest.Name
		if item.kind != homeItemApp {
			name = item.doc.Name
		}
		if screen := strings.Join(screenText(m.View()), "\n"); !strings.Contains(screen, marker+" "+name) {
			t.Fatalf("selected %s is not on screen:\n%s", name, screen)
		}
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !m.busy {
		t.Fatal("Enter on the last document did not run its open command")
	}
}

// The view shortcut opens the app the launcher highlights, even after a
// refresh shrinks the filtered list below the stored selection.
func TestLauncherViewOpensHighlightedAppAfterListShrinks(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
	m.apps = []appStatus{
		fixtureApp("a.echo", "zzEcho", true, 7),
		fixtureApp("a.notes", "zzNotes", false, 0),
		fixtureApp("a.clock", "zzClock", true, 31),
		fixtureApp("a.calc", "Calc", true, 40),
	}
	m.setMode(modeLauncher)
	for _, r := range "zz" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	m.apps = append(m.apps[:1:1], m.apps[2:]...)
	screen := strings.Join(screenText(m.View()), "\n")
	if !strings.Contains(screen, "zzClock") {
		t.Fatalf("highlighted app not on screen:\n%s", screen)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if m.presentation == nil || m.presentation.id != "a.clock" {
		t.Fatalf("F4 opened %+v, want the highlighted zzClock", m.presentation)
	}
}

// Task Manager: secondary columns collapse lowest priority first, PID and
// NAME never do, and the selected process stays identifiable and keyed.
func TestTasksTableCollapsesColumnsAndKeepsSelection(t *testing.T) {
	m := fixtureModel(theme.Nostalgia(), ui.TrueColor)
	m.taskView = true
	m.tasks.SelectNext()
	walk := [][2]int{{200, 60}, {120, 40}, {80, 24}, {60, 16}, {40, 12}, {30, 10}, {40, 12}, {80, 24}, {200, 60}}
	for _, size := range walk {
		sized(m, size[0], size[1])
		lines := screenText(m.View())
		screen := strings.Join(lines, "\n")
		var header string
		for _, line := range lines {
			if strings.Contains(line, "PID") && strings.Contains(line, "NAME") {
				header = line
				break
			}
		}
		if header == "" {
			t.Fatalf("%dx%d: no PID/NAME header:\n%s", size[0], size[1], screen)
		}
		if size[0] >= 80 {
			for _, title := range []string{"STATE", "RESTARTS", "CPU", "MEMORY"} {
				if !strings.Contains(header, title) {
					t.Fatalf("%dx%d: %s collapsed with room to spare: %q", size[0], size[1], title, header)
				}
			}
		}
		if size[0] == 30 {
			for _, title := range []string{"STATE", "RESTARTS", "CPU", "MEMORY"} {
				if strings.Contains(header, title) {
					t.Fatalf("30x10: %s kept instead of collapsing: %q", title, header)
				}
			}
		}
		if !strings.Contains(screen, m.kit.Theme().Focus.Marker+" 7 ") || !strings.Contains(screen, "com.gos") {
			t.Fatalf("%dx%d: selected process 7 not identifiable:\n%s", size[0], size[1], screen)
		}
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})
		if cmd == nil {
			t.Fatalf("%dx%d: l did not fetch logs", size[0], size[1])
		}
		if msg, ok := cmd().(procLogsMsg); !ok || msg.pid != 7 {
			t.Fatalf("%dx%d: l fetched %+v, want PID 7", size[0], size[1], msg)
		}
	}
}

// bodyText is the frame's body slot as rendered text, checked against the
// solved frame first.
func bodyText(t *testing.T, m *Model) string {
	t.Helper()
	rects := checkFrame(t, m)
	lines := checkRendered(t, m.View(), m.width, m.height)
	return strings.Join(lines[rects[slotBody].Min.Y:rects[slotBody].Max.Y], "\n")
}

// lineWith is the body line that contains s, or "".
func lineWith(body, s string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	return ""
}

func press(m *Model, keys ...tea.KeyPressMsg) {
	for _, k := range keys {
		m.Update(k)
	}
}

var (
	keyDown  = tea.KeyPressMsg{Code: tea.KeyDown}
	keyUp    = tea.KeyPressMsg{Code: tea.KeyUp}
	keyEnter = tea.KeyPressMsg{Code: tea.KeyEnter}
	keyEsc   = tea.KeyPressMsg{Code: tea.KeyEscape}
)

// At the 30×10 minimum the Notification Center shows the selected entry's
// title and message, and its keys act on that visible entry. Growing
// restores the bordered panel with the same selection.
func TestMinimumNotificationsShowSelectedEntry(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.TrueColor, ui.Plain} {
			t.Run(fmt.Sprintf("%s/%d", th.Name, mode), func(t *testing.T) { checkMinimumNotifications(t, th, mode) })
		}
	}
}

func checkMinimumNotifications(t *testing.T, th theme.Theme, mode ui.ColorMode) {
	t.Helper()
	m := sized(fixtureModel(th, mode), minFrameWidth, minFrameHeight)
	m.notifView = true
	marker := m.kit.Theme().Focus.Marker
	body := bodyText(t, m)
	if !strings.Contains(lineWith(body, "indexer Backoff"), marker+" ") {
		t.Fatalf("selected entry not marked on screen:\n%s", body)
	}
	if !strings.Contains(body, "PID 12 backoff") || !strings.Contains(body, "1/2") {
		t.Fatalf("selected entry's message or position missing:\n%s", body)
	}

	press(m, keyDown)
	if sel := m.notifs.Selected(); sel == nil || sel.ID != "n1" {
		t.Fatalf("Down selected %+v, want n1", sel)
	}
	body = bodyText(t, m)
	if !strings.Contains(lineWith(body, "clock Crashed"), marker+" ") || !strings.Contains(body, "PID 31 exit 2") || !strings.Contains(body, "2/2") {
		t.Fatalf("Down did not bring the newly selected entry on screen:\n%s", body)
	}
	press(m, keyUp)
	if body = bodyText(t, m); !strings.Contains(lineWith(body, "indexer Backoff"), marker+" ") || strings.Contains(body, "clock Crashed") {
		t.Fatalf("Up did not return to the first entry:\n%s", body)
	}
	press(m, keyDown)

	// a and n keep working and their effect is visible in the summary.
	press(m, tea.KeyPressMsg{Code: 'a', Text: "a"}, tea.KeyPressMsg{Code: 'n', Text: "n"})
	summary := "2/2" + m.kit.Separator() + "0 unread" + m.kit.Separator() + "DND ON"
	if body = bodyText(t, m); !strings.Contains(body, summary) || !m.notifs.DND() {
		t.Fatalf("a/n not reflected at 30x10:\n%s", body)
	}

	// Growing restores the bordered panel around the same entry.
	sized(m, 80, 24)
	body = bodyText(t, m)
	if !strings.Contains(body, "NOTIFICATION CENTER") || !strings.Contains(lineWith(body, "clock Crashed"), marker+" ") {
		t.Fatalf("80x24 lost the panel or the selection:\n%s", body)
	}

	// Enter on the visible crash opens its receipt, readable at 30×10.
	sized(m, minFrameWidth, minFrameHeight)
	press(m, keyEnter)
	if !m.taskView || m.tasks.Mode() != taskmanager.ModeReceipt || m.tasks.Receipt().ID != "CR-31-fixture" {
		t.Fatalf("Enter did not open the visible entry's receipt: task=%v mode=%v", m.taskView, m.tasks.Mode())
	}
	body = bodyText(t, m)
	for _, want := range []string{"clock (PID 31)", "Reason: exit status 2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("receipt %q not readable at 30x10:\n%s", want, body)
		}
	}
	sized(m, 80, 24)
	body = bodyText(t, m)
	for _, want := range []string{"CRASH RECEIPT #CR-31-fixture", "CRASH RECEIPT: clock (PID 31)", "Reason: exit status 2", "panic: wound too tight"} {
		if !strings.Contains(body, want) {
			t.Fatalf("80x24 receipt lost %q:\n%s", want, body)
		}
	}
	sized(m, minFrameWidth, minFrameHeight)
	press(m, keyEsc)
	if !m.taskView || m.tasks.Mode() != taskmanager.ModeTable {
		t.Fatal("Esc did not return from the receipt to the process table")
	}

	// d dismisses the entry on screen, not another one.
	m = sized(fixtureModel(th, mode), minFrameWidth, minFrameHeight)
	m.notifView = true
	press(m, keyDown, tea.KeyPressMsg{Code: 'd', Text: "d"})
	if h := m.notifs.History(); len(h) != 1 || h[0].ID != "n2" {
		t.Fatalf("d dismissed the wrong entry: %+v", h)
	}
	if body := bodyText(t, m); !strings.Contains(body, "indexer Backoff") || !strings.Contains(body, "1/1") {
		t.Fatalf("remaining entry not shown after dismiss:\n%s", body)
	}
	if mode == ui.Plain && strings.Contains(m.View().Content, "\x1b") {
		t.Fatal("Plain compact view emitted an escape sequence")
	}
}

// At the 30×10 minimum the log tail shows actual log text, Up and Down
// scroll it inside the frame, and growing restores the bordered tail at
// the same offset.
func TestMinimumLogsShowAndScrollText(t *testing.T) {
	var log []string
	for i := range 20 {
		log = append(log, fmt.Sprintf("log line %02d", i+1))
	}
	for _, th := range builtinThemes() {
		t.Run(th.Name, func(t *testing.T) {
			m := sized(fixtureModel(th, ui.TrueColor), minFrameWidth, minFrameHeight)
			m.taskView = true
			m.tasks.ShowLogs(7, "com.gostalgia.echo", strings.Join(log, "\n"))
			body := bodyText(t, m)
			for _, want := range []string{"LOGS" + m.kit.Separator() + "PID 7", "log line 01", "log line 02"} {
				if !strings.Contains(body, want) {
					t.Fatalf("%q not on screen at 30x10:\n%s", want, body)
				}
			}
			press(m, keyDown, keyDown, keyDown, keyDown, keyDown)
			body = bodyText(t, m)
			if !strings.Contains(body, "log line 06") || strings.Contains(body, "log line 01") {
				t.Fatalf("Down did not scroll the log text:\n%s", body)
			}
			press(m, keyUp)
			if body = bodyText(t, m); !strings.Contains(body, "log line 05") || strings.Contains(body, "log line 07") {
				t.Fatalf("Up did not scroll back:\n%s", body)
			}

			sized(m, 80, 24)
			body = bodyText(t, m)
			for _, want := range []string{"LOGS" + m.kit.Separator() + "PID 7", "CHILD LOG TAIL: com.gostalgia.echo (PID 7)", "log line 05", "log line 11"} {
				if !strings.Contains(body, want) {
					t.Fatalf("80x24 log tail lost %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "log line 04") {
				t.Fatalf("80x24 log tail did not keep the scroll offset:\n%s", body)
			}

			sized(m, minFrameWidth, minFrameHeight)
			press(m, keyEsc)
			if !m.taskView || m.tasks.Mode() != taskmanager.ModeTable {
				t.Fatal("Esc did not return from the logs to the process table")
			}
		})
	}
}

func TestBelowMinimumNoticeAndRecovery(t *testing.T) {
	for _, size := range [][2]int{{29, 10}, {30, 9}, {20, 6}, {29, 40}, {200, 9}} {
		m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
		f := m.solve()
		if _, ok := f.Rect(slotPanel); ok {
			t.Fatalf("%dx%d: frame drawn below the minimum", size[0], size[1])
		}
		if r, ok := f.Rect(slotNote); !ok || r != image.Rect(0, 0, size[0], size[1]) {
			t.Fatalf("%dx%d: note = %v %v", size[0], size[1], r, ok)
		}
		lines := checkRendered(t, m.View(), size[0], size[1])
		if !strings.HasPrefix(lines[0], "GOSTALGIA") || !strings.HasPrefix(lines[1], "Resize to 30") {
			t.Fatalf("%dx%d: notice missing:\n%s", size[0], size[1], strings.Join(lines, "\n"))
		}
	}
	m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 30, 10)
	if _, ok := m.solve().Rect(slotPanel); !ok || strings.Contains(m.View().Content, "Resize to") {
		t.Fatal("30x10 did not render the normal frame")
	}
	for _, size := range [][2]int{{0, 10}, {30, 0}, {0, 0}, {-1, 5}, {5, -1}} {
		v := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1]).View()
		if v.Content != "" || !v.AltScreen || !v.KeyboardEnhancements.ReportEventTypes {
			t.Fatalf("%dx%d: view = %q alt=%v", size[0], size[1], v.Content, v.AltScreen)
		}
	}
}

func TestResizeBelowMinimumPreservesState(t *testing.T) {
	for _, setup := range []geometryScreen{geometryScreens()[1], geometryScreens()[2], geometryScreens()[4]} {
		t.Run(setup.name, func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
			setup.setup(m)
			want := stateOf(m)
			before := m.View().Content
			for _, size := range [][2]int{{29, 10}, {12, 4}, {0, 0}, {30, 9}} {
				sized(m, size[0], size[1]).View()
			}
			sized(m, 80, 24)
			if got := stateOf(m); got != want {
				t.Fatalf("resize round trip changed state:\n got %+v\nwant %+v", got, want)
			}
			if m.View().Content != before {
				t.Fatal("80x24 frame did not return unchanged")
			}
			sized(m, 20, 6)
			_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
			if cmd == nil {
				t.Fatal("Ctrl-C ignored below the minimum")
			}
			if _, ok := cmd().(tea.QuitMsg); !ok {
				t.Fatal("Ctrl-C below the minimum did not quit")
			}
		})
	}
}

// cursorCase is prompt input built from whole grapheme clusters, so the
// expected insertion points are known without a segmenter.
type cursorCase struct {
	name     string
	clusters []string
}

func cursorCases() []cursorCase {
	long := make([]string, 0, 300)
	for i := range 60 {
		long = append(long, "a", "界", "👩\u200d💻", "e\u0301", string(rune('0'+i%10)))
	}
	return []cursorCase{
		{"ascii", strings.Split("echo hello", "")},
		{"combining", []string{"h", "e\u0301", "l", "l", "o"}},
		{"cjk", []string{"界", "界", " ", "界"}},
		{"zwj", []string{"a", "👩\u200d💻", "b"}},
		{"long", long},
	}
}

func runesBefore(clusters []string, k int) int {
	n := 0
	for _, c := range clusters[:k] {
		n += utf8.RuneCountInString(c)
	}
	return n
}

// checkCursor asserts the real cursor sits in the solved prompt row on the
// cell that shows the cluster after the insertion point.
func checkCursor(t *testing.T, m *Model, clusters []string, k int) {
	t.Helper()
	if m.cursor != runesBefore(clusters, k) {
		t.Fatalf("cursor rune %d, want %d (cluster %d)", m.cursor, runesBefore(clusters, k), k)
	}
	prompt, _ := m.solve().Rect(slotPrompt)
	v := m.View()
	if v.Cursor == nil {
		t.Fatal("editable prompt has no cursor")
	}
	pos := v.Cursor.Position
	if pos.Y != prompt.Min.Y || pos.X < prompt.Min.X || pos.X >= prompt.Max.X {
		t.Fatalf("cursor %v outside prompt row %v", pos, prompt)
	}
	line := screenText(v)[pos.Y]
	if left := ansi.Cut(line, 0, pos.X); ansi.StringWidth(left) != pos.X {
		t.Fatalf("cursor cell %d splits a wide cluster in %q", pos.X, line)
	}
	under := ansi.Cut(line, pos.X, prompt.Max.X)
	next := " "
	if k < len(clusters) {
		next = clusters[k]
	}
	if !strings.HasPrefix(under, next) {
		t.Fatalf("cursor at cell %d shows %q, want %q next: %q", pos.X, under, next, line)
	}
	if k > 0 {
		left := ansi.Cut(line, prompt.Min.X, pos.X)
		if prev := clusters[k-1]; !strings.HasSuffix(left, prev) {
			t.Fatalf("cluster before the cursor is not %q: %q", prev, left)
		}
	}
}

func TestPromptRealCursorFollowsInsertionPoint(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {30, 10}, {200, 60}} {
		for _, c := range cursorCases() {
			t.Run(fmt.Sprintf("%s/%dx%d", c.name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
				m.setMode(modePrompt)
				m.Update(tea.PasteMsg{Content: strings.Join(c.clusters, "")})
				input := string(m.input)
				checkCursor(t, m, c.clusters, len(c.clusters))
				m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
				checkCursor(t, m, c.clusters, 0)
				mid := len(c.clusters) / 2
				for range mid {
					m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
				}
				checkCursor(t, m, c.clusters, mid)
				if string(m.input) != input {
					t.Fatal("moving and rendering changed the input")
				}

				m.Update(tea.KeyPressMsg{Code: 'Z', Text: "Z"})
				edited := append(append(append([]string{}, c.clusters[:mid]...), "Z"), c.clusters[mid:]...)
				if string(m.input) != strings.Join(edited, "") {
					t.Fatalf("insert = %q", string(m.input))
				}
				checkCursor(t, m, edited, mid+1)
				m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
				m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
				removed := append(append([]string{}, c.clusters[:mid-1]...), c.clusters[mid:]...)
				if string(m.input) != strings.Join(removed, "") {
					t.Fatalf("backspace removed the wrong cluster: %q", string(m.input))
				}
				checkCursor(t, m, removed, mid-1)

				// Resizing recomputes the cursor for the new row.
				other := [2]int{30, 10}
				if size[0] == 30 {
					other = [2]int{80, 24}
				}
				sized(m, other[0], other[1])
				checkCursor(t, m, removed, mid-1)
				m.SetThemeByName("high-contrast")
				checkCursor(t, m, removed, mid-1)
			})
		}
	}
}

func TestPromptCursorHiddenWhenNotEditable(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
	m.setMode(modePrompt)
	m.input, m.cursor = []rune("echo"), 2
	if m.View().Cursor == nil {
		t.Fatal("editable prompt has no cursor")
	}
	m.busy = true
	if v := m.View(); v.Cursor != nil {
		t.Fatalf("busy prompt shows a cursor at %v", v.Cursor.Position)
	}
	m.busy = false
	sized(m, 20, 6)
	if v := m.View(); v.Cursor != nil {
		t.Fatal("size notice shows a prompt cursor")
	}
}

// surfaceSGR is the SGR parameter that paints the kit's surface color.
func surfaceSGR(m *Model) string {
	_, bg := m.kit.Colors()
	if c, ok := bg.(ansi.IndexedColor); ok {
		return fmt.Sprintf("48;5;%d", c)
	}
	r, g, b, _ := bg.RGBA()
	return fmt.Sprintf("48;2;%d;%d;%d", r>>8, g>>8, b>>8)
}

func TestRootIgnoresHostColors(t *testing.T) {
	m := sized(fixtureModel(theme.Midnight(), ui.TrueColor), 80, 24)
	m.setMode(modeHome)
	before := m.View().Content
	if !strings.Contains(before, surfaceSGR(m)) {
		t.Fatalf("frame does not paint the theme surface %s", surfaceSGR(m))
	}
	root := m.ui
	red := color.RGBA{R: 255, A: 255}
	for _, msg := range []tea.Msg{
		tea.BackgroundColorMsg{Color: red}, tea.ForegroundColorMsg{Color: red},
		tea.ColorProfileMsg{Profile: colorprofile.ANSI},
	} {
		m.Update(msg)
	}
	if m.ui != root {
		t.Fatal("host color reports rebuilt the root")
	}
	after := m.View().Content
	if after != before || strings.Contains(after, "255;0;0") {
		t.Fatal("host color reports changed the frame")
	}
	// Blending effects read the root's default colors; they must still be
	// the theme's.
	probe := bloom.Overlay(bloom.Text("base"), bloom.Layer(bloom.Text("x")).Backdrop(fx.Dim{Amount: 0.5}))
	fresh := bloom.New(bloom.WithColors(m.kit.Colors()), bloom.WithCopyFlash(false))
	if m.ui.Render(8, 1, probe) != fresh.Render(8, 1, probe) {
		t.Fatal("host color reports replaced the root's theme colors")
	}
}

func TestRootFollowsThemeAndProfileChanges(t *testing.T) {
	ctx := context.Background()
	m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
	m.setMode(modePrompt)
	for i := range 40 {
		m.append(entry{fmt.Sprintf("line %d", i), "output"})
	}
	m.scroll = 5
	m.Update(tea.PasteMsg{Content: "draft 界 text"})
	m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m.homeSelected, m.selected = 1, 2
	m.tasks.SelectNext()
	m.View()
	want := stateOf(m)

	ocean := theme.Nostalgia()
	ocean.Name = "ocean"
	ocean.Palette.Surface = lipgloss.Color("#1A2E3C")
	steps := []struct {
		name   string
		change func()
		check  func(content string) error
	}{
		{"built-in theme", func() { m.SetThemeByName("midnight") }, nil},
		{"theme command", func() {
			m.Update(execute(ctx, m.client, m.cwd, "theme high-contrast"))
			want.transcript = len(m.transcript) // the command reports the switch
		}, nil},
		{"user theme", func() {
			m.pendingTheme = "ocean"
			m.Update(themeResolveMsg{name: "ocean", theme: &ocean})
		}, func(content string) error {
			if m.Theme().Name != "ocean" || !strings.Contains(content, surfaceSGR(m)) {
				return fmt.Errorf("user theme surface %s not painted", surfaceSGR(m))
			}
			return nil
		}},
		{"reduced motion", func() { m.SetReducedMotion(true) }, nil},
		{"color mode plain", func() { m.SetColorModeByName("plain") }, func(content string) error {
			if strings.Contains(content, "\x1b") {
				return fmt.Errorf("plain frame has escapes")
			}
			return nil
		}},
		{"color mode 256", func() { m.SetColorModeByName("256") }, func(content string) error {
			if strings.Contains(content, "8;2;") {
				return fmt.Errorf("ANSI256 frame leaks truecolor")
			}
			return nil
		}},
		{"config truecolor", func() { m.Update(configUpdateMsg{theme: "nostalgia", colorMode: "truecolor"}) }, nil},
	}
	prev := m.View().Content
	for _, step := range steps {
		root := m.ui
		step.change()
		content := m.View().Content
		if m.ui == root {
			t.Fatalf("%s: root not rebuilt", step.name)
		}
		// Motion has no static rendering, so only it may leave the frame alone.
		if content == prev && step.name != "reduced motion" {
			t.Fatalf("%s: frame unchanged", step.name)
		}
		if got := stateOf(m); got != want {
			t.Fatalf("%s changed state:\n got %+v\nwant %+v", step.name, got, want)
		}
		if step.check != nil {
			if err := step.check(content); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
		}
		prev = content
	}
	// A config poll re-applies the same settings through intermediate kits;
	// the root follows only the effective result.
	root := m.ui
	m.Update(configUpdateMsg{theme: "nostalgia", colorMode: "truecolor"})
	if m.View(); m.ui != root {
		t.Fatal("identical settings rebuilt the root")
	}
}

func TestColorProfilesAndMonochromeChrome(t *testing.T) {
	for _, s := range geometryScreens() {
		mono := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
		mono.SetThemeByName("monochrome")
		s.setup(mono)
		if strings.Contains(mono.View().Content, "\x1b") {
			t.Fatalf("%s: the monochrome theme emits escapes", s.name)
		}
	}
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256, ui.TrueColor} {
			for _, s := range geometryScreens() {
				t.Run(fmt.Sprintf("%s/%d/%s", th.Name, mode, s.name), func(t *testing.T) {
					m := sized(fixtureModel(th, mode), 80, 24)
					s.setup(m)
					content := m.View().Content
					switch {
					case mode == ui.Plain && strings.Contains(content, "\x1b"):
						t.Fatalf("plain frame has escapes: %q", content)
					case mode == ui.ANSI256 && strings.Contains(content, "8;2;"):
						t.Fatal("ANSI256 frame leaks truecolor")
					case th.Name == "monochrome" && (strings.Contains(content, "8;5;") || strings.Contains(content, "8;2;")):
						t.Fatal("monochrome frame leaves the basic 16 colors")
					}
					if th.Name != "monochrome" {
						return
					}
					lines := screenText(m.View())
					rects := checkFrame(t, m)
					chrome := []string{lines[0], lines[len(lines)-2], lines[rects[slotTabs].Min.Y]}
					for _, line := range lines[1 : len(lines)-2] {
						r := []rune(line)
						chrome = append(chrome, string(r[0])+string(r[len(r)-1]))
					}
					for _, c := range chrome {
						for _, r := range c {
							if r > unicode.MaxASCII {
								t.Fatalf("monochrome chrome is not ASCII: %q", c)
							}
						}
					}
				})
			}
		}
	}
}

// Shell models in one process, as several sshx attaches are, each own a
// root; changing one never reaches another.
func TestShellModelsKeepIndependentRoots(t *testing.T) {
	a := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
	b := sized(fixtureModel(theme.Midnight(), ui.TrueColor), 80, 24)
	a.setMode(modeHome)
	b.setMode(modeHome)
	wantA, wantB := a.View().Content, b.View().Content
	if a.ui == b.ui || wantA == wantB {
		t.Fatal("two models share a root or a frame")
	}
	b.SetThemeByName("high-contrast-light")
	b.SetColorModeByName("plain")
	b.Update(tea.BackgroundColorMsg{Color: color.White})
	sized(b, 30, 10).View()
	if a.View().Content != wantA {
		t.Fatal("changing one model changed the other's frame")
	}
	sized(b, 80, 24)
	b.SetThemeByName("midnight")
	b.SetColorModeByName("truecolor")
	if b.View().Content != wantB {
		t.Fatal("model did not return to its own frame")
	}
}

// The shell never asks the terminal for its colors: the bloom root's Init,
// which would, is never run.
func TestProgramSendsNoColorQueries(t *testing.T) {
	m := fixtureModel(theme.Nostalgia(), ui.TrueColor)
	out := &lockedBuffer{}
	p := tea.NewProgram(m, tea.WithContext(t.Context()), tea.WithInput(nil), tea.WithOutput(out),
		tea.WithWindowSize(80, 24), tea.WithoutSignalHandler())
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "G O S T A L G I A") {
		if time.Now().After(deadline) {
			p.Kill()
			<-done
			t.Fatal("program never rendered the frame")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.Quit()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"\x1b]10;?", "\x1b]11;?"} {
		if strings.Contains(out.String(), query) {
			t.Fatalf("program queried the terminal's colors with %q", query)
		}
	}
}
