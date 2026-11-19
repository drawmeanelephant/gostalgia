package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/taskmanager"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// Regression for #160: a document name is VFS data and may legally contain
// terminal escape sequences (vfs.Normalize rejects only backslashes, dot
// segments, and NUL). Home and the command palette must route VFS-derived
// strings through ui.Sanitize, or a hostile app writes OSC 52 clipboard or
// OSC 8 link payloads into the operator's terminal.
func TestAuditHomePaletteDocumentNamesNotSanitized(t *testing.T) {
	m := NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), ui.Plain)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	evil := "\x1b]52;c;cHJldmVydA==\x07.txt"
	m.homeData.Documents = []docShortcut{{
		Name: evil,
		Path: "/users/guest/documents/" + evil,
		Size: 42,
	}}
	var app appStatus
	app.Manifest.Name = "na\x1b[2Jme"
	app.Manifest.ID = "com.evil.\x1b]8;;https://example.invalid\x07app"
	app.Manifest.Version = "1.0"
	app.Running, app.PID = true, 7
	m.apps = []appStatus{app}

	m.setMode(modeHome)
	home := m.View().Content
	m.openPalette()
	palette := m.View().Content

	// ui.Plain emits no terminal styling of its own, so any raw ESC or BEL
	// in the rendered view came straight from unsanitized app/VFS data.
	leaked := func(view string) bool {
		return strings.ContainsAny(view, "\x1b\a")
	}
	if leaked(home) || leaked(palette) {
		t.Fatalf("BUG: document/app names render without ui.Sanitize — home view contains raw ESC: %v; palette view contains raw ESC: %v", leaked(home), leaked(palette))
	}
	if !strings.Contains(home, ".txt") {
		t.Fatalf("home view lost the document name entirely:\n%s", home)
	}
	if strings.Contains(home, "cHJldmVydA==") || strings.Contains(palette, "cHJldmVydA==") {
		t.Fatal("OSC 52 payload survived sanitization")
	}
}

// auditDocCaller answers the document-listing IPC routes with a path and
// label carrying terminal escape sequences.
type auditDocCaller struct{ noopCaller }

func (auditDocCaller) Call(_ context.Context, method string, _ any, out any) error {
	evil := "/users/guest/documents/\x1b]52;c;cHJldmVydA==\x07.txt"
	switch method {
	case "doc/search":
		res, _ := out.(*struct {
			Results []struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			} `json:"results"`
			Total int `json:"total"`
		})
		if res != nil {
			res.Results = append(res.Results, struct {
				Path string `json:"path"`
				Size int64  `json:"size"`
			}{Path: evil, Size: 1})
			res.Total = 1
		}
	case "doc/recents":
		res, _ := out.(*struct {
			Entries []struct {
				Path   string `json:"path"`
				Exists bool   `json:"exists"`
			} `json:"entries"`
		})
		if res != nil {
			res.Entries = append(res.Entries, struct {
				Path   string `json:"path"`
				Exists bool   `json:"exists"`
			}{Path: evil, Exists: true})
		}
	case "doc/favorites":
		res, _ := out.(*struct {
			Entries []struct {
				Path  string `json:"path"`
				Label string `json:"label"`
			} `json:"entries"`
		})
		if res != nil {
			res.Entries = append(res.Entries, struct {
				Path  string `json:"path"`
				Label string `json:"label"`
			}{Path: evil, Label: "fav\x1b[2J"})
		}
	}
	return nil
}

func TestAuditDocCommandOutputSanitized(t *testing.T) {
	ctx := context.Background()
	for _, line := range []string{"search anything", "recents", "favorites"} {
		res := execute(ctx, auditDocCaller{}, "/", line)
		if res.err != nil {
			t.Fatalf("%s: %v", line, res.err)
		}
		if strings.ContainsAny(res.text, "\x1b\a") {
			t.Fatalf("%s output contained raw terminal escapes: %q", line, res.text)
		}
		if strings.Contains(res.text, "cHJldmVydA==") {
			t.Fatalf("%s output kept the OSC 52 payload: %q", line, res.text)
		}
	}
}

// injections are terminal control strings, each with its payload marked
// cHduZWQ, with the terminators terminals accept: the 7-bit forms, and
// the 8-bit C1 forms as UTF-8. Sanitize drops a C1 introducer as the
// control character it is, which leaves its payload inert, plain text.
var injections = []struct {
	name, seq string
	inert     bool
}{
	{"OSC 8 BEL", "\x1b]8;;https://evil.invalid/cHduZWQ\aclick\x1b]8;;\a", false},
	{"OSC 8 ST", "\x1b]8;;https://evil.invalid/cHduZWQ\x1b\\click\x1b]8;;\x1b\\", false},
	{"OSC 52 BEL", "\x1b]52;c;cHduZWQ=\a", false},
	{"OSC 52 ST", "\x1b]52;c;cHduZWQ=\x1b\\", false},
	{"DCS", "\x1bP1$qcHduZWQ\x1b\\", false},
	{"DCS tmux passthrough", "\x1bPtmux;\x1b\x1b]52;c;cHduZWQ=\a\x1b\\", false},
	{"APC ST", "\x1b_Ga=T,f=100;cHduZWQ=\x1b\\", false},
	{"C1 OSC 8", "\u009d8;;https://evil.invalid/cHduZWQ\u009cclick\u009d8;;\u009c", true},
	{"C1 OSC 52 BEL", "\u009d52;c;cHduZWQ=\a", true},
	{"C1 DCS", "\u00901$qcHduZWQ\u009c", true},
	{"C1 APC", "\u009fGa=T,f=100;cHduZWQ=\u009c", true},
}

