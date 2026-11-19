package shell

import (
	"fmt"
	"strings"
	"time"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fig"
	"bloomfx.dev/bloom/fig/fonts"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

type homeData struct {
	UptimeSeconds float64
	User          string
	ProcessCount  int
	ServicesCount int
	Documents     []docShortcut
}

type docShortcut struct {
	Name string
	Path string
	Size int64
}

type homeItemType int

const (
	homeItemApp homeItemType = iota
	homeItemDoc
)

type homeItem struct {
	kind homeItemType
	app  appStatus
	doc  docShortcut
}

// key is what tells the item from the others of its kind: an app's ID, a
// document's path.
func (it homeItem) key() string {
	if it.kind == homeItemApp {
		return it.app.Manifest.ID
	}
	return it.doc.Path
}

// openHome opens it, as Enter does on it: an app's view, or the document.
func (m *Model) openHome(it homeItem) tea.Cmd {
	if it.kind == homeItemApp {
		return m.openView(it.app)
	}
	return m.submit("open " + it.doc.Path)
}

// openHomeItem selects the visible Home item of kind with key and opens it,
// if Home still shows it.
func (m *Model) openHomeItem(kind homeItemType, key string) tea.Cmd {
	hv := m.homeView()
	for i, it := range hv.items[:max(0, hv.visible)] {
		if it.kind == kind && it.key() == key {
			m.homeSelected = i
			return m.openHome(it)
		}
	}
	return nil
}

func formatUptime(seconds float64) string {
	if seconds <= 0 {
		return "just booted"
	}
	d := time.Duration(seconds * float64(time.Second))
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func (m *Model) homeItems() []homeItem {
	var items []homeItem
	for _, a := range m.apps {
		if a.Running {
			items = append(items, homeItem{kind: homeItemApp, app: a})
		}
	}
	for _, d := range m.homeData.Documents {
		items = append(items, homeItem{kind: homeItemDoc, doc: d})
	}
	return items
}

// homeSection names a Home dashboard section for Frame.Rect.
type homeSection int

const (
	homeWordmarkID homeSection = iota + 1
	homeSystemID
	homeAppsID
	homeDocsID
	homeCompositionID
)

// homeMaxContentWidth bounds Home's composition at large sizes. The large
// wordmark is 80 cells wide, and the section rows fit within it. A wider
// body centers that column instead of spreading the dashboard over the whole
// frame and leaving it pinned to the upper-left.
const homeMaxContentWidth = 80

const homeSpacedTitle = "G O S T A L G I A"

type homeWordmark struct {
	text          string
	font          string
	width, height int
}

// selectHomeWordmark uses the available content cells, not the raw terminal
// width. Both bundled fonts are ASCII; the final fallback is the pre-FIGlet
// spaced title. Rendering is static and attaches no animation commands.
func selectHomeWordmark(width int) homeWordmark {
	for _, candidate := range []struct {
		name    string
		font    *fig.Font
		options fig.Options
	}{{"big", fonts.Big(), fig.Options{Tracking: 2}}, {"mini", fonts.Mini(), fig.Options{}}} {
		figure := candidate.font.Figure("GOSTALGIA", candidate.options)
		w, h := figure.Size()
		if w <= width {
			return homeWordmark{text: figure.String(), font: candidate.name, width: w, height: h}
		}
	}
	return spacedHomeWordmark()
}

func spacedHomeWordmark() homeWordmark {
	return homeWordmark{text: homeSpacedTitle, font: "spaced-title", width: ui.GraphemeWidth(homeSpacedTitle), height: 1}
}

// selectHomeWordmarkForBounds also preserves one gap and one actionable app
// row on short screens. Width thresholds remain the large/mini/spaced order.
func selectHomeWordmarkForBounds(width, height int) homeWordmark {
	mark := selectHomeWordmark(width)
	if height > 0 && mark.height+2 > height {
		return spacedHomeWordmark()
	}
	return mark
}

// Collapse priorities of Home's secondary sections, lowest dropped first.
// Running applications are primary and never collapse; they shrink to the
// selected row instead.
const (
	prioHomeSystem    = 1
	prioHomeDocuments = 2
)

// homeView is what Home shows and acts on at the current size: every item,
// how many of them are on screen (documents drop out when their section
// collapses), and the selection clamped to those visible items.
type homeView struct {
	items    []homeItem
	visible  int
	selected int // -1 when nothing visible is actionable
}

func (m *Model) homeView() homeView {
	items := m.homeItems()
	hv := homeView{items: items, visible: len(items)}
	if m.currentMode() == modeHome && len(m.homeData.Documents) > 0 {
		hidden := m.width < minFrameWidth || m.height < minFrameHeight
		if !hidden {
			// Section visibility depends only on sizes, never on the
			// selection, so the probe can use any selection.
			_, ok := bloom.Solve(m.width, m.height, m.frame(hv)).Rect(homeDocsID)
			hidden = !ok
		}
		if hidden {
			hv.visible -= len(m.homeData.Documents)
		}
	}
	hv.selected = min(max(0, m.homeSelected), hv.visible-1)
	return hv
}

// homeNode lays Home out as sections one blank row apart. Running
// applications are primary: they keep their full list whenever the body can
// hold it. A long document list first shrinks to a window around the
// selection; when even that does not fit, system activity collapses first,
// then the document shortcuts, before the app list itself is windowed.
func (m *Model) homeNode(hv homeView) bloom.Child {
	running := 0
	for _, a := range m.apps {
		if a.Running {
			running++
		}
	}
	system := m.kit.Leaf(func(b ui.Bounds) string {
		up := formatUptime(m.homeData.UptimeSeconds)
		user := viewText(m.homeData.User)
		if user == "" {
			user = "guest"
		}
		separator := m.kit.Separator()
		sysLine := fmt.Sprintf("Uptime: %s%sProcesses: %d%sServices: %d%sUser: %s%sDrive: C:",
			up, separator, m.homeData.ProcessCount, separator, m.homeData.ServicesCount, separator, user, separator)
		return m.kit.Heading("SYSTEM ACTIVITY") + "\n" + m.kit.Muted(m.kit.Truncate(sysLine, b.Width))
	})
	appsRows := 1 + max(1, running)
	apps := m.withMouse(m.kit.Leaf(func(b ui.Bounds) string { return m.renderHomeApps(b, hv) }).Natural(0, appsRows),
		func(b ui.Bounds) []hit { return m.homeAppHits(b, hv) }, nil)
	docsRows := 1 + max(1, len(m.homeData.Documents))
	docs := m.withMouse(m.kit.Leaf(func(b ui.Bounds) string { return m.renderHomeDocs(b, hv, running) }).Natural(0, docsRows),
		func(b ui.Bounds) []hit { return m.homeDocHits(b, hv, running) }, nil)
	return m.withMouse(ui.SizedNode(func(ctx bloom.Context) bloom.Child {
		// Bound the composition, then center that column on bodies wider
		// than it. At 80x24 and 30x10 the column still takes the whole body,
		// so the established composition and collapse priorities hold.
		width := min(ctx.Width, homeMaxContentWidth)
		mark := selectHomeWordmarkForBounds(width, ctx.Height)
		wordmark := m.kit.Leaf(func(ui.Bounds) string { return m.kit.Heading(mark.text) })
		content := bloom.Column(
			bloom.Fixed(mark.height, wordmark).ID(homeWordmarkID),
			bloom.Fixed(2, system).Collapsible(prioHomeSystem).ID(homeSystemID),
			bloom.Auto(apps).Min(1).ID(homeAppsID),
			bloom.Auto(docs).Min(min(2, docsRows)).Collapsible(prioHomeDocuments).ID(homeDocsID),
		).Gap(1).ID(homeCompositionID)
		if width >= ctx.Width {
			return content
		}
		return bloom.Row(
			bloom.Fixed((ctx.Width-width)/2, nil),
			bloom.Fixed(width, content),
			bloom.Fill(nil),
		)
	}), nil, m.homeScroll)
}

func (m *Model) homeRow(selected bool, name, rest string) string {
	if selected {
		return m.kit.Selection(m.kit.Theme().Focus.Marker+" "+name) + rest
	}
	return "  " + m.kit.Text(name) + rest
}

// renderHomeApps lists the running applications, windowed so a selected
// app stays visible. With a single row it drops the heading and shows the
// selected app.
func (m *Model) renderHomeApps(b ui.Bounds, hv homeView) string {
	var rows []string
	selectedRow := -1
	for _, a := range m.apps {
		if !a.Running {
			continue
		}
		idx := len(rows)
		if idx == hv.selected {
			selectedRow = idx
		}
		badge := m.kit.Badge(fmt.Sprintf("LIVE / PID %d", a.PID), theme.Success, 20)
		rows = append(rows, m.kit.Truncate(m.homeRow(idx == hv.selected, viewText(a.Manifest.Name),
			"  "+badge+"  "+m.kit.Muted(viewText(a.Manifest.ID))), b.Width))
	}
	if len(rows) == 0 {
		rows = append(rows, m.kit.Muted(m.kit.Truncate("  "+m.kit.EmptyMarker()+" No applications running (press F2 for App Launcher)", b.Width)))
	}
	heading := m.kit.Heading("RUNNING APPLICATIONS")
	n := b.Height - 1
	if n < 1 {
		n, heading = 1, ""
	}
	start := max(0, min(selectedRow-n+1, len(rows)-n))
	rows = rows[start:min(len(rows), start+n)]
	if heading == "" {
		return strings.Join(rows, "\n")
	}
	return heading + "\n" + strings.Join(rows, "\n")
}

// renderHomeDocs lists the document shortcuts, windowed so a selected
// document stays visible.
func (m *Model) renderHomeDocs(b ui.Bounds, hv homeView, base int) string {
	lines := []string{m.kit.Heading("DOCUMENT SHORTCUTS") + " " + m.kit.Muted("(C:\\users\\guest\\documents)")}
	docs := m.homeData.Documents
	if len(docs) == 0 {
		lines = append(lines, m.kit.Muted(m.kit.Truncate("  "+m.kit.EmptyMarker()+" No documents found in C:\\users\\guest\\documents", b.Width)))
	}
	n := max(1, b.Height-1)
	start := max(0, min(hv.selected-base-n+1, len(docs)-n))
	for i := start; i < min(len(docs), start+n); i++ {
		lines = append(lines, m.kit.Truncate(m.homeRow(base+i == hv.selected, viewText(docs[i].Name),
			"  "+m.kit.Muted(fmt.Sprintf("%d B", docs[i].Size))), b.Width))
	}
	return strings.Join(lines, "\n")
}

func (m *Model) handleHomeKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	hv := m.homeView()
	switch msg.String() {
	case "up":
		if hv.visible > 0 {
			m.homeSelected = max(0, hv.selected-1)
		}
		return m, nil
	case "down":
		if hv.visible > 0 {
			m.homeSelected = min(hv.visible-1, hv.selected+1)
		}
		return m, nil
	case "enter":
		if msg.IsRepeat {
			// A held key opens once, not per auto-repeat.
			return m, nil
		}
		if hv.selected >= 0 {
			return m, m.openHome(hv.items[hv.selected])
		}
		return m, nil
	case "f4":
		if hv.selected >= 0 {
			it := hv.items[hv.selected]
			if it.kind == homeItemApp {
				return m, m.openView(it.app)
			}
		}
		return m, nil
	case "/":
		m.openPalette()
		return m, nil
	case "esc":
		m.setMode(modePrompt)
		return m, nil
	}
	if safe(msg.Text) != "" {
		m.setMode(modePrompt)
		return m.handlePromptKey(msg)
	}
	return m, nil
}
