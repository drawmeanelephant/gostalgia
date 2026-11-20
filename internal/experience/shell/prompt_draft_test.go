package shell

import (
	"context"
	"fmt"
	"image"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// promptRender solves the prompt row and renders the frame holding it.
func promptRender(t *testing.T, m *Model) (tea.View, image.Rectangle) {
	t.Helper()
	r, ok := m.solve().Rect(slotPrompt)
	if !ok {
		t.Fatal("prompt row not solved")
	}
	return m.View(), r
}

// promptContent is the prompt slot's rendered text without styling.
func promptContent(t *testing.T, v tea.View, r image.Rectangle) string {
	t.Helper()
	lines := screenText(v)
	if r.Min.Y < 0 || r.Min.Y >= len(lines) {
		t.Fatalf("prompt row %d outside %d rendered rows", r.Min.Y, len(lines))
	}
	return ansi.Cut(lines[r.Min.Y], r.Min.X, r.Max.X)
}

// checkPromptGraphemes rejects a prompt row that shows half of a test
// cluster: a combining mark without its base or a ZWJ emoji without its
// joined rune.
func checkPromptGraphemes(t *testing.T, content string) {
	t.Helper()
	if !utf8.ValidString(content) {
		t.Fatalf("prompt row has invalid UTF-8: %q", content)
	}
	for i := 0; i < len(content); {
		switch {
		case strings.HasPrefix(content[i:], "\u0301"):
			if i == 0 || content[i-1] != 'e' {
				t.Fatalf("prompt row has an orphan combining mark: %q", content)
			}
			i += len("\u0301")
		case strings.HasPrefix(content[i:], "👩"):
			if !strings.HasPrefix(content[i:], "👩\u200d💻") {
				t.Fatalf("prompt row splits a ZWJ cluster: %q", content)
			}
			i += len("👩\u200d💻")
		default:
			_, size := utf8.DecodeRuneInString(content[i:])
			i += size
		}
	}
}

// TestPromptFittingDraftRemainsFullyVisible pins issue #230: a draft that
// fits with its cursor is never scrolled, even when it exceeds half of the
// available input cells, and the DOS-path prompt still shortens.
func TestPromptFittingDraftRemainsFullyVisible(t *testing.T) {
	t.Run("dos-path repro", func(t *testing.T) {
		const prefix = `C:\users\guest\documents> `
		m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), 80, 24)
		m.setMode(modePrompt)
		m.cwd = "/users/guest/documents"
		draft := "echo 123456789012345678901234567890"
		m.Update(tea.PasteMsg{Content: draft})

		v, r := promptRender(t, m)
		content := promptContent(t, v, r)
		if !strings.Contains(content, prefix+draft) {
			t.Fatalf("fitting draft is clipped:\n got %q\nwant %q complete", content, draft)
		}
		if v.Cursor == nil {
			t.Fatal("editable prompt has no cursor")
		}
		wantX := r.Min.X + lipgloss.Width(prefix+draft)
		if pos := v.Cursor.Position; pos.Y != r.Min.Y || pos.X != wantX {
			t.Fatalf("cursor at %v, want end of the draft at (%d,%d)", pos, wantX, r.Min.Y)
		}
		if string(m.input) != draft || m.cursor != len(draft) {
			t.Fatalf("render changed the draft: input %q cursor %d", string(m.input), m.cursor)
		}
	})

	t.Run("just over half the row", func(t *testing.T) {
		for _, size := range [][2]int{{30, 10}, {80, 24}, {200, 60}} {
			t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
				m.setMode(modePrompt)
				prompt, _ := m.solve().Rect(slotPrompt)
				_, prefixX, editable := m.promptLine(prompt.Dx())
				if !editable {
					t.Fatal("empty prompt row is not editable")
				}
				avail := prompt.Dx() - prefixX
				n := avail/2 + 1
				if n >= avail {
					t.Fatalf("no draft can exceed half and still fit in %d cells", avail)
				}
				draft := strings.Repeat("x", n)
				m.Update(tea.PasteMsg{Content: draft})

				v, r := promptRender(t, m)
				if !strings.Contains(promptContent(t, v, r), draft) {
					t.Fatalf("draft of %d cells (>%d half) is clipped from a %d-cell row", n, avail/2, r.Dx())
				}
				if v.Cursor == nil {
					t.Fatal("editable prompt has no cursor")
				}
				if wantX := r.Min.X + prefixX + n; v.Cursor.Position.X != wantX {
					t.Fatalf("cursor X = %d, want %d after the whole draft", v.Cursor.Position.X, wantX)
				}
			})
		}
	})

	t.Run("long path still shortens", func(t *testing.T) {
		for _, size := range [][2]int{{30, 10}, {80, 24}} {
			t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
				m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
				m.setMode(modePrompt)
				m.cwd = "/users/guest/" + strings.Repeat("long-directory/", 20)
				prompt, _ := m.solve().Rect(slotPrompt)
				_, prefixX, editable := m.promptLine(prompt.Dx())
				if !editable {
					t.Fatal("empty prompt row is not editable")
				}
				avail := prompt.Dx() - prefixX
				draft := "echo " + strings.Repeat("y", max(1, avail/2))
				if lipgloss.Width(draft) >= avail {
					t.Fatalf("draft %d does not fit in %d cells", lipgloss.Width(draft), avail)
				}
				m.Update(tea.PasteMsg{Content: draft})

				v, r := promptRender(t, m)
				content := promptContent(t, v, r)
				if !strings.Contains(content, "…") {
					t.Fatalf("long DOS path is not shortened: %q", content)
				}
				if !strings.Contains(content, draft) {
					t.Fatalf("fitting draft is clipped under a shortened path: %q", content)
				}
			})
		}
	})
}

