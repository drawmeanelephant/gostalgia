package shell

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"gostalgia/internal/experience/ui"
)

type paletteItem struct {
	Tag    string
	Title  string
	Detail string
	Action func(m *Model) tea.Cmd
}

func (m *Model) paletteItems() []paletteItem {
	var items []paletteItem

	// Apps
	for _, a := range m.apps {
		app := a
		name, id := viewText(app.Manifest.Name), viewText(app.Manifest.ID)
		if app.Running {
			items = append(items, paletteItem{
				Tag:    "APP",
				Title:  "Switch to " + name,
				Detail: fmt.Sprintf("%s · PID %d (LIVE)", id, app.PID),
				Action: func(m *Model) tea.Cmd {
					return m.openView(app)
				},
			})
			items = append(items, paletteItem{
				Tag:    "APP",
				Title:  "Stop " + name,
				Detail: fmt.Sprintf("Graceful stop %s (PID %d)", id, app.PID),
				Action: func(m *Model) tea.Cmd {
					return m.submit("stop " + app.Manifest.ID)
				},
			})
			items = append(items, paletteItem{
				Tag:    "APP",
				Title:  "Launch " + name + " (relaunch)",
				Detail: fmt.Sprintf("%s (already running, single-instance)", id),
				Action: func(m *Model) tea.Cmd {
					return m.submit("launch " + app.Manifest.ID)
				},
			})
		} else {
			items = append(items, paletteItem{
				Tag:    "APP",
				Title:  "Launch " + name,
				Detail: id,
				Action: func(m *Model) tea.Cmd {
					return m.submit("launch " + app.Manifest.ID)
				},
			})
		}
	}

	// Commands
	items = append(items,
		paletteItem{
			Tag:    "CMD",
			Title:  "help",
			Detail: "Command center help and keybindings",
			Action: func(m *Model) tea.Cmd { return m.submit("help") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "home",
			Detail: "Switch to Home screen dashboard (F1)",
			Action: func(m *Model) tea.Cmd { m.setMode(modeHome); return nil },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "apps",
			Detail: "Search and launch applications (F2)",
			Action: func(m *Model) tea.Cmd { m.setMode(modeLauncher); return nil },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "prompt",
			Detail: "Switch to command prompt (Esc)",
			Action: func(m *Model) tea.Cmd { m.setMode(modePrompt); return nil },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "ps",
			Detail: "Process list and supervision diagnostics",
			Action: func(m *Model) tea.Cmd { return m.submit("ps") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "settings",
			Detail: "Interactive settings, themes, shortcuts, and startup views (F7)",
			Action: func(m *Model) tea.Cmd { return m.submit("settings") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "status",
			Detail: "Full system status and services",
			Action: func(m *Model) tea.Cmd { return m.submit("status") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "profile",
			Detail: "List and manage personal workspace profiles",
			Action: func(m *Model) tea.Cmd { return m.submit("profile") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "backup",
			Detail: "Create portable backup export of personal workspace",
			Action: func(m *Model) tea.Cmd { return m.submit("backup export") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "dir",
			Detail: "Directory contents of current drive",
			Action: func(m *Model) tea.Cmd { return m.submit("dir") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "cls",
			Detail: "Clear terminal transcript",
			Action: func(m *Model) tea.Cmd {
				m.transcript = nil
				m.input = nil
				m.cursor = 0
				m.scroll = 0
				return nil
			},
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "shutdown",
			Detail: "Shut down Gostalgia environment",
			Action: func(m *Model) tea.Cmd { return m.submit("shutdown") },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "exit",
			Detail: "Leave the shell",
			Action: func(m *Model) tea.Cmd { return tea.Quit },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "theme monochrome",
			Detail: "Monochrome / high-contrast ASCII mode (accessible)",
			Action: func(m *Model) tea.Cmd { return m.submit("theme monochrome") },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "theme high-contrast",
			Detail: "High-contrast dark theme with explicit text cues",
			Action: func(m *Model) tea.Cmd { return m.submit("theme high-contrast") },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "theme high-contrast-light",
			Detail: "High-contrast light theme with explicit text cues",
			Action: func(m *Model) tea.Cmd { return m.submit("theme high-contrast-light") },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "theme nostalgia",
			Detail: "Default warm cream, amber, and slate palette",
			Action: func(m *Model) tea.Cmd { return m.submit("theme nostalgia") },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "theme midnight",
			Detail: "Cool slate and amber palette",
			Action: func(m *Model) tea.Cmd { return m.submit("theme midnight") },
		},
		paletteItem{
			Tag:    "THEME",
			Title:  "motion toggle",
			Detail: "Toggle reduced-motion mode (disable ticks/animations)",
			Action: func(m *Model) tea.Cmd { return m.submit("motion") },
		},
	)

	// Documents
	for _, d := range m.homeData.Documents {
		doc := d
		items = append(items, paletteItem{
			Tag:    "DOC",
			Title:  "Open " + viewText(doc.Name),
			Detail: fmt.Sprintf("%s (%d B)", viewText(doc.Path), doc.Size),
			Action: func(m *Model) tea.Cmd {
				return m.submit("type " + doc.Path)
			},
		})
	}

	return items
}

func (m *Model) filteredPaletteItems() []paletteItem {
	items := m.paletteItems()
	q := strings.TrimSpace(strings.ToLower(m.paletteQuery))
	if q == "" {
		return items
	}
	var out []paletteItem
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Title), q) ||
			strings.Contains(strings.ToLower(it.Detail), q) ||
			strings.Contains(strings.ToLower(it.Tag), q) {
			out = append(out, it)
		}
	}
	return out
}

func (m *Model) openPalette() {
	m.paletteOpen = true
	m.paletteQuery = ""
	m.paletteSelected = 0
	m.previousMode = m.currentMode()
}

func (m *Model) closePalette() {
	m.paletteOpen = false
	m.paletteQuery = ""
	m.paletteSelected = 0
}

func (m *Model) renderPalette(w, bodyHeight int) []string {
	var lines []string
	filtered := m.filteredPaletteItems()
	if m.paletteSelected >= len(filtered) {
		m.paletteSelected = max(0, len(filtered)-1)
	}

	paletteW := min(w-2, 66)
	paletteH := min(bodyHeight, 14)

	var inner []string
	inner = append(inner, m.kit.Heading("> ")+m.kit.Text(m.paletteQuery)+m.kit.Selection(" "))
	inner = append(inner, "")

	if len(filtered) == 0 {
		inner = append(inner, m.kit.Muted("  ◇ No matching commands, apps, or documents."))
	} else {
		availRows := max(1, paletteH-5)
		visible := availRows
		start := max(0, m.paletteSelected-visible+1)
		end := min(len(filtered), start+visible)

		for i := start; i < end; i++ {
			it := filtered[i]
			cursor := "  "
			if i == m.paletteSelected {
				cursor = m.kit.Theme().Focus.Marker + " "
			}
			tagBadge := "[" + it.Tag + "] "
			detailText := "  " + it.Detail
			if i == m.paletteSelected {
				inner = append(inner, m.kit.Selection(ui.Truncate(cursor+tagBadge+it.Title, paletteW-4))+" "+m.kit.Muted(ui.Truncate(detailText, paletteW/2)))
			} else {
				inner = append(inner, m.kit.Heading(cursor+tagBadge)+m.kit.Text(it.Title)+m.kit.Muted(ui.Truncate(detailText, paletteW/2)))
			}
		}
	}

	paletteBox := m.kit.Panel(ui.Panel{
		Title:   "COMMAND PALETTE (Ctrl-P / Esc)",
		Body:    strings.Join(inner, "\n"),
		Focused: true,
	}, ui.Bounds{Width: paletteW, Height: paletteH})

	boxLines := strings.Split(paletteBox, "\n")
	padTop := max(0, (bodyHeight-len(boxLines))/2)
	padLeft := max(0, (w-paletteW)/2)
	leftSpaces := strings.Repeat(" ", padLeft)

	for i := 0; i < padTop; i++ {
		lines = append(lines, "")
	}
	for _, l := range boxLines {
		lines = append(lines, leftSpaces+l)
	}
	return lines
}

func (m *Model) handlePaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredPaletteItems()
	switch msg.String() {
	case "up":
		m.paletteSelected = max(0, m.paletteSelected-1)
		return m, nil
	case "down":
		m.paletteSelected = min(max(0, len(filtered)-1), m.paletteSelected+1)
		return m, nil
	case "enter":
		if len(filtered) > 0 && m.paletteSelected < len(filtered) {
			item := filtered[m.paletteSelected]
			m.closePalette()
			if item.Action != nil {
				return m, item.Action(m)
			}
		}
		m.closePalette()
		return m, nil
	case "esc", "ctrl+p":
		m.closePalette()
		return m, nil
	case "backspace", "ctrl+h":
		r := []rune(m.paletteQuery)
		if len(r) > 0 {
			m.paletteQuery = string(r[:len(r)-1])
			m.paletteSelected = 0
		}
		return m, nil
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		text := string(msg.Runes)
		if msg.Type == tea.KeySpace {
			text = " "
		}
		m.paletteQuery += text
		m.paletteSelected = 0
		return m, nil
	}
	return m, nil
}
