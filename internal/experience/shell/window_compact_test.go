package shell

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/sdk"
)

// compendiumNoteData builds a version-3 snapshot matching Compendium's note
// editor: linked items, a multiline Markdown draft, a single-line Rename
// field, eight actions, and a Rendered scroll region.
func compendiumNoteData(bodyLines int) sdk.View {
	lines := make([]string, bodyLines)
	for i := range lines {
		lines[i] = fmt.Sprintf("note line %02d", i+1)
	}
	rendered := make([]string, max(bodyLines, 8))
	for i := range rendered {
		rendered[i] = fmt.Sprintf("rendered %02d", i+1)
	}
	return sdk.View{
		Version:  3,
		Instance: "launch",
		Title:    "Compendium — Field Guide",
		State:    sdk.ViewReady,
		Items: []sdk.Item{
			{ID: "link-1", Label: "→ Architecture", Detail: "Linked note. Follow opens it."},
			{ID: "tag-1", Label: "#notes", Detail: "3 notes"},
		},
		Fields: []sdk.Field{
			{ID: "body", Label: "Markdown", Value: strings.Join(lines, "\n"), Multiline: true},
			{ID: "rename", Label: "Rename to"},
		},
		Actions: []sdk.Action{
			{ID: "save", Label: "Save"},
			{ID: "follow", Label: "Follow / create link"},
			{ID: "reload", Label: "Reload saved version"},
			{ID: "rename", Label: "Rename"},
			{ID: "trash", Label: "Move to trash"},
			{ID: "export_note", Label: "Export"},
			{ID: "today", Label: "Today"},
			{ID: "home", Label: "Back"},
		},
		Region: &sdk.Region{
			ID:    "rendered",
			Label: "Rendered",
			Text:  strings.Join(rendered, "\n"),
		},
		Status: "Saved · 1 links (0 missing) · 0 backlinks",
	}
}

