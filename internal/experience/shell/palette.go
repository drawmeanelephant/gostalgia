package shell

import (
	"fmt"
	"strings"

	"bloomfx.dev/bloom"
	"bloomfx.dev/bloom/fx"
	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/experience/windows"
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
				Detail: fmt.Sprintf("%s%sPID %d (LIVE)", id, m.kit.Separator(), app.PID),
				Action: func(m *Model) tea.Cmd {
					return m.openWindow(app)
				},
			})
			items = append(items, paletteItem{
				Tag:    "APP",
				Title:  "Stop " + name,
				Detail: fmt.Sprintf("Graceful stop %s (PID %d)", id, app.PID),
				Action: func(m *Model) tea.Cmd {
					return m.openConfirm(stopAppConfirmation(app))
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

	// Windows. Close window and Move window are the keyboard's way to
	// Ctrl-W and Alt-arrows for terminals that send them elsewhere.
	if v := m.focusedView(); v != nil {
		id := windows.ID(v.id)
		items = append(items, paletteItem{
			Tag:    "WINDOW",
			Title:  "Close window",
			Detail: "Close the " + viewText(v.data.Title) + " window (Ctrl-W)",
			Action: func(m *Model) tea.Cmd { return m.closeWindow(id) },
		})
		if m.floatingWindows() {
			items = append(items, paletteItem{
				Tag:    "WINDOW",
				Title:  "Move window",
				Detail: "Move the " + viewText(v.data.Title) + " window with the arrow keys (Alt-arrows)",
				Action: func(m *Model) tea.Cmd { return m.startMovingWindow() },
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
			Action: func(m *Model) tea.Cmd { return m.switchScreen(screenHome, false) },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "apps",
			Detail: "Search and launch applications (F2)",
			Action: func(m *Model) tea.Cmd { return m.switchScreen(screenApps, false) },
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "prompt",
			Detail: "Switch to command prompt (Esc)",
			Action: func(m *Model) tea.Cmd { return m.switchScreen(screenPrompt, false) },
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
				m.clearTranscript()
				m.input = nil
				m.cursor = 0
				return nil
			},
		},
		paletteItem{
			Tag:    "CMD",
			Title:  "shutdown",
			Detail: "Shut down Gostalgia environment",
			Action: func(m *Model) tea.Cmd { return m.openConfirm(confirmation{kind: confirmShutdown}) },
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

// key is what tells the item from the others the palette lists.
func (it paletteItem) key() string { return it.Tag + "\x00" + it.Title + "\x00" + it.Detail }

// executePaletteItem closes the palette and runs its selected item, as
// Enter does.
func (m *Model) executePaletteItem() tea.Cmd {
	filtered := m.filteredPaletteItems()
	if len(filtered) > 0 && m.paletteSelected < len(filtered) {
		item := filtered[m.paletteSelected]
		m.closePalette()
		if item.Action != nil {
			return item.Action(m)
		}
	}
	m.closePalette()
	return nil
}

// runPaletteItem selects the listed item with key and runs it, if the
// palette still lists it.
func (m *Model) runPaletteItem(key string) tea.Cmd {
	for i, it := range m.filteredPaletteItems() {
		if it.key() == key {
			m.paletteSelected = i
			return m.executePaletteItem()
		}
	}
	return nil
}

// openPalette opens the palette and starts its finite opening reveal. Every
// entry path returns the command so the event that opened it schedules the
// frames; ReducedMotion renders it settled with no command.
func (m *Model) openPalette() tea.Cmd {
	m.paletteOpen = true
	m.paletteQuery = ""
	m.paletteSelected = 0
	m.previousMode = m.currentMode()
	// The layer replaces the screen beneath it; no old cue may survive.
	m.clearHoverAll()
	return m.startOverlayEntrance()
}

func (m *Model) closePalette() {
	m.paletteOpen = false
	m.paletteQuery = ""
	m.paletteSelected = 0
	m.clearHoverKind(hoverPaletteItem)
}

// paletteRect is where the palette goes on the w×h screen: centered
// horizontally, and vertically in the rows between the frame's top border
// and the prompt, so the prompt row's palette hint stays in view.
func paletteRect(w, h int) (y, width, height int) {
	rows := h - 4
	width, height = min(w-2, 66), min(rows, 14)
	return 1 + (rows-height)/2, width, height
}

// paletteLayer is the open palette as a modal layer over the screen. While
// its opening reveal is active the layer draws with that progress; the layer
// stays modal throughout, so input guards never depend on the visual mask.
func (m *Model) paletteLayer() bloom.LayerNode {
	y, w, h := paletteRect(m.width, m.height)
	box := m.withMouseAllowed(ui.Sized(func(b ui.Bounds) string { return m.renderPalette(b.Width, b.Height) }),
		m.paletteHits, m.paletteScroll, m.paletteMouseAllowed)
	layer := bloom.Layer(box).Anchor(bloom.Center, bloom.Start).Offset(0, y).Size(w, h).
		Modal().Backdrop(m.backdrop()...).ID(layerPalette)
	if progress, ok := m.overlayProgress(); ok {
		layer = layer.Transition(progress, fx.Iris{})
	}
	return layer
}

// renderPalette is the palette's box at exactly paletteW×paletteH.
func (m *Model) renderPalette(paletteW, paletteH int) string {
	filtered := m.filteredPaletteItems()
	selected := min(m.paletteSelected, max(0, len(filtered)-1))

	var inner []string
	inner = append(inner, m.kit.Heading("> ")+m.kit.Text(m.paletteQuery)+m.kit.Selection(" "))
	// A short palette spends no row on the spacer under the query.
	if paletteH-3 >= 4 {
		inner = append(inner, "")
	}

	if len(filtered) == 0 {
		inner = append(inner, m.kit.Muted("  "+m.kit.EmptyMarker()+" No matching commands, apps, or documents."))
	} else {
		visible := max(1, paletteH-3-len(inner))
		start := max(0, selected-visible+1)
		end := min(len(filtered), start+visible)

		for i := start; i < end; i++ {
			it := filtered[i]
			cursor := "  "
			if i == selected {
				cursor = m.kit.Theme().Focus.Marker + " "
			} else if m.hoverAt(hoverPaletteItem, it.key()) {
				// The pointer's cue occupies the same two cells as the
				// blank prefix, so the palette geometry never moves.
				cursor = ui.HoverMarker + " "
			}
			tagBadge := "[" + it.Tag + "] "
			detailText := "  " + it.Detail
			if i == selected {
				inner = append(inner, m.kit.Selection(m.kit.Truncate(cursor+tagBadge+it.Title, paletteW-4))+" "+m.kit.Muted(m.kit.Truncate(detailText, paletteW/2)))
			} else {
				inner = append(inner, m.kit.Heading(cursor+tagBadge)+m.kit.Text(it.Title)+m.kit.Muted(m.kit.Truncate(detailText, paletteW/2)))
			}
		}
	}

	return m.kit.Panel(ui.Panel{
		Title:   "COMMAND PALETTE (Ctrl-P / Esc)",
		Body:    strings.Join(inner, "\n"),
		Focused: true,
	}, ui.Bounds{Width: paletteW, Height: paletteH})
}

func (m *Model) handlePaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	filtered := m.filteredPaletteItems()
	m.paletteSelected = min(m.paletteSelected, max(0, len(filtered)-1))
	if msg.Code == tea.KeyBackspace {
		// Backspace deletes regardless of modifiers, matching app-view
		// fields (Ctrl+Backspace included).
		r := []rune(m.paletteQuery)
		if len(r) > 0 {
			m.paletteQuery = string(r[:len(r)-1])
			m.paletteSelected = 0
			m.clearHoverKind(hoverPaletteItem)
		}
		return m, nil
	}
	switch msg.String() {
	case "up":
		m.paletteSelected = max(0, m.paletteSelected-1)
		return m, nil
	case "down":
		m.paletteSelected = min(max(0, len(filtered)-1), m.paletteSelected+1)
		return m, nil
	case "enter":
		if msg.IsRepeat {
			// A held key executes once, not per auto-repeat (kitty event
			// types flag the repeats; see viewKey for the full rule).
			return m, nil
		}
		return m, m.executePaletteItem()
	case "esc", "ctrl+p":
		m.closePalette()
		return m, nil
	case "ctrl+h":
		r := []rune(m.paletteQuery)
		if len(r) > 0 {
			m.paletteQuery = string(r[:len(r)-1])
			m.paletteSelected = 0
			m.clearHoverKind(hoverPaletteItem)
		}
		return m, nil
	}
	if text := safe(msg.Text); text != "" {
		m.paletteQuery += text
		m.paletteSelected = 0
		m.clearHoverKind(hoverPaletteItem)
		return m, nil
	}
	return m, nil
}
