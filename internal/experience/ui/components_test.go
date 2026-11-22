package ui

import (
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
)

var update = flag.Bool("update", false, "update component golden snapshots")

func TestComponentSnapshots(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	var views []string
	views = append(views,
		"PANEL\n"+k.Panel(Panel{Title: "Welcome home", Body: "Cream, amber, and slate.\nA place to make things."}, Bounds{32, 6}),
		"FOCUS\n"+k.Panel(Panel{Title: "Notes", Body: "A familiar workspace.", Focused: true}, Bounds{32, 5}),
		"TABS\n"+k.Tabs([]Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Settings", Disabled: true}}, 1, 32),
		"APP CARD\n"+k.AppCard(AppCard{Name: "Notes", ID: "com.gostalgia.notes", Version: "0.1.0",
			Description: "Keep a thought close to home.", Status: "LIVE / PID 7", State: theme.Success, Focused: true}, Bounds{36, 7}),
		"DIALOG\n"+k.Dialog(Dialog{Title: "Leave workspace?", Body: "Your apps will stop.",
			Actions: []Action{{Label: "Cancel"}, {Label: "Leave"}, {Label: "Save", Disabled: true}}, Active: 0}, Bounds{40, 12}),
		"PROGRESS\n"+k.Progress(Progress{Label: "Copy", Value: 0.5}, 32),
		"BUSY PROGRESS\n"+k.Progress(Progress{Label: "Discover", Indeterminate: true, State: theme.Busy}, 32),
		"HELP\n"+k.HelpBar([]Binding{{Key: "Esc", Help: "back"}, {Key: "Enter", Help: "open"}, {Key: "F3", Help: "stop", Disabled: true}}, 40),
	)
	for _, state := range []theme.State{theme.Normal, theme.Disabled, theme.Busy, theme.Success, theme.Error, theme.Empty} {
		views = append(views, strings.ToUpper(string(state))+"\n"+k.Badge("", state, 32)+"\n"+
			k.Notice(Notice{State: state}, Bounds{36, 6}))
	}
	custom := theme.Midnight()
	custom.Spacing.PanelX = 0
	custom.Focus.Marker = "*"
	custom.Borders.Panel = lipgloss.RoundedBorder()
	views = append(views, "CUSTOM TOKENS\n"+New(custom, Plain).Panel(
		Panel{Title: "Custom", Body: "No horizontal padding.", Focused: true}, Bounds{32, 5}))
	snapshot(t, "components", strings.Join(views, "\n\n"))
}

func TestAccessibleComponentSnapshots(t *testing.T) {
	for _, th := range []theme.Theme{theme.Monochrome(), theme.HighContrast()} {
		t.Run(th.Name, func(t *testing.T) {
			k := New(th, Plain)
			var views []string
			views = append(views,
				"PANEL\n"+k.Panel(Panel{Title: "Welcome home", Body: "Cream, amber, and slate.\nA place to make things."}, Bounds{32, 6}),
				"FOCUS\n"+k.Panel(Panel{Title: "Notes", Body: "A familiar workspace.", Focused: true}, Bounds{32, 5}),
				"TABS\n"+k.Tabs([]Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Settings", Disabled: true}}, 1, 32),
				"APP CARD\n"+k.AppCard(AppCard{Name: "Notes", ID: "com.gostalgia.notes", Version: "0.1.0",
					Description: "Keep a thought close to home.", Status: "LIVE / PID 7", State: theme.Success, Focused: true}, Bounds{36, 7}),
				"DIALOG\n"+k.Dialog(Dialog{Title: "Leave workspace?", Body: "Your apps will stop.",
					Actions: []Action{{Label: "Cancel"}, {Label: "Leave"}, {Label: "Save", Disabled: true}}, Active: 0}, Bounds{40, 12}),
				"PROGRESS\n"+k.Progress(Progress{Label: "Copy", Value: 0.5}, 32),
				"BUSY PROGRESS (REDUCED MOTION)\n"+k.Progress(Progress{Label: "Discover", Indeterminate: true, State: theme.Busy}, 32),
				"HELP\n"+k.HelpBar([]Binding{{Key: "Esc", Help: "back"}, {Key: "Enter", Help: "open"}, {Key: "F3", Help: "stop", Disabled: true}}, 40),
			)
			for _, state := range []theme.State{theme.Normal, theme.Disabled, theme.Busy, theme.Success, theme.Error, theme.Empty} {
				views = append(views, strings.ToUpper(string(state))+"\n"+k.Badge("", state, 32)+"\n"+
					k.Notice(Notice{State: state}, Bounds{36, 6}))
			}
			snapshot(t, "accessible-"+th.Name, strings.Join(views, "\n\n"))
		})
	}
}

func TestColorSnapshots(t *testing.T) {
	var views []string
	for _, th := range []theme.Theme{theme.Nostalgia(), theme.Midnight()} {
		for _, mode := range []ColorMode{Plain, ANSI256, TrueColor} {
			k := New(th, mode)
			views = append(views, th.Name+" "+[]string{"plain", "ansi256", "truecolor"}[mode]+"\n"+
				k.Panel(Panel{Title: "Focus", Body: "界 e\u0301 👩‍💻", Focused: true}, Bounds{16, 4}))
			for _, state := range []theme.State{theme.Disabled, theme.Busy, theme.Success, theme.Error, theme.Empty} {
				views = append(views, k.Badge("", state, 16))
			}
		}
	}
	snapshot(t, "colors", strings.ReplaceAll(strings.Join(views, "\n"), "\x1b", `\x1b`))
}

// TestThemeChromeSnapshots pins the same populated header and selected
// control in every built-in theme, plus a separate Plain render. The styled
// snapshots retain their escape sequences so a color regression cannot hide
// behind identical stripped text.
func TestThemeChromeSnapshots(t *testing.T) {
	cases := []struct {
		name  string
		theme theme.Theme
		mode  ColorMode
	}{
		{"nostalgia", theme.Nostalgia(), ANSI256},
		{"midnight-ansi256", theme.Midnight(), ANSI256},
		{"midnight-truecolor", theme.Midnight(), TrueColor},
		{"monochrome", theme.Monochrome(), Plain},
		{"high-contrast", theme.HighContrast(), ANSI256},
		{"high-contrast-light", theme.HighContrastLight(), ANSI256},
		{"plain", theme.Midnight(), Plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := New(tc.theme, tc.mode)
			header := k.Tabs([]Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Prompt"}}, 1, 40)
			selected := k.AppCard(AppCard{
				Name: "Echo", ID: "com.gostalgia.echo", Version: "0.1.0",
				Description: "A friendly first app.", Status: "LIVE / PID 7",
				State: theme.Success, Focused: true,
			}, Bounds{40, 6})
			view := "HEADER\n" + header + "\nSELECTED\n" + selected
			if tc.mode == Plain {
				if strings.Contains(view, "\x1b") {
					t.Fatalf("plain render contains ANSI styling: %q", view)
				}
			} else {
				// A low base guard for every theme: Nostalgia's ANSI256
				// downsampling is deliberately warmer and lighter than its
				// TrueColor header. Midnight's own 4.5:1 floor lives in
				// TestMidnightChromeProfilesAreLegibleAndTeal.
				headerFG, headerBG := k.resolve(tc.theme.Palette.HeaderText), k.resolve(tc.theme.Palette.Header)
				if got := contrastRatio(headerFG, headerBG); got < 3 {
					t.Errorf("header labels contrast %.2f:1 in %s", got, tc.name)
				}
				focusFG, focusBG := k.resolve(tc.theme.Focus.Text), k.resolve(tc.theme.Focus.Fill)
				if got := contrastRatio(focusFG, focusBG); got < 3 {
					t.Errorf("selected control contrast %.2f:1 in %s", got, tc.name)
				}
			}
			if tc.theme.Name == theme.Monochrome().Name && !strings.Contains(selected, "> Echo") {
				t.Fatalf("monochrome lost its ASCII focus marker:\n%s", selected)
			}
			snapshot(t, "chrome-"+tc.name, strings.ReplaceAll(view, "\x1b", `\x1b`))
		})
	}
}

func TestComponentsStayInsideOuterBounds(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("界e\u0301👩‍💻", 100)
	for _, th := range []theme.Theme{theme.Nostalgia(), theme.Midnight()} {
		for _, mode := range []ColorMode{Plain, ANSI256, TrueColor} {
			k := New(th, mode)
			for _, bounds := range []Bounds{{-1, 2}, {0, 0}, {1, 1}, {2, 2}, {2, 4}, {3, 3}, {3, 5}, {4, 4}, {4, 6},
				{12, 8}, {30, 10}, {80, 24}, {120, 40}, {240, 80}} {
				for _, state := range []theme.State{theme.Normal, theme.Disabled, theme.Busy, theme.Success, theme.Error, theme.Empty} {
					assertRectangle(t, k.Panel(Panel{Title: long, Body: long + "\n" + long, State: state, Focused: true}, bounds), bounds)
					assertRectangle(t, k.AppCard(AppCard{Name: long, ID: long, Description: long, Icon: long, State: state, Focused: true}, bounds), bounds)
					assertRectangle(t, k.Dialog(Dialog{Title: long, Body: long, State: state,
						Actions: []Action{{Label: long}, {Label: long, Disabled: true}}}, bounds), bounds)
					assertRectangle(t, k.Notice(Notice{Title: long, Message: long, State: state}, bounds), bounds)
				}
				lineBounds := Bounds{bounds.Width, 1}
				assertRectangle(t, k.Tabs([]Tab{{Label: long}, {Label: long, Disabled: true}}, 0, bounds.Width), lineBounds)
				assertRectangle(t, k.HelpBar([]Binding{{Key: long, Help: long}}, bounds.Width), lineBounds)
				for _, value := range []float64{-1, 0, 0.5, 1, 2, math.NaN(), math.Inf(1), math.Inf(-1)} {
					assertRectangle(t, k.Progress(Progress{Label: long, Value: value}, bounds.Width), lineBounds)
					assertRectangle(t, k.Progress(Progress{Label: long, Value: value, Indeterminate: true}, bounds.Width), lineBounds)
				}
				if ansi.StringWidth(k.Badge(long, theme.Error, bounds.Width)) > max(0, bounds.Width) {
					t.Fatal("badge exceeded bounds")
				}
			}
		}
	}
}

func TestFocusDisabledTabsAndDialogActions(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	tabs := []Tab{{Label: "Home"}, {Label: "Apps"}, {Label: "Notes"}, {Label: "Settings"}}
	if view := k.Tabs(tabs, 3, 12); !strings.Contains(view, "[Settings]") || strings.Contains(view, "Home") {
		t.Fatalf("active tab not kept visible: %q", view)
	}
	tabs[3].Disabled = true
	if view := k.Tabs(tabs, 3, 12); !strings.Contains(view, "(Settings)") || strings.Contains(view, "[Settings]") {
		t.Fatalf("disabled tab was highlighted: %q", view)
	}
	panel := k.Panel(Panel{Title: "Unavailable", State: theme.Disabled, Focused: true}, Bounds{24, 4})
	if strings.Contains(panel, theme.Nostalgia().Focus.Marker) {
		t.Fatal("disabled panel has a focus marker")
	}
	dialog := k.Dialog(Dialog{Title: "Confirm", Body: strings.Repeat("Long body. ", 100),
		Actions: []Action{{Label: "Cancel"}, {Label: "OK"}}, Active: 1}, Bounds{24, 8})
	if !strings.Contains(dialog, "[OK]") {
		t.Fatalf("dialog clipped actions:\n%s", dialog)
	}
	if view := k.Tabs(nil, -1, 10); view != strings.Repeat(" ", 10) {
		t.Fatalf("empty tabs = %q", view)
	}
}

func TestCompactAppCardKeepsNameAndStatus(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	for _, height := range []int{1, 2, 3} {
		for _, state := range []struct {
			status string
			state  theme.State
		}{{"LIVE / PID 7", theme.Success}, {"READY", theme.Normal}} {
			view := k.AppCard(AppCard{Name: "Echo", Status: state.status, State: state.state, Focused: true}, Bounds{24, height})
			assertRectangle(t, view, Bounds{24, height})
			if !strings.Contains(view, "› Echo") || !strings.Contains(view, state.status) {
				t.Fatalf("%d-row compact card hid name or actual status:\n%s", height, view)
			}
		}
	}
	// A name longer than the cells the state leaves is cut, but the state
	// keeps its whole label.
	view := k.AppCard(AppCard{Name: "A very long application name", Status: "LIVE / PID 1234", State: theme.Success}, Bounds{24, 1})
	if !strings.Contains(view, "LIVE / PID 1234") || strings.Contains(view, "application name") {
		t.Fatalf("one-row card did not prioritize state:\n%s", view)
	}
}

// HelpBar draws whole key/action hints only: a binding that does not fit is
// dropped, never cut mid-action.
func TestHelpBarDropsHintsThatWouldBeCut(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	bindings := []Binding{{Key: "Esc", Help: "BACK"}, {Key: "Enter", Help: "RUN"}, {Key: "F3", Help: "STOP"}}
	whole := []string{"Esc BACK", "Enter RUN", "F3 STOP"}
	for width := 1; width <= 40; width++ {
		var want string
		for n := len(whole); n >= 0; n-- {
			candidate := strings.Join(whole[:n], k.Separator())
			if ansi.StringWidth(candidate) <= width {
				want = candidate
				break
			}
		}
		if got := strings.TrimRight(k.HelpBar(bindings, width), " "); got != want {
			t.Fatalf("width %d: HelpBar = %q, want whole hints %q", width, got, want)
		}
	}
}

func TestProgressReportsActualState(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	for value, percentage := range map[float64]string{-1: "0%", 0.5: "50%", 2: "100%"} {
		if view := k.Progress(Progress{Value: value}, 24); !strings.Contains(view, percentage) {
			t.Fatalf("progress %v = %q", value, view)
		}
	}
	if view := k.Progress(Progress{Value: 0.9, Indeterminate: true, State: theme.Busy}, 32); strings.Contains(view, "%") || !strings.Contains(view, "WORKING") {
		t.Fatalf("indeterminate progress fabricated a percentage: %q", view)
	}
}

func TestRenderingIsExplicitAndConcurrent(t *testing.T) {
	th := theme.Nostalgia()
	k := New(th, TrueColor)
	panel := Panel{Title: "Home", Body: "Hello", Focused: true}
	want := k.Panel(panel, Bounds{24, 5})
	th.Focus.Marker = "*"
	if got := k.Panel(panel, Bounds{24, 5}); got != want {
		t.Fatal("kit did not copy theme")
	}
	for key, value := range map[string]string{"TERM": "dumb", "COLORTERM": "", "NO_COLOR": "1", "COLORFGBG": "0;15"} {
		t.Setenv(key, value)
	}
	if got := New(theme.Nostalgia(), TrueColor).Panel(panel, Bounds{24, 5}); got != want {
		t.Fatal("host environment changed explicit rendering")
	}
	if strings.Contains(New(theme.Midnight(), Plain).Panel(panel, Bounds{24, 5}), "\x1b") {
		t.Fatal("plain rendering contains ANSI")
	}
	dark := New(theme.Midnight(), TrueColor).Panel(panel, Bounds{24, 5})
	if want == dark || ansi.Strip(want) != ansi.Strip(dark) {
		t.Fatal("palette must change colors without changing layout")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := k.Panel(panel, Bounds{24, 5}); got != want {
				t.Error("shared kit rendered inconsistently")
			}
		}()
	}
	wg.Wait()
}

func TestCompositionRestoresOuterStyle(t *testing.T) {
	k := New(theme.Nostalgia(), TrueColor)
	for _, view := range []string{
		k.Text(k.Heading("Hello") + " home."),
		k.HelpBar([]Binding{{Key: "Esc", Help: "back"}, {Key: "F3", Help: "stop", Disabled: true}}, 32),
		k.Tabs([]Tab{{Label: "Home"}, {Label: "Apps"}}, 1, 32),
		k.Panel(Panel{Title: "Home", Body: k.Badge("ONLINE", theme.Success, 12) + " guest"}, Bounds{32, 5}),
		k.Dialog(Dialog{Title: "Confirm", Body: "Leave?", Actions: []Action{{Label: "Cancel"}, {Label: "OK"}}}, Bounds{40, 12}),
	} {
		parts := strings.Split(view, "\x1b[0m")
		for _, following := range parts[1:] {
			if following != "" && !strings.HasPrefix(following, "\x1b[") && !strings.HasPrefix(following, "\n") {
				t.Fatalf("full reset exposed unstyled content: %q", following)
			}
		}
	}
}

func TestComponentLabelsFilterTerminalControls(t *testing.T) {
	k := New(theme.Nostalgia(), Plain)
	text := "hello\x1b]52;c;unsafe\a\x1b[2J\r"
	for _, view := range []string{
		k.Panel(Panel{Title: text}, Bounds{24, 4}),
		k.AppCard(AppCard{Name: text, ID: text, Version: text, Description: text, Icon: text, Status: text}, Bounds{24, 7}),
		k.Dialog(Dialog{Title: text, Body: text, Actions: []Action{{Label: text}}}, Bounds{24, 8}),
		k.Tabs([]Tab{{Label: text}}, 0, 24),
		k.Notice(Notice{Title: text, Message: text}, Bounds{24, 6}),
		k.Badge(text, theme.Error, 24),
		k.Progress(Progress{Label: text}, 24),
		k.HelpBar([]Binding{{Key: text, Help: text}}, 24),
	} {
		if strings.ContainsAny(view, "\x1b\a\r") || strings.Contains(view, "unsafe") {
			t.Fatalf("component rendered terminal controls: %q", view)
		}
	}
}

func TestWindowStyleAndMutedChrome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		th        theme.Theme
		mode      ColorMode
		focusedTL string
		unfocusTL string
	}{
		{"nostalgia plain", theme.Nostalgia(), Plain, "┏", "┌"},
		{"nostalgia ansi256", theme.Nostalgia(), ANSI256, "┏", "┌"},
		{"midnight truecolor", theme.Midnight(), TrueColor, "┏", "┌"},
		{"high-contrast ansi256", theme.HighContrast(), ANSI256, "┏", "┌"},
		{"monochrome plain", theme.Monochrome(), Plain, "+", "+"},
		{"monochrome ansi256", theme.Monochrome(), ANSI256, "+", "+"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := New(tc.th, tc.mode)
			focused := k.WindowStyle(true).Render("hi")
			unfocused := k.WindowStyle(false).Render("hi")
			muted := k.WindowMuted("Alpha")
			if tc.mode == Plain {
				for _, s := range []string{focused, unfocused, muted} {
					if strings.Contains(s, "\x1b") {
						t.Fatalf("Plain emitted escapes: %q", s)
					}
				}
			}
			if got := ansi.Strip(focused); !strings.HasPrefix(got, tc.focusedTL) {
				t.Fatalf("focused window top-left = %q, want prefix %q", got, tc.focusedTL)
			}
			if got := ansi.Strip(unfocused); !strings.HasPrefix(got, tc.unfocusTL) {
				t.Fatalf("unfocused window top-left = %q, want prefix %q", got, tc.unfocusTL)
			}
			if tc.mode != Plain && focused == unfocused {
				t.Fatal("focused and unfocused styles are identical in color mode")
			}
		})
	}
}

func snapshot(t *testing.T, name, view string) {
	t.Helper()
	lines := strings.Split(view, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " ")
	}
	got := strings.Join(lines, "\n") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("snapshot %s changed; inspect the render before running go test ./internal/experience/ui -update\n%s", path, got)
	}
}
