package shell

import (
	"encoding/json"
	"strings"
	"testing"

	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

// At 80x24 a full card shows the manifest's optional icon art beside the
// name, status, and identity, and a manifest without art shows the generic
// ASCII glyph rather than a reserved blank column.
func TestLauncherShowsIconBesideNameAndState(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.setMode(modeLauncher)
	m.selected = 0
	m.apps[0].Manifest.Icon = ">_\n>_"
	m.View()

	body := bodyText(t, m)
	for _, want := range []string{"Echo", "LIVE", ">_"} {
		if !strings.Contains(body, want) {
			t.Fatalf("shelf lacks %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, ">_"); got != 2 {
		t.Fatalf("shelf drew %d icon lines, want 2:\n%s", got, body)
	}

	m.apps[0].Manifest.Icon = ""
	m.View()
	body = bodyText(t, m)
	for _, want := range []string{"+-+", "| |", "Echo", "LIVE"} {
		if !strings.Contains(body, want) {
			t.Fatalf("fallback shelf lacks %q:\n%s", want, body)
		}
	}
}

// Hostile external art cannot inject terminal operations through a card.
func TestLauncherSanitizesHostileIconArt(t *testing.T) {
	m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), 80, 24)
	m.setMode(modeLauncher)
	m.selected = 0
	m.apps[0].Manifest.Icon = "\x1b[2J\x1b]52;c;clipboard\a\n\x00\x1b[31mred\x1b[0m"
	m.View()

	body := bodyText(t, m)
	if strings.ContainsAny(body, "\x1b\a\r\x00") {
		t.Fatalf("hostile icon escaped the sanitizer: %q", body)
	}
	// The remaining printable row fits and is clipped by the twelve-cell
	// budget rather than widening the card.
	if !strings.Contains(body, "red") || !strings.Contains(body, "Echo") || !strings.Contains(body, "LIVE") {
		t.Fatalf("sanitized icon lost identity or art:\n%s", body)
	}
}

// At 30x10 the two-row compact card keeps the selected app's name and state;
// icon art, including a wide supplied icon or the fallback, never takes their
// cells.
func TestMinimumLauncherKeepsNameAndStateOverIconArt(t *testing.T) {
	for _, art := range []string{"WWWWWWWWWWWW\nWWWWWWWWWWWW", ""} {
		t.Run("icon="+strings.SplitN(art, "\n", 2)[0], func(t *testing.T) {
			m := sized(fixtureModel(theme.Nostalgia(), ui.Plain), minFrameWidth, minFrameHeight)
			m.setMode(modeLauncher)
			m.selected = 0
			m.apps[0].Manifest.Name = "Echo With An Extraordinarily Long Name"
			m.apps[0].Manifest.Icon = art
			m.View()

			body := bodyText(t, m)
			for _, want := range []string{"Echo With", "LIVE"} {
				if !strings.Contains(body, want) {
					t.Fatalf("30x10 shelf lost %q:\n%s", want, body)
				}
			}
			if strings.Contains(body, "WWWWWWWWWWWW") || strings.Contains(body, "+-+") {
				t.Fatalf("30x10 shelf spent identity rows on icon art:\n%s", body)
			}
		})
	}
}

// app/list transport carries the optional manifest icon into the shelf's
// decoded status, and a legacy entry without the field still decodes.
func TestAppStatusKeepsOptionalIconFromList(t *testing.T) {
	var apps []appStatus
	data := `[{"manifest":{"id":"com.gostalgia.echo","name":"Echo","version":"0.1.0","description":"","icon":">_\n>_","permissions":["ipc"]},"running":true,"pid":7},` +
		`{"manifest":{"id":"com.gostalgia.clock","name":"Clock","version":"0.1.0","description":""},"running":false}]`
	if err := json.Unmarshal([]byte(data), &apps); err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Manifest.Icon != ">_\n>_" || apps[1].Manifest.Icon != "" {
		t.Fatalf("decoded app/list = %+v", apps)
	}
}