// TestCompactWindowPrioritizesFocusedControlAndCuesAtMinimum verifies the
// 30x10 usability contract across every built-in theme and Plain mode:
// non-essential frame chrome yields so the window body gets five rows,
// the truncated inline hint row is dropped, editing context and navigation
// cues stay visible together, and every focus stop is reachable and visible.
func TestCompactWindowPrioritizesFocusedControlAndCuesAtMinimum(t *testing.T) {
	for _, th := range builtinThemes() {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.TrueColor} {
			t.Run(fmt.Sprintf("%s/%d", th.Name, mode), func(t *testing.T) {
				var dispatched []sdk.ActionRequest
				m := sized(fixtureModel(th, mode), minFrameWidth, minFrameHeight)
				m.client = callerFunc(func(_ context.Context, method string, params, out any) error {
					if strings.HasSuffix(method, "/action") {
						dispatched = append(dispatched, params.(sdk.ActionRequest))
					}
					return writeJSON(out, compendiumNoteData(6))
				})
				data := compendiumNoteData(6)
				v := m.showWindow(&appView{
					id:       "com.gostalgia.compendium",
					data:     data,
					instance: data.Instance,
					version:  3,
					values:   map[string]string{"body": data.Fields[0].Value},
				})

				rects := checkFrame(t, m)
				if rects[slotBody].Dy() != minWindowBodyRows {
					t.Fatalf("30x10 app body height = %d, want %d", rects[slotBody].Dy(), minWindowBodyRows)
				}
				if _, ok := rects[slotBadge]; ok {
					t.Fatal("30x10 app window kept non-essential badge row")
				}
				if _, ok := rects[slotPrompt]; ok {
					t.Fatal("30x10 app window kept non-editable prompt row")
				}

				lines := checkRendered(t, m.View(), minFrameWidth, minFrameHeight)
				body := strings.Join(lines[rects[slotBody].Min.Y:rects[slotBody].Max.Y], "\n")
				help := strings.TrimRight(lines[len(lines)-1], " ")
				if mode == ui.Plain && strings.Contains(m.View().Content, "\x1b") {
					t.Fatal("Plain mode emitted ANSI escapes")
				}
				if th.Name == theme.Monochrome().Name {
					// App-owned title/status Unicode stays verbatim; shell-owned
					// frame, tabs, footer, and help chrome must stay ASCII.
					shellChrome := strings.Join([]string{
						lines[0], lines[1], lines[rects[slotTabs].Min.Y],
						lines[rects[slotBody].Max.Y-1], lines[len(lines)-2], lines[len(lines)-1],
					}, "\n")
					requireMonochromeChromeASCII(t, shellChrome)
				}

				// Editing context: title, focused Markdown label with active
				// tail lines, and navigation/action cues are visible together
				// without the truncated inline hint row.
				for _, want := range []string{"Compendium", "Markdown (5-6/6):", "note line 05", "note line 06", "Tab 1/11", "[Save]"} {
					if !strings.Contains(body, want) {
						t.Fatalf("30x10 body missing %q:\n%s", want, body)
					}
				}
				if strings.Contains(body, "Tab focus") {
					t.Fatalf("30x10 body still shows redundant inline hints:\n%s", body)
				}
				for _, want := range []string{"Esc BACK", "Tab NEXT"} {
					if !strings.Contains(help, want) {
						t.Fatalf("30x10 help missing whole hint %q: %q", want, help)
					}
				}
				wholeHints(t, help, [2]string{"Esc", "BACK"}, [2]string{"Tab", "NEXT"}, [2]string{"Enter", "GO"})

				// Typing appends to the active line and stays visible immediately.
				typeText(m, "!")
				if got := bodyText(t, m); !strings.Contains(got, "note line 06!") {
					t.Fatalf("newly typed text not visible in 30x10 editor:\n%s", got)
				}

				// Tab reaches Rename, every action including Save and Back,
				// and the Rendered region — each visible when focused.
				press(m, tea.KeyPressMsg{Code: tea.KeyTab})
				if got := bodyText(t, m); !strings.Contains(got, "Rename to:") || !strings.Contains(got, "[Save]") {
					t.Fatalf("Tab to Rename lost focused field or adjacent Save action:\n%s", got)
				}
				if got := compactHelp(t, m); !strings.Contains(got, "Enter GO") {
					t.Fatalf("single-line field help missing Enter GO: %q", got)
				}

				press(m, tea.KeyPressMsg{Code: tea.KeyTab})
				if got := bodyText(t, m); !strings.Contains(got, "[Save]") || !strings.Contains(got, "Tab 3/11") {
					t.Fatalf("Tab to Save lost focused action or position cue:\n%s", got)
				}
				_, cmd := m.Update(keyEnter)
				if cmd == nil {
					t.Fatal("Enter on focused Save action at 30x10 did not dispatch")
				}
				m.Update(cmd())
				if len(dispatched) != 1 || dispatched[0].Action != "save" || !strings.HasSuffix(dispatched[0].Values["body"], "note line 06!") {
					t.Fatalf("Save dispatch at 30x10 = %+v", dispatched)
				}

				// Advance through the remaining actions to Back and then Rendered.
				for i := 3; i < len(v.data.Fields)+len(v.data.Actions); i++ {
					press(m, tea.KeyPressMsg{Code: tea.KeyTab})
					action := v.data.Actions[i-len(v.data.Fields)]
					want := "[" + action.Label + "]"
					if got := bodyText(t, m); !strings.Contains(got, want) {
						t.Fatalf("30x10 Tab stop %d (%s) fell outside viewport:\n%s", i, want, got)
					}
				}
				press(m, tea.KeyPressMsg{Code: tea.KeyTab})
				if !v.regionFocused() {
					t.Fatalf("Tab did not reach Rendered region: focus=%d", v.focus)
				}
				if got := bodyText(t, m); !strings.Contains(got, "Rendered") || !strings.Contains(got, "rendered ") {
					t.Fatalf("30x10 focused region lost its label or text:\n%s", got)
				}

				// Up/Down reaches items and advertises item navigation + Tab return.
				press(m, tea.KeyPressMsg{Code: tea.KeyTab}) // wrap back to Markdown
				press(m, keyDown)
				if !v.itemFocus {
					t.Fatal("Down did not move to items")
				}
				if got := bodyText(t, m); !strings.Contains(got, "#notes") || !strings.Contains(got, "2/2") {
					t.Fatalf("30x10 item focus lost selected item or position:\n%s", got)
				}
			})
		}
	}
}

