package shell

import (
	"fmt"
	"image"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// compactHelp is the frame's help row, stripped and without its padding.
func compactHelp(t *testing.T, m *Model) string {
	t.Helper()
	lines := checkRendered(t, m.View(), m.width, m.height)
	return strings.TrimRight(ansi.Strip(lines[len(lines)-1]), " ")
}

// cellSlice is the rows in r as rendered cells, without the columns outside
// r on those rows.
func cellSlice(lines []string, r image.Rectangle) []string {
	out := make([]string, 0, r.Dy())
	for _, line := range lines[r.Min.Y:r.Max.Y] {
		out = append(out, ansi.Truncate(ansi.TruncateLeft(line, r.Min.X, ""), r.Dx(), ""))
	}
	return out
}

// wholeHints fails when row shows the start of a binding without its whole
// action, as "F3 ST" did at 30x10 before #228.
func wholeHints(t *testing.T, row string, bindings ...[2]string) {
	t.Helper()
	for _, binding := range bindings {
		if strings.Contains(row, binding[0]) && !strings.Contains(row, binding[0]+" "+binding[1]) {
			t.Fatalf("help row shows a clipped %q hint: %q", binding[0], row)
		}
	}
}

// At exactly 30x10 the Apps shelf keeps the selected app's name and its
// LIVE/READY state, and its primary keys are complete hints rather than a
// cut-off fragment. Every built-in theme and Plain carry the same cues.
func TestMinimumLauncherShowsSelectedNameStateAndWholeHints(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.TrueColor} {
			t.Run(fmt.Sprintf("%s/%d", th.Name, mode), func(t *testing.T) {
				m := sized(fixtureModel(th, mode), minFrameWidth, minFrameHeight)
				m.setMode(modeLauncher)
				m.selected = 0 // Echo, live

				screen := strings.Join(checkRendered(t, m.View(), 30, 10), "\n")
				for _, want := range []string{"Echo", "LIVE"} {
					if !strings.Contains(screen, want) {
						t.Fatalf("live selection lost %q at 30x10:\n%s", want, screen)
					}
				}
				help := compactHelp(t, m)
				sep := m.kit.Separator()
				wantHelp := "Esc BACK" + sep + "Enter RUN" + sep + "F3 STOP"
				if help != wantHelp {
					t.Fatalf("live help = %q, want the complete primary hints %q", help, wantHelp)
				}

				m.selected = 1 // Notes, ready
				screen = strings.Join(checkRendered(t, m.View(), 30, 10), "\n")
				for _, want := range []string{"Notes", "READY"} {
					if !strings.Contains(screen, want) {
						t.Fatalf("ready selection lost %q at 30x10:\n%s", want, screen)
					}
				}
				help = compactHelp(t, m)
				wantHelp = "Esc BACK" + sep + "Enter RUN"
				if help != wantHelp {
					t.Fatalf("ready help = %q, want %q", help, wantHelp)
				}
				wholeHints(t, help, [2]string{"Esc", "BACK"}, [2]string{"Enter", "RUN"})
			})
		}
	}
}

// A rejected launch keeps one body row for its message at 30x10. The
// selected card must still carry both its name and its state, packed into
// the row that is left.
func TestMinimumLauncherKeepsNameAndStateWithLaunchRejection(t *testing.T) {
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%v", running), func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), minFrameWidth, minFrameHeight)
			m.setMode(modeLauncher)
			m.selected = 0
			m.launchNotice = "app: com.gostalgia.echo is already running or initializing (pid 7)"
			if !running {
				m.selected = 1 // Notes, ready
				m.launchNotice = "app: com.gostalgia.notes could not start"
			}

			body := bodyText(t, m)
			for _, want := range []string{"LAUNCH FAILED", m.filteredApps()[m.selected].Manifest.Name} {
				if !strings.Contains(body, want) {
					t.Fatalf("shelf lost %q beside the rejection:\n%s", want, body)
				}
			}
			state := "READY"
			if running {
				state = "LIVE"
			}
			if !strings.Contains(body, state) {
				t.Fatalf("shelf lost the %s state beside the rejection:\n%s", state, body)
			}
		})
	}
}

// The compact renderer and bloom's laid-out rows, which own the mouse hit
// geometry, draw the same launcher. The full 80x24 card keeps its border,
// identity detail, description and grants.
func TestLauncherRendererMatchesBloomRowsAndCardGeometry(t *testing.T) {
	for _, size := range [][2]int{{30, 10}, {80, 24}} {
		for _, notice := range []bool{false, true} {
			t.Run(fmt.Sprintf("%dx%d/notice=%v", size[0], size[1], notice), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), size[0], size[1])
				m.setMode(modeLauncher)
				m.selected = 0
				if notice {
					m.launchNotice = "app: com.gostalgia.echo is already running"
				}

				rects := checkFrame(t, m)
				body := rects[slotBody]
				rendered := m.renderLauncher(body.Dx(), body.Dy())
				drawn := cellSlice(checkRendered(t, m.View(), size[0], size[1]), body)
				for i, line := range rendered {
					if i >= len(drawn) || strings.TrimRight(line, " ") != strings.TrimRight(drawn[i], " ") {
						t.Fatalf("row %d: renderer %q != bloom %q", i, line, drawn[i])
					}
				}
				for _, line := range drawn[len(rendered):] {
					if strings.TrimSpace(line) != "" {
						t.Fatalf("bloom drew an extra row %q", line)
					}
				}

				// The clickable card rect covers the rows that show its
				// name and state, at both sizes.
				card, ok := m.solve().Rect(launcherCard("com.gostalgia.echo"))
				if !ok {
					t.Fatal("selected card has no mouse rect")
				}
				rows := strings.Join(cellSlice(checkRendered(t, m.View(), size[0], size[1]), card), "\n")
				for _, want := range []string{"Echo", "LIVE"} {
					if !strings.Contains(rows, want) {
						t.Fatalf("%dx%d card rect %v lacks %q:\n%s", size[0], size[1], card, want, rows)
					}
				}
			})
		}
	}
}

