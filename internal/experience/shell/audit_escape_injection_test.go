package shell

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	home := m.View()
	m.openPalette()
	palette := m.View()

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