// TestPromptOverflowViewportUsesFullRowAndKeepsCursor covers editing a draft
// longer than the row at its beginning, middle and end, with wide, combining
// and ZWJ clusters, across a resize. The overflow viewport keeps the
// insertion point on screen using the full row budget and never splits a
// grapheme; shrinking the draft shows it whole again.
func TestPromptOverflowViewportUsesFullRowAndKeepsCursor(t *testing.T) {
	clusters := make([]string, 0, 400)
	for i := range 80 {
		clusters = append(clusters, "a", "界", "👩\u200d💻", "e\u0301", string(rune('0'+i%10)))
	}
	text := strings.Join(clusters, "")

	for _, size := range [][2]int{{80, 24}, {30, 10}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.TrueColor), size[0], size[1])
			m.setMode(modePrompt)
			m.Update(tea.PasteMsg{Content: text})
			if string(m.input) != text {
				t.Fatal("paste lost text")
			}

			// The cursor viewport covers start, wide/combining/ZWJ
			// clusters, the middle and the end.
			for _, k := range []int{0, 1, 2, 3, len(clusters) / 2, len(clusters)} {
				m.cursor = runesBefore(clusters, k)
				checkCursor(t, m, clusters, k)
				v, r := promptRender(t, m)
				checkPromptGraphemes(t, promptContent(t, v, r))
				_, x, editable := m.promptLine(r.Dx())
				if !editable || x < 0 || x >= r.Dx() {
					t.Fatalf("cluster %d: cursor cell %d outside prompt row %d", k, x, r.Dx())
				}
				if v.Cursor == nil || v.Cursor.Position.X != r.Min.X+x {
					t.Fatalf("cluster %d: rendered cursor %v disagrees with prompt cell %d", k, v.Cursor, x)
				}
				// At the end the viewport spends the whole row budget
				// (minus at most a wide cluster dropped at its left edge)
				// instead of stopping at half.
				if k == len(clusters) && x < r.Dx()-2 {
					t.Fatalf("end cursor at cell %d, want the full row budget near %d", x, r.Dx()-1)
				}
			}

			// Insert and delete at each end and the middle without losing
			// the logical text outside the viewport.
			for _, k := range []int{0, len(clusters) / 2, len(clusters)} {
				m.cursor = runesBefore(clusters, k)
				m.Update(tea.KeyPressMsg{Code: 'Z', Text: "Z"})
				edited := append(append(append([]string{}, clusters[:k]...), "Z"), clusters[k:]...)
				if string(m.input) != strings.Join(edited, "") {
					t.Fatalf("insert at cluster %d = %q", k, string(m.input))
				}
				checkCursor(t, m, edited, k+1)
				m.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
				if string(m.input) != text {
					t.Fatalf("delete at cluster %d = %q", k, string(m.input))
				}
				checkCursor(t, m, clusters, k)
			}

			// Resizing between the two sizes keeps the cursor visible and
			// the whole logical draft intact.
			m.cursor = runesBefore(clusters, len(clusters)/2)
			other := [2]int{30, 10}
			if size[0] == 30 {
				other = [2]int{80, 24}
			}
			sized(m, other[0], other[1])
			checkCursor(t, m, clusters, len(clusters)/2)
			sized(m, size[0], size[1])
			checkCursor(t, m, clusters, len(clusters)/2)
			if string(m.input) != text {
				t.Fatal("resize lost text outside the viewport")
			}

			// A draft that fits again reappears in full.
			short := "echo short"
			m.input, m.cursor = []rune(short), len(short)
			v, r := promptRender(t, m)
			if content := promptContent(t, v, r); !strings.Contains(content, short) {
				t.Fatalf("fitting draft not shown whole after overflow: %q", content)
			}
			_, x, editable := m.promptLine(r.Dx())
			if !editable || v.Cursor == nil || v.Cursor.Position.X != r.Min.X+x {
				t.Fatalf("short draft cursor = %v, want cell %d", v.Cursor, r.Min.X+x)
			}
		})
	}
}

// TestPromptSubmitsCompleteOverflowingDraft proves the viewport never
// truncates what Enter submits, even when most of the draft is off screen.
func TestPromptSubmitsCompleteOverflowingDraft(t *testing.T) {
	var submitted string
	m := fixtureModel(theme.Nostalgia(), ui.TrueColor)
	m.client = callerFunc(func(_ context.Context, method string, params, _ any) error {
		if method == "app/com.gostalgia.echo/echo" {
			submitted = params.(map[string]string)["msg"]
		}
		return nil
	})
	m.cwd = "/users/guest/documents"
	m.setMode(modePrompt)
	draft := "echo " + strings.Repeat("1234567890", 30)
	m.Update(tea.PasteMsg{Content: draft})
	if string(m.input) != draft {
		t.Fatal("paste lost text")
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter returned no command")
	}
	cmd()
	if want := strings.TrimPrefix(draft, "echo "); submitted != want {
		t.Fatalf("echo received %q, want the complete %q", submitted, want)
	}
	line := dosPath("/users/guest/documents") + "> " + draft
	found := false
	for _, e := range m.transcript {
		if e.kind == "command" && e.text == line {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("transcript does not record the complete command %q", line)
	}
	if m.input != nil || m.cursor != 0 {
		t.Fatal("submission left draft state behind")
	}
}
