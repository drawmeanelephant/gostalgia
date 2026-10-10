package shell

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

func (m *Model) renderHome(w, bodyHeight int) []string {
	var lines []string
	items := m.homeItems()
	if m.homeSelected >= len(items) {
		m.homeSelected = max(0, len(items)-1)
	}

	// System Activity
	lines = append(lines, m.kit.Heading("SYSTEM ACTIVITY"))
	up := formatUptime(m.homeData.UptimeSeconds)
	user := viewText(m.homeData.User)
	if user == "" {
		user = "guest"
	}
	sysLine := fmt.Sprintf("Uptime: %s · Processes: %d · Services: %d · User: %s · Drive: C:",
		up, m.homeData.ProcessCount, m.homeData.ServicesCount, user)
	lines = append(lines, m.kit.Muted(ui.Truncate(sysLine, w)))
	lines = append(lines, "")

	// Running Applications
	lines = append(lines, m.kit.Heading("RUNNING APPLICATIONS"))
	runningCount := 0
	itemIdx := 0
	for _, a := range m.apps {
		if a.Running {
			runningCount++
			cursor := "  "
			if itemIdx == m.homeSelected {
				cursor = m.kit.Theme().Focus.Marker + " "
			}
			badge := m.kit.Badge(fmt.Sprintf("LIVE / PID %d", a.PID), theme.Success, 20)
			if itemIdx == m.homeSelected {
				lines = append(lines, ui.Truncate(m.kit.Selection(cursor+viewText(a.Manifest.Name))+"  "+badge+"  "+m.kit.Muted(viewText(a.Manifest.ID)), w))
			} else {
				lines = append(lines, ui.Truncate(cursor+m.kit.Text(viewText(a.Manifest.Name))+"  "+badge+"  "+m.kit.Muted(viewText(a.Manifest.ID)), w))
			}
			itemIdx++
		}
	}
	if runningCount == 0 {
		lines = append(lines, m.kit.Muted(ui.Truncate("  ◇ No applications running (press F2 for App Launcher)", w)))
	}
	lines = append(lines, "")

	// Document Shortcuts
	lines = append(lines, m.kit.Heading("DOCUMENT SHORTCUTS")+" "+m.kit.Muted("(C:\\users\\guest\\documents)"))
	if len(m.homeData.Documents) == 0 {
		lines = append(lines, m.kit.Muted(ui.Truncate("  ◇ No documents found in C:\\users\\guest\\documents", w)))
	} else {
		for _, d := range m.homeData.Documents {
			cursor := "  "
			if itemIdx == m.homeSelected {
				cursor = m.kit.Theme().Focus.Marker + " "
			}
			sizeStr := fmt.Sprintf("%d B", d.Size)
			if itemIdx == m.homeSelected {
				lines = append(lines, ui.Truncate(m.kit.Selection(cursor+viewText(d.Name))+"  "+m.kit.Muted(sizeStr), w))
			} else {
				lines = append(lines, ui.Truncate(cursor+m.kit.Text(viewText(d.Name))+"  "+m.kit.Muted(sizeStr), w))
			}
			itemIdx++
		}
	}

	return lines
}

func (m *Model) handleHomeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.homeItems()
	switch msg.String() {
	case "up":
		if len(items) > 0 {
			m.homeSelected = max(0, m.homeSelected-1)
		}
		return m, nil
	case "down":
		if len(items) > 0 {
			m.homeSelected = min(len(items)-1, m.homeSelected+1)
		}
		return m, nil
	case "enter":
		if len(items) > 0 && m.homeSelected < len(items) {
			it := items[m.homeSelected]
			if it.kind == homeItemApp {
				return m, m.openView(it.app)
			}
			return m, m.submit("open " + it.doc.Path)
		}
		return m, nil
	case "f4":
		if len(items) > 0 && m.homeSelected < len(items) {
			it := items[m.homeSelected]
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
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		m.setMode(modePrompt)
		return m.handlePromptKey(msg)
	}
	return m, nil
}
