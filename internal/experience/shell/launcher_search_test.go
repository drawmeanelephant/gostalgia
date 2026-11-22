package shell

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gostalgia/internal/experience/theme"
	"gostalgia/internal/experience/ui"
)

func searchApp(id, name, desc string) appStatus {
	var a appStatus
	a.Manifest.ID = id
	a.Manifest.Name = name
	a.Manifest.Version = "0.1.0"
	a.Manifest.Description = desc
	a.Manifest.Permissions = []string{"ipc"}
	return a
}

func appIDs(apps []appStatus) []string {
	ids := make([]string, len(apps))
	for i, a := range apps {
		ids[i] = a.Manifest.ID
	}
	return ids
}

func TestLauncherSearchRanksExactNameAheadOfDescription(t *testing.T) {
	rec := &recordingCaller{}
	m := sized(NewWithTheme(context.Background(), rec, nil, theme.Nostalgia(), ui.Plain), 100, 32)
	m.apps = []appStatus{
		searchApp("com.gostalgia.compendium", "Compendium", "Local-first knowledge base: Markdown notes, wikilinks, backlinks, search, tags, and a note graph."),
		searchApp("com.gostalgia.notes", "Notes", "Text editor with safe saving, dirty state, and crash recovery."),
	}
	m.setMode(modeLauncher)

	for _, r := range "notes" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}

	got := appIDs(m.filteredApps())
	want := []string{"com.gostalgia.notes", "com.gostalgia.compendium"}
	if !slices.Equal(got, want) {
		t.Fatalf("filteredApps(%q) = %v, want %v", m.launcherQuery, got, want)
	}
	if m.selected != 0 || selectedAppID(m) != "com.gostalgia.notes" {
		t.Fatalf("default selection = (%d, %q), want (0, com.gostalgia.notes)", m.selected, selectedAppID(m))
	}

	_, cmd := m.Update(enterKey)
	runCmd(t, m, cmd)
	if launched := calls(rec, "app/launch"); len(launched) != 1 || launched[0].params != `{"id":"com.gostalgia.notes"}` {
		t.Fatalf("Enter launched %v, want Notes", launched)
	}

	m.setLauncherQuery("  NoTeS  ")
	if got := appIDs(m.filteredApps()); !slices.Equal(got, want) {
		t.Fatalf("case-insensitive trimmed query = %v, want %v", got, want)
	}

	m.setLauncherQuery("wikilinks")
	if got := appIDs(m.filteredApps()); !slices.Equal(got, []string{"com.gostalgia.compendium"}) {
		t.Fatalf("description-only query = %v, want [com.gostalgia.compendium]", got)
	}
}

func TestLauncherSearchRankingTiers(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	m.apps = []appStatus{
		searchApp("com.gostalgia.compendium", "Compendium", "Markdown notes and wikilinks."),
		searchApp("org.notes.writer", "Writer", "Plain text workspace."),
		searchApp("com.gostalgia.notesapp", "Drafts", "Quick jotter."),
		searchApp("org.example.notes", "Scratchpad", "Temporary buffer."),
		searchApp("com.gostalgia.quicknotes", "Quick Notes", "Fast capture."),
		searchApp("com.gostalgia.notesplus", "Notes Plus", "Extended editor."),
		searchApp("com.gostalgia.notes", "Notes", "Text editor."),
	}

	m.launcherQuery = "notes"
	got := appIDs(m.filteredApps())
	want := []string{
		"com.gostalgia.notes",      // exact name
		"com.gostalgia.notesplus",  // name prefix
		"com.gostalgia.quicknotes", // name substring
		"org.example.notes",        // exact short ID segment
		"com.gostalgia.notesapp",   // short ID prefix
		"org.notes.writer",         // ID substring
		"com.gostalgia.compendium", // description only
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ranked order = %v, want %v", got, want)
	}

	m.apps = []appStatus{
		searchApp("com.gostalgia.calculator", "Calculator", "Retired desk calculator: superseded by Dogcalc (com.gostalgia.dogcalc)."),
		searchApp("com.gostalgia.dogcalc.pro", "Dogcalc Pro", "Extended dog calculator."),
		searchApp("com.gostalgia.dogcalc", "Dogcalc", "The calculator, but every button is a dog."),
	}

	m.launcherQuery = "com.gostalgia.dogcalc"
	got = appIDs(m.filteredApps())
	want = []string{
		"com.gostalgia.dogcalc",     // exact full ID
		"com.gostalgia.dogcalc.pro", // full ID prefix
		"com.gostalgia.calculator",  // description only
	}
	if !slices.Equal(got, want) {
		t.Fatalf("full ID query ranked = %v, want %v", got, want)
	}

	m.launcherQuery = "calc"
	got = appIDs(m.filteredApps())
	want = []string{
		"com.gostalgia.calculator",  // name prefix
		"com.gostalgia.dogcalc.pro", // name substring (earlier in m.apps)
		"com.gostalgia.dogcalc",     // name substring (later in m.apps)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("prefix vs substring query ranked = %v, want %v", got, want)
	}
}

