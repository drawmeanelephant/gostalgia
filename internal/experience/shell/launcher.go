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

// launcherSearch is the launcher's search line.
func (m *Model) launcherSearch() string {
	searchBar := m.kit.Heading("SEARCH: ")
	if m.launcherQuery == "" {
		return searchBar + m.kit.Muted("type to filter apps...")
	}
	return searchBar + m.kit.Text(m.launcherQuery) + m.kit.Selection(" ")
}

// launcherCards is which of n filtered apps the launcher shows cards for in
// a body of bodyHeight rows, [start, end), keeping the selected one in
// view, and how many rows each card takes. The cards from top stay while
// the selected one is among them, so a click that selects a card does not
// scroll it from under the pointer before the second click of a double
// click.
func launcherCards(selected, n, bodyHeight, top int) (start, end, cardHeight int) {
	availHeight := max(1, bodyHeight-1)
	cardHeight = min(5, availHeight)
	if cardHeight == 3 {
		cardHeight = 2
	}
	visible := max(1, (availHeight-2)/cardHeight)
	start = max(0, selected-visible+1)
	if top >= 0 && top <= max(0, n-visible) && selected >= top && selected < top+visible {
		start = top
	}
	return start, min(n, start+visible), cardHeight
}

// launcherCard is a's card, w cells wide and cardHeight rows tall. hovered
// is the pointer's independent cue, not the keyboard's selection.
func (m *Model) launcherCard(a appStatus, focused, hovered bool, w, cardHeight int) string {
	status, state := "READY", theme.Normal
	if a.Running {
		status, state = fmt.Sprintf("LIVE / PID %d", a.PID), theme.Success
	}
	return m.kit.AppCard(ui.AppCard{
		Name: a.Manifest.Name, ID: a.Manifest.ID, Version: a.Manifest.Version,
		Icon: a.Manifest.Icon, Status: status, State: state, Focused: focused, Hovered: hovered,
	}, ui.Bounds{Width: w, Height: cardHeight})
}

// renderLauncher is the launcher's lines in a body w cells wide and
// bodyHeight rows tall; launcherNode lays the same lines out as nodes.
func (m *Model) renderLauncher(w, bodyHeight int) []string {
	var lines []string
	filtered := m.filteredApps()
	selected := min(m.selected, max(0, len(filtered)-1))
	lines = append(lines, m.kit.Truncate(m.launcherSearch(), w))

	// A rejection keeps one of the shelf's rows; the cards below get the
	// rest, so the notice never pushes the layout past the body.
	cardsHeight, noticeRows := bodyHeight, 0
	if notice := m.launcherNoticeLine(w); notice != "" {
		lines = append(lines, notice)
		cardsHeight--
		noticeRows = 1
	}

	if len(m.apps) == 0 {
		notice := m.kit.Notice(ui.Notice{
			Title: "No apps installed", Message: "Your shelf is ready for its first app.", State: theme.Empty,
		}, ui.Bounds{Width: w, Height: max(1, bodyHeight-2-noticeRows)})
		lines = append(lines, strings.Split(notice, "\n")...)
		return lines
	}

	if len(filtered) == 0 {
		notice := m.kit.Notice(ui.Notice{
			Title: "No matching apps", Message: fmt.Sprintf("No apps match %q. Esc clears filter.", m.launcherQuery), State: theme.Empty,
		}, ui.Bounds{Width: w, Height: max(1, bodyHeight-2-noticeRows)})
		lines = append(lines, strings.Split(notice, "\n")...)
		return lines
	}

	start, end, cardHeight := launcherCards(selected, len(filtered), cardsHeight, m.launcherTop)
	for i := start; i < end; i++ {
		lines = append(lines, strings.Split(m.launcherCard(filtered[i], i == selected,
			m.hoverAt(hoverLauncherCard, filtered[i].Manifest.ID), w, cardHeight), "\n")...)
	}
	a := filtered[selected]
	if len(lines) < bodyHeight {
		lines = append(lines, m.kit.Muted(m.kit.Truncate(safe(a.Manifest.Description), w)))
	}
	if len(lines) < bodyHeight {
		lines = append(lines, m.kit.Muted(m.kit.Truncate("GRANT  "+safe(strings.Join(a.Manifest.Permissions, m.kit.Separator())), w)))
	}
	return lines
}

