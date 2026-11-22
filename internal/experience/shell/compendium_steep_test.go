package shell

import (
	"context"
	"testing"
	"time"

	"getsteep.dev/steep"
	"gostalgia/internal/experience/notifications"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
	"gostalgia/internal/runtime"
)

func TestSteepCompendiumNewNoteFlow(t *testing.T) {
	for _, size := range [][2]int{{100, 32}, {80, 24}} {
		name := "100x32"
		if size[0] == 80 {
			name = "80x24"
		}
		t.Run(name, func(t *testing.T) {
			rt, client, err := bootShellTestRuntime(t.TempDir(), runtime.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { rt.Shutdown("steep compendium cleanup") })
			t.Cleanup(func() { _ = client.Close() })

			m := NewWithThemeAndNotifs(context.Background(), client, rt.Done(), theme.Nostalgia(), ui.ANSI256, notifications.NewManager(100, 3, 8))
			m.motion.startupPhase = startupConsumed
			m.setMode(modePrompt)
			h := steep.NewHarness(t, m,
				steep.WithWindowSize(size[0], size[1]),
				steep.WithTimeout(15*time.Second),
				steep.WithColor(steep.ColorANSI256),
			)
			h.Wait("Services: 15")

			h.Type("launch com.gostalgia.compendium").Key("enter").Wait("Launched com.gostalgia.compendium")
			h.Key("ctrl+p").Wait("COMMAND PALETTE")
			h.Type("switch to compendium").Key("enter").Wait([]string{
				"Compendium",
				"Search or command:",
				"[Go]",
				"[New note]",
				"[Today]",
				"More",
				"[Graph]",
				"[Help]",
			})

			// Exact #297 flow: enter `new Visual Test` and activate Go.
			h.Type("new Visual Test").Key("enter").Wait([]string{
				"Compendium — Visual Test",
				"Markdown:",
				"[Save]",
				"[Back]",
				`Created "Visual Test"`,
			})
			h.AssertNot("(empty note)")

			// Without pressing Tab, the Markdown editor is focused and accepts text immediately.
			h.Type("# Visual Note").Key("enter").Type("Body line")
			h.Wait([]string{"# Visual Note", "Body line"})

			// Tab past Rename to -> Save, then commit.
			h.Key("tab").Key("tab").Key("enter").Wait([]string{
				"Saved Visual Test",
				"Rendered",
				"Visual Note",
				"Markdown:",
				"[Save]",
			})

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var opened struct {
				Title   string `json:"title"`
				Content string `json:"content"`
			}
			if err := client.Call(ctx, "app/com.gostalgia.compendium/open", map[string]string{"title": "Visual Test"}, &opened); err != nil {
				t.Fatalf("open Visual Test: %v", err)
			}
			if opened.Title != "Visual Test" || opened.Content != "# Visual Note\nBody line" {
				t.Fatalf("persisted note = %+v", opened)
			}

			// Return to Home and prove secondary actions in the More grid remain keyboard-accessible.
			// Focus is on Save; Tab reaches Back.
			h.Key("tab").Key("enter").Wait([]string{
				"Compendium",
				"Visual Test",
				"More",
				"[Graph]",
			})
			// Reopening a non-empty note with a Rendered region also keeps the editor and Save visible.
			// Focus is on Search or command; 2 Tabs reach Open (Go, Open).
			h.Key("tab").Key("tab").Key("enter").Wait([]string{
				"Compendium — Visual Test",
				"Markdown:",
				"# Visual Note",
				"[Save]",
				"Rendered",
			})
			// Return to Home (Tab: Rename to -> Save -> Back).
			h.Key("tab").Key("tab").Key("tab").Key("enter").Wait([]string{
				"Compendium",
				"Visual Test",
				"More",
				"[Graph]",
			})
			// Focus is on Search or command; 5 Tabs reach More grid (Go, Open, New note, Today, More).
			for range 5 {
				h.Key("tab")
			}
			// Cell 0 is Graph; activate it via Enter.
			h.Key("enter").Wait([]string{
				"Compendium — Graph",
				"Graph overview",
			})
		})
	}
}