func TestLauncherSearchTieBreakingStabilityAndEmptyQuery(t *testing.T) {
	m := New(context.Background(), noopCaller{}, nil)
	original := []appStatus{
		searchApp("com.gostalgia.z-desc", "Alpha", "Contains notes."),
		searchApp("com.gostalgia.a-desc", "Beta", "Also contains notes."),
		searchApp("com.gostalgia.z-prefix", "Notes Zeta", "Editor."),
		searchApp("com.gostalgia.a-prefix", "Notes Alpha", "Editor."),
	}
	m.apps = slices.Clone(original)
	originalIDs := appIDs(original)

	for _, q := range []string{"", "   ", "\t"} {
		m.launcherQuery = q
		if got := appIDs(m.filteredApps()); !slices.Equal(got, originalIDs) {
			t.Fatalf("empty/blank query %q = %v, want original %v", q, got, originalIDs)
		}
	}

	m.launcherQuery = "notes"
	want := []string{
		"com.gostalgia.z-prefix",
		"com.gostalgia.a-prefix",
		"com.gostalgia.z-desc",
		"com.gostalgia.a-desc",
	}
	if got := appIDs(m.filteredApps()); !slices.Equal(got, want) {
		t.Fatalf("stable tie-breaking = %v, want %v", got, want)
	}
	if got := appIDs(m.apps); !slices.Equal(got, originalIDs) {
		t.Fatalf("filteredApps mutated m.apps: %v, want %v", got, originalIDs)
	}
}

func TestLauncherSearchSelectionMouseHoverAndScrollConsistency(t *testing.T) {
	rec := &recordingCaller{}
	m := sized(NewWithTheme(context.Background(), rec, nil, theme.Nostalgia(), ui.Plain), 80, 24)
	m.apps = []appStatus{
		searchApp("com.gostalgia.alpha", "Alpha", "First filler app."),
		searchApp("com.gostalgia.beta", "Beta", "Second filler app."),
		searchApp("com.gostalgia.compendium", "Compendium", "Markdown notes and wikilinks."),
		searchApp("com.gostalgia.notes", "Notes", "Text editor."),
	}
	m.setMode(modeLauncher)
	m.selected = 3
	m.launcherTop = 2
	m.setHover(hoverLauncherCard, "com.gostalgia.compendium")

	m.Update(tea.PasteMsg{Content: "notes"})
	if m.launcherQuery != "notes" || m.selected != 0 || m.launcherTop != 0 {
		t.Fatalf("after paste: query=%q selected=%d top=%d", m.launcherQuery, m.selected, m.launcherTop)
	}
	if m.hover.valid() {
		t.Fatalf("hover survived query change: %+v", m.hover)
	}
	if got := selectedAppID(m); got != "com.gostalgia.notes" {
		t.Fatalf("selected after paste = %q, want Notes", got)
	}

	notesRect := rectOf(t, m, launcherCard("com.gostalgia.notes"))
	compRect := rectOf(t, m, launcherCard("com.gostalgia.compendium"))
	if notesRect.Min.Y >= compRect.Min.Y {
		t.Fatalf("Notes card (%v) should appear above Compendium card (%v)", notesRect, compRect)
	}

	rects := checkFrame(t, m)
	body := rects[slotBody]
	rendered := m.renderLauncher(body.Dx(), body.Dy())
	drawn := cellSlice(checkRendered(t, m.View(), 80, 24), body)
	for i, line := range rendered {
		if i >= len(drawn) || strings.TrimRight(line, " ") != strings.TrimRight(drawn[i], " ") {
			t.Fatalf("row %d: renderer %q != bloom %q", i, line, drawn[i])
		}
	}

	mouse(t, m, tea.MouseMotionMsg{X: compRect.Min.X + 2, Y: compRect.Min.Y + 1})
	if m.hover.kind != hoverLauncherCard || m.hover.id != "com.gostalgia.compendium" {
		t.Fatalf("hover on ranked Compendium card = %+v", m.hover)
	}

	clickAt(t, m, compRect.Min)
	if m.selected != 1 || selectedAppID(m) != "com.gostalgia.compendium" {
		t.Fatalf("click selected (%d, %q), want (1, com.gostalgia.compendium)", m.selected, selectedAppID(m))
	}

	clickAt(t, m, notesRect.Min)
	clickAt(t, m, notesRect.Min)
	if launched := calls(rec, "app/launch"); len(launched) != 1 || launched[0].params != `{"id":"com.gostalgia.notes"}` {
		t.Fatalf("double click on ranked Notes launched %v", launched)
	}

	press(m, key(tea.KeyBackspace))
	if m.launcherQuery != "note" || m.selected != 0 || selectedAppID(m) != "com.gostalgia.notes" {
		t.Fatalf("after backspace: query=%q selected=%d app=%q", m.launcherQuery, m.selected, selectedAppID(m))
	}

	press(m, escKey)
	if m.launcherQuery != "" || m.selected != 0 || selectedAppID(m) != "com.gostalgia.alpha" {
		t.Fatalf("after Esc clear: query=%q selected=%d app=%q", m.launcherQuery, m.selected, selectedAppID(m))
	}
}