// TestWindowMultilineFocusAndOverflowCuesAt80x24 verifies that at 80x24 a
// long multiline field does not strand later controls below the viewport and
// that the footer clearly signals when more controls continue off-screen.
func TestWindowMultilineFocusAndOverflowCuesAt80x24(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	data := compendiumNoteData(14)
	v := m.showWindow(&appView{
		id:       "com.gostalgia.compendium",
		data:     data,
		instance: data.Instance,
		version:  3,
		values:   map[string]string{"body": data.Fields[0].Value},
	})

	rects := checkFrame(t, m)
	if _, ok := rects[slotBadge]; !ok {
		t.Fatal("80x24 window unexpectedly collapsed badge")
	}
	if _, ok := rects[slotPrompt]; !ok {
		t.Fatal("80x24 window unexpectedly collapsed prompt")
	}

	view := ansi.Strip(m.View().Content)
	for _, want := range []string{"[X]", "Markdown (7-14/14):", "note line 14", "Tab 1/11", "[Save]", "↑1 ↓15"} {
		if !strings.Contains(view, want) {
			t.Fatalf("80x24 note view missing %q:\n%s", want, view)
		}
	}

	// Every field and action after the 14-line multiline field must scroll
	// cleanly into the viewport when focused.
	press(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if view = ansi.Strip(m.View().Content); !strings.Contains(view, "› Rename to:") {
		t.Fatalf("80x24 Rename field fell outside viewport after multiline field:\n%s", view)
	}
	for i, action := range v.data.Actions {
		press(m, tea.KeyPressMsg{Code: tea.KeyTab})
		want := "› [" + action.Label + "]"
		if view = ansi.Strip(m.View().Content); !strings.Contains(view, want) {
			t.Fatalf("80x24 action %d (%s) fell outside viewport:\n%s", i, want, view)
		}
	}
	press(m, tea.KeyPressMsg{Code: tea.KeyTab})
	if !v.regionFocused() {
		t.Fatal("80x24 Tab did not reach Rendered region")
	}
	if view = ansi.Strip(m.View().Content); !strings.Contains(view, "Rendered") || !strings.Contains(view, "rendered 01") || !strings.Contains(view, "↑10 more") {
		t.Fatalf("80x24 focused region lost header, text, or overflow cue:\n%s", view)
	}
}

// TestWindowLongFieldLabelKeepsDraftVisible verifies that a verbose field
// label (such as Compendium's search/command prompt) truncates before the
// colon so typed input remains visible at both 30x10 and 80x24.
func TestWindowLongFieldLabelKeepsDraftVisible(t *testing.T) {
	for _, th := range []theme.Theme{theme.Nostalgia(), theme.Monochrome()} {
		for _, size := range [][2]int{{30, 10}, {80, 24}} {
			t.Run(fmt.Sprintf("%s/%dx%d", th.Name, size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(th, ui.Plain), size[0], size[1])
				m.showWindow(&appView{
					id: "com.gostalgia.compendium",
					data: sdk.View{
						Version:  3,
						Instance: "launch",
						Title:    "Compendium",
						State:    sdk.ViewReady,
						Fields: []sdk.Field{{
							ID:    "q",
							Label: "Search or command (today, new <title>, open <title>, graph, tags, trash, help)",
						}},
						Actions: []sdk.Action{{ID: "run", Label: "Go"}},
						Status:  "0 notes",
					},
					instance: "launch",
					version:  3,
					values:   map[string]string{"q": "today"},
				})
				body := bodyText(t, m)
				if !strings.Contains(body, ": today") {
					t.Fatalf("%dx%d long field label hid typed value:\n%s", size[0], size[1], body)
				}
				if th.Name == theme.Monochrome().Name {
					requireMonochromeChromeASCII(t, body)
				}
			})
		}
	}
}

// TestCompactWindowPaletteAndBelowMinimumRoundTrip verifies that opening the
// palette over a 30x10 app window restores the PALETTE> prompt row, closing
// it returns to the 5-row compact window, and shrinking below 30x10 keeps
// the existing sizeNote fallback and restores state when grown back.
func TestCompactWindowPaletteAndBelowMinimumRoundTrip(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), minFrameWidth, minFrameHeight)
	data := compendiumNoteData(4)
	v := m.showWindow(&appView{
		id:       "com.gostalgia.compendium",
		data:     data,
		instance: data.Instance,
		version:  3,
		values:   map[string]string{"body": data.Fields[0].Value},
	})
	typeText(m, " kept")
	before := m.View().Content

	m.openPalette()
	settleFiniteMotion(m)
	rects := checkFrame(t, m)
	if _, ok := rects[slotPrompt]; !ok {
		t.Fatal("palette over 30x10 app window dropped slotPrompt")
	}
	if screen := strings.Join(checkRendered(t, m.View(), 30, 10), "\n"); !strings.Contains(screen, "PALETTE>") {
		t.Fatalf("palette prompt missing at 30x10:\n%s", screen)
	}
	press(m, keyEsc)
	if m.View().Content != before {
		t.Fatalf("closing palette did not restore compact 30x10 app window:\n%s", m.View().Content)
	}

	// Below 30x10 still falls back to sizeNote and preserves window state.
	for _, small := range [][2]int{{29, 10}, {30, 9}} {
		sized(m, small[0], small[1])
		lines := checkRendered(t, m.View(), small[0], small[1])
		if !strings.HasPrefix(lines[0], "GOSTALGIA") || !strings.HasPrefix(lines[1], "Resize to 30") {
			t.Fatalf("%dx%d did not show sizeNote:\n%s", small[0], small[1], strings.Join(lines, "\n"))
		}
	}

	// Growing to 30x10 restores the compact window; growing to 80x24 restores
	// full chrome and floating window framing with the edited draft intact.
	sized(m, 30, 10)
	if got := checkFrame(t, m)[slotBody].Dy(); got != minWindowBodyRows {
		t.Fatalf("returned to 30x10 with body height %d, want %d", got, minWindowBodyRows)
	}
	if !strings.Contains(bodyText(t, m), "note line 04 kept") {
		t.Fatalf("30x10 recovery lost draft:\n%s", bodyText(t, m))
	}
	sized(m, 80, 24)
	full := checkFrame(t, m)
	if _, ok := full[slotBadge]; !ok {
		t.Fatal("80x24 recovery did not restore badge")
	}
	if _, ok := full[slotPrompt]; !ok {
		t.Fatal("80x24 recovery did not restore prompt")
	}
	if v.values["body"] != data.Fields[0].Value+" kept" || !strings.Contains(ansi.Strip(m.View().Content), "[X]") {
		t.Fatalf("80x24 recovery lost draft or floating frame: draft=%q", v.values["body"])
	}
}