// selectApp selects the filtered app with ID id, as the arrow keys move to
// it, and reports whether the launcher lists it.
func (m *Model) selectApp(id string) bool {
	for i, a := range m.filteredApps() {
		if a.Manifest.ID == id {
			m.selected = i
			return true
		}
	}
	return false
}

// launchSelected launches the selected app, as Enter does in the launcher.
// It is a shelf launch: a rejection belongs in the shelf, not on the command
// line, and only its newest answer, from this visit to the shelf, may show.
func (m *Model) launchSelected() tea.Cmd {
	filtered := m.filteredApps()
	if len(filtered) == 0 || m.selected < 0 || m.selected >= len(filtered) {
		return nil
	}
	a := filtered[m.selected]
	m.launchSent++
	m.launchNotice, m.launchNoticeApp = "", ""
	return m.submitFrom("launch "+a.Manifest.ID, originShelfLaunch, m.launchSent, a.Manifest.ID, m.shelfEpoch)
}

// launcherNoticeLine is the shelf's launch rejection, w cells wide. The
// heading words carry it without color, so Monochrome and Plain read it too.
func (m *Model) launcherNoticeLine(w int) string {
	if m.launchNotice == "" || w <= 0 {
		return ""
	}
	return m.kit.Truncate(m.kit.StatusText("LAUNCH FAILED  "+m.launchNotice, theme.Error), w)
}

// showShelfLaunchFailure keeps a rejected launch in the shelf. Only the
// newest launch, from the visit to the shelf the user is still on, and for an
// app still listed there may set it; a late answer never pulls the user back
// or rewrites the filter and selection they moved on to.
func (m *Model) showShelfLaunchFailure(msg resultMsg) bool {
	if msg.originSeq != m.launchSent || msg.originEpoch != m.shelfEpoch {
		return false
	}
	if m.currentMode() != modeLauncher || m.presentation != nil {
		return false
	}
	listed := false
	for _, a := range m.apps {
		if a.Manifest.ID == msg.originApp {
			listed = true
			break
		}
	}
	if !listed {
		return false
	}
	m.launchNotice, m.launchNoticeApp = safe(msg.err.Error()), msg.originApp
	return true
}

func (m *Model) handleLauncherKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredApps()
	m.selected = min(m.selected, max(0, len(filtered)-1))
	if msg.Code == tea.KeyBackspace {
		// Backspace deletes regardless of modifiers, matching app-view
		// fields (Ctrl+Backspace included).
		r := []rune(m.launcherQuery)
		if len(r) > 0 {
			m.launcherQuery = string(r[:len(r)-1])
			m.selected = 0
		}
		return m, nil
	}
	switch msg.String() {
	case "up":
		m.selected = max(0, m.selected-1)
		return m, nil
	case "down":
		m.selected = min(max(0, len(filtered)-1), m.selected+1)
		return m, nil
	case "enter":
		if msg.IsRepeat {
			// A held key launches once, not per auto-repeat.
			return m, nil
		}
		return m, m.launchSelected()
	case "f3":
		if len(filtered) > 0 && m.selected < len(filtered) && !msg.IsRepeat {
			a := filtered[m.selected]
			if !a.Running {
				m.append(entry{viewText(a.Manifest.Name) + " is not running.", "error"})
				return m, nil
			}
			return m, m.openConfirm(stopAppConfirmation(a))
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
	case "ctrl+h":
		r := []rune(m.launcherQuery)
		if len(r) > 0 {
			m.launcherQuery = string(r[:len(r)-1])
			m.selected = 0
		}
		return m, nil
	}
	if text := safe(msg.Text); text != "" {
		m.launcherQuery += text
		m.selected = 0
		return m, nil
	}
	return m, nil
}
