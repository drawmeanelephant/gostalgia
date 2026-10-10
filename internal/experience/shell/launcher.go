package shell

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func (m *Model) filteredApps() []appStatus {
	q := strings.TrimSpace(strings.ToLower(m.launcherQuery))
	if q == "" {
		return m.apps
	}
	var out []appStatus
	for _, a := range m.apps {
		if strings.Contains(strings.ToLower(a.Manifest.Name), q) ||
			strings.Contains(strings.ToLower(a.Manifest.ID), q) ||
			strings.Contains(strings.ToLower(a.Manifest.Description), q) {
			out = append(out, a)
		}
	}
	return out
}

func (m *Model) renderLauncher(w, bodyHeight int) []string {
	var lines []string
	filtered := m.filteredApps()
	if m.selected >= len(filtered) {
		m.selected = max(0, len(filtered)-1)
	}

	searchBar := m.kit.Heading("SEARCH: ")
	if m.launcherQuery == "" {
		searchBar += m.kit.Muted("type to filter apps...")
	} else {
		searchBar += m.kit.Text(m.launcherQuery) + m.kit.Selection(" ")
	}
	lines = append(lines, ui.Truncate(searchBar, w))

	if len(m.apps) == 0 {
		notice := m.kit.Notice(ui.Notice{
			Title: "No apps installed", Message: "Your shelf is ready for its first app.", State: theme.Empty,
		}, ui.Bounds{Width: w, Height: max(1, bodyHeight-2)})
		lines = append(lines, strings.Split(notice, "\n")...)
		return lines
	}

	if len(filtered) == 0 {
		notice := m.kit.Notice(ui.Notice{
			Title: "No matching apps", Message: fmt.Sprintf("No apps match %q. Esc clears filter.", m.launcherQuery), State: theme.Empty,
		}, ui.Bounds{Width: w, Height: max(1, bodyHeight-2)})
		lines = append(lines, strings.Split(notice, "\n")...)
		return lines
	}

	availHeight := max(1, bodyHeight-1)
	cardHeight := min(5, availHeight)
	if cardHeight == 3 {
		cardHeight = 2
	}
	visible := max(1, (availHeight-2)/cardHeight)
	start := max(0, m.selected-visible+1)
	end := min(len(filtered), start+visible)

	for i := start; i < end; i++ {
		a := filtered[i]
		status, state := "READY", theme.Normal
		if a.Running {
			status, state = fmt.Sprintf("LIVE / PID %d", a.PID), theme.Success
		}
		card := m.kit.AppCard(ui.AppCard{
			Name: a.Manifest.Name, ID: a.Manifest.ID, Version: a.Manifest.Version,
			Status: status, State: state, Focused: i == m.selected,
		}, ui.Bounds{Width: w, Height: cardHeight})
		lines = append(lines, strings.Split(card, "\n")...)
	}

	if len(filtered) > 0 && m.selected < len(filtered) {
		a := filtered[m.selected]
		lines = append(lines, m.kit.Muted(ui.Truncate(safe(a.Manifest.Description), w)),
			m.kit.Muted(ui.Truncate("GRANT  "+safe(strings.Join(a.Manifest.Permissions, " · ")), w)))
	}

	return lines
}

func (m *Model) handleLauncherKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredApps()
	switch msg.String() {
	case "up":
		m.selected = max(0, m.selected-1)
		return m, nil
	case "down":
		m.selected = min(max(0, len(filtered)-1), m.selected+1)
		return m, nil
	case "enter":
		if len(filtered) > 0 && m.selected < len(filtered) {
			a := filtered[m.selected]
			return m, m.submit("launch " + a.Manifest.ID)
		}
		return m, nil
	case "f3":
		if len(filtered) > 0 && m.selected < len(filtered) {
			a := filtered[m.selected]
			return m, m.submit("stop " + a.Manifest.ID)
		}
		return m, nil
	case "f4":
		if len(filtered) > 0 && m.selected < len(filtered) {
			a := filtered[m.selected]
			if a.Running {
				return m, m.openView(a)
			}
			m.append(entry{"Launch an app before opening its view.", "error"})
		}
		return m, nil
	case "esc":
		if m.launcherQuery != "" {
			m.launcherQuery = ""
			m.selected = 0
			return m, nil
		}
		m.setMode(modePrompt)
		return m, nil
	case "backspace", "ctrl+h":
		r := []rune(m.launcherQuery)
		if len(r) > 0 {
			m.launcherQuery = string(r[:len(r)-1])
			m.selected = 0
		}
		return m, nil
	}
	if msg.Text != "" {
		m.launcherQuery += msg.Text
		m.selected = 0
		return m, nil
	}
	return m, nil
}