// Everything the compact key row promises still acts through the same
// paths at 30x10: filter and move, launch a ready app, open a live app's
// view, stop a live app through the confirmation, clear the filter and
// leave the shelf. Growing back to 80x24 restores the full card and hints.
func TestMinimumLauncherKeyboardControls(t *testing.T) {
	rec := &recordingCaller{}
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), minFrameWidth, minFrameHeight)
	m.client = rec
	m.setMode(modeLauncher)

	// Filter for a ready app and launch it with Enter.
	press(m, typed("Note"))
	if got := selectedAppID(m); got != "com.gostalgia.notes" {
		t.Fatalf("filter selected %q, want the ready Notes app", got)
	}
	_, cmd := m.Update(enterKey)
	runCmd(t, m, cmd)
	if got := calls(rec, "app/launch"); len(got) != 1 || got[0].params != `{"id":"com.gostalgia.notes"}` {
		t.Fatalf("Enter at 30x10 launched %v", got)
	}

	// Esc clears the filter and keeps the shelf; selection is back on the
	// live Echo app.
	press(m, escKey)
	if m.launcherQuery != "" || m.currentMode() != modeLauncher {
		t.Fatalf("first Esc = query %q in mode %v", m.launcherQuery, m.currentMode())
	}
	if got := selectedAppID(m); got != "com.gostalgia.echo" {
		t.Fatalf("filter clear selected %q, want Echo", got)
	}

	// F4 opens the live app's view.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF4})
	if !m.onWindows() {
		t.Fatal("F4 at 30x10 did not open the live app view")
	}
	press(m, escKey)
	if m.onWindows() || m.currentMode() != modeLauncher || m.FocusedWindow() != "com.gostalgia.echo" {
		t.Fatal("Esc did not return to the shelf with the window kept open")
	}

	// F3 stops the live app through the existing confirmation.
	m.Update(tea.KeyPressMsg{Code: tea.KeyF3})
	if m.confirm == nil || m.confirm.kind != confirmStopApp || m.confirm.appID != "com.gostalgia.echo" {
		t.Fatalf("F3 at 30x10 opened %+v, want the Echo stop confirmation", m.confirm)
	}
	press(m, tabKey)
	_, cmd = m.Update(enterKey)
	runCmd(t, m, cmd)
	if got := calls(rec, "app/stop"); len(got) != 1 || got[0].params != `{"id":"com.gostalgia.echo"}` {
		t.Fatalf("confirmed stop at 30x10 called %v", got)
	}

	// A second Esc leaves Apps for the prompt.
	press(m, escKey)
	if m.currentMode() != modePrompt {
		t.Fatalf("second Esc left the shelf in mode %v, want prompt", m.currentMode())
	}

	// Growing restores the full 80x24 card and its complete guidance. Echo was
	// just stopped above, so it shows READY; Clock remains LIVE.
	m.setMode(modeLauncher)
	sized(m, 80, 24)
	body := bodyText(t, m)
	for _, want := range []string{"Echo", "READY", "com.gostalgia.echo", "GRANT"} {
		if !strings.Contains(body, want) {
			t.Fatalf("80x24 shelf lost %q:\n%s", want, body)
		}
	}
	m.selected = 2 // Clock, still LIVE
	body = bodyText(t, m)
	for _, want := range []string{"Clock", "LIVE / PID 31", "com.gostalgia.clock", "GRANT"} {
		if !strings.Contains(body, want) {
			t.Fatalf("80x24 live shelf lost %q:\n%s", want, body)
		}
	}
	help := compactHelp(t, m)
	for _, want := range []string{"Enter OPEN", "F3 STOP", "F4 VIEW", "F5 TASKS"} {
		if !strings.Contains(help, want) {
			t.Fatalf("80x24 help lost %q: %q", want, help)
		}
	}

	// Widths 31-32 keep Enter OPEN and F3 STOP whole with Esc BACK; width 33+
	// restores Esc PROMPT beside Enter OPEN and F3 STOP.
	sep := m.kit.Separator()
	sized(m, 31, minFrameHeight)
	if got, want := compactHelp(t, m), "Esc BACK"+sep+"Enter OPEN"+sep+"F3 STOP"; got != want {
		t.Fatalf("31x10 live help = %q, want %q", got, want)
	}
	sized(m, 33, minFrameHeight)
	if got, want := compactHelp(t, m), "Esc PROMPT"+sep+"Enter OPEN"+sep+"F3 STOP"; got != want {
		t.Fatalf("33x10 live help = %q, want %q", got, want)
	}
}
