package shell

import (
	"fmt"
	"strings"
	"time"

	"bloomfx.dev/bloom"
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
	homeSystemID homeSection = iota + 1
	homeAppsID
	homeDocsID
)

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
// hold it, so when it cannot hold every section, system activity collapses
// first, then the document shortcuts, before the list itself is windowed.
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
		sysLine := fmt.Sprintf("Uptime: %s · Processes: %d · Services: %d · User: %s · Drive: C:",
			up, m.homeData.ProcessCount, m.homeData.ServicesCount, user)
		return m.kit.Heading("SYSTEM ACTIVITY") + "\n" + m.kit.Muted(ui.Truncate(sysLine, b.Width))
	})
	appsRows := 1 + max(1, running)
	apps := m.kit.Leaf(func(b ui.Bounds) string { return m.renderHomeApps(b, hv) }).Natural(0, appsRows)
	docs := m.kit.Leaf(func(b ui.Bounds) string { return m.renderHomeDocs(b, hv, running) })
	return sizedNode(func(ctx bloom.Context) bloom.Child {
		return bloom.Column(
			bloom.Fixed(2, system).Collapsible(prioHomeSystem).ID(homeSystemID),
			bloom.Auto(apps).Min(min(appsRows, ctx.Height)).ID(homeAppsID),
			bloom.Fixed(1+max(1, len(m.homeData.Documents)), docs).Collapsible(prioHomeDocuments).ID(homeDocsID),
		).Gap(1)
	})
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
		rows = append(rows, ui.Truncate(m.homeRow(idx == hv.selected, viewText(a.Manifest.Name),
			"  "+badge+"  "+m.kit.Muted(viewText(a.Manifest.ID))), b.Width))
	}
	if len(rows) == 0 {
		rows = append(rows, m.kit.Muted(ui.Truncate("  ◇ No applications running (press F2 for App Launcher)", b.Width)))
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

func (m *Model) renderHomeDocs(b ui.Bounds, hv homeView, base int) string {
	lines := []string{m.kit.Heading("DOCUMENT SHORTCUTS") + " " + m.kit.Muted("(C:\\users\\guest\\documents)")}
	if len(m.homeData.Documents) == 0 {
		lines = append(lines, m.kit.Muted(ui.Truncate("  ◇ No documents found in C:\\users\\guest\\documents", b.Width)))
	}
	for i, d := range m.homeData.Documents {
		lines = append(lines, ui.Truncate(m.homeRow(base+i == hv.selected, viewText(d.Name),
			"  "+m.kit.Muted(fmt.Sprintf("%d B", d.Size))), b.Width))
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
			it := hv.items[hv.selected]
			if it.kind == homeItemApp {
				return m, m.openView(it.app)
			}
			return m, m.submit("open " + it.doc.Path)
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