// checkInjected fails if view, drawn from text holding an injection
// between "safe before" and "safe after", lets any control string
// through, or its payload unless the injection leaves that inert, or
// loses the safe text around it. Plain views carry no escape at all;
// colored ones keep only the kit's own styling.
func checkInjected(t *testing.T, where string, view string, plain, inert bool) {
	t.Helper()
	bad := []string{"\x1b]", "\x1bP", "\x1b_", "\x1b^", "\x1bX", "\a", "\u0090", "\u009b", "\u009c", "\u009d", "\u009f"}
	if !inert {
		bad = append(bad, "cHduZWQ", "evil.invalid")
	}
	for _, bad := range bad {
		if strings.Contains(view, bad) {
			t.Fatalf("%s: rendered %q", where, bad)
		}
	}
	if plain && strings.Contains(view, "\x1b") {
		t.Fatalf("%s: Plain view holds an escape", where)
	}
	if text := ansi.Strip(view); !strings.Contains(text, "safe before") || !strings.Contains(text, "safe after") {
		t.Fatalf("%s: the safe text around the injection is gone:\n%s", where, text)
	}
}

// Transcript output, error results and their restyled history never draw
// OSC 8, OSC 52, DCS or APC strings, whatever the theme and profile.
func TestAuditTranscriptBufferSanitized(t *testing.T) {
	for _, inj := range injections {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256} {
			t.Run(fmt.Sprintf("%s/%v", inj.name, mode), func(t *testing.T) {
				m := sized(NewWithTheme(context.Background(), noopCaller{}, nil, theme.Nostalgia(), mode), 80, 24)
				m.setMode(modePrompt)
				text := "safe before " + inj.seq + " safe after"
				m.Update(resultMsg{text: text})
				checkInjected(t, "output", m.View().Content, mode == ui.Plain, inj.inert)
				m.Update(resultMsg{err: errors.New(text)})
				checkInjected(t, "error", m.View().Content, mode == ui.Plain, inj.inert)
				for i, e := range m.transcript {
					if strings.ContainsAny(e.text, "\x1b\a") || strings.ContainsAny(m.transcriptBuf.Line(i), "\a") {
						t.Fatalf("line %d was kept unsanitized: %q", i, e.text)
					}
				}
				for _, th := range []theme.Theme{theme.Midnight(), theme.Monochrome(), theme.HighContrast()} {
					m.SetTheme(th, mode)
					checkInjected(t, "restyled "+th.Name, m.View().Content, mode == ui.Plain, inj.inert)
				}
			})
		}
	}
}

// Log tails, as first opened, refreshed, replaced and searched, never
// draw OSC 8, OSC 52, DCS or APC strings either.
func TestAuditLogBuffersSanitized(t *testing.T) {
	for _, inj := range injections {
		for _, mode := range []ui.ColorMode{ui.Plain, ui.ANSI256} {
			t.Run(fmt.Sprintf("%s/%v", inj.name, mode), func(t *testing.T) {
				line := "safe before " + inj.seq + " safe after\n"
				client := &experienceCaller{
					procs: []taskmanager.ProcessRow{{ID: 2, Name: "evil\x1b]52;c;cHduZWQ=\a", State: "running"}},
					logs:  map[int32]string{2: "boot\n" + line},
				}
				m := sized(NewWithTheme(context.Background(), client, nil, theme.Nostalgia(), mode), 80, 24)
				press(m, tea.KeyPressMsg{Code: tea.KeyF5})
				m.Update(procUpdateMsg{procs: client.procs})
				m.Update(requestLogs(t, m, 2)())
				checkInjected(t, "opened", m.View().Content, mode == ui.Plain, inj.inert)

				press(m, typed("f"))
				client.logs[2] = "boot\n" + line + "more " + inj.seq + "\n" + line
				refresh, ok := tickLogs(m)
				if !ok {
					t.Fatal("no refresh")
				}
				m.Update(refresh)
				checkInjected(t, "refreshed", m.View().Content, mode == ui.Plain, inj.inert)

				client.logs[2] = line + "rotated " + inj.seq
				refresh, _ = tickLogs(m)
				m.Update(refresh)
				checkInjected(t, "replaced", m.View().Content, mode == ui.Plain, inj.inert)

				press(m, typed("/"))
				m.Update(tea.PasteMsg{Content: "safe" + inj.seq})
				press(m, keyEnter, typed("n"))
				view := m.View().Content
				checkInjected(t, "searched", view, mode == ui.Plain, inj.inert)
				if !strings.Contains(ansi.Strip(view), "for \"safe") {
					t.Fatalf("the search did not run:\n%s", ansi.Strip(view))
				}
				m.SetTheme(theme.Midnight(), mode)
				checkInjected(t, "restyled", m.View().Content, mode == ui.Plain, inj.inert)
			})
		}
	}
}
