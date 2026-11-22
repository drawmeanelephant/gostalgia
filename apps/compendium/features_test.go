package compendium

import (
	"context"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gostalgia/sdk"
)

// syntheticVault writes n deterministic notes straight into the vault
// directory (as an import or sync tool would) and opens Compendium on it,
// so the first open performs a full index build from sources.
func syntheticVault(t testing.TB, n int) *harness {
	t.Helper()
	fs := newMockFS()
	rng := rand.New(rand.NewPCG(42, 2026))
	syll := []string{"ka", "lo", "mi", "ren", "ta", "vo", "shi", "na", "pel", "dur", "an", "ex", "or", "qui", "zan", "bel"}
	vocab := make([]string, 0, 3000)
	for len(vocab) < 3000 {
		w := syll[rng.IntN(len(syll))] + syll[rng.IntN(len(syll))] + syll[rng.IntN(len(syll))]
		vocab = append(vocab, w)
	}
	topical := []string{"lantern", "quiet", "harbor", "atlas", "river", "stone", "orchard", "compass", "ridge", "meadow", "solstice", "solar"}
	tags := []string{"#garden", "#travel", "#reading", "#work", "#idea", "#orchard"}
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, "# Note %04d\n\n", i)
		for line := 0; line < 15; line++ {
			for w := 0; w < 12; w++ {
				if rng.IntN(25) == 0 {
					b.WriteString(topical[rng.IntN(len(topical))])
				} else {
					b.WriteString(vocab[rng.IntN(len(vocab))])
				}
				b.WriteByte(' ')
			}
			b.WriteString("\n")
		}
		for l := 0; l < 3; l++ {
			fmt.Fprintf(&b, "See [[Note %04d]]. ", rng.IntN(n))
		}
		fmt.Fprintf(&b, "\n%s %s\n", tags[rng.IntN(len(tags))], tags[rng.IntN(len(tags))])
		fs.put(fmt.Sprintf("%s/Note %04d.md", testVault, i), []byte(b.String()))
	}
	return openHarness(t, fs)
}

// BenchmarkSearch2000 measures ranked search with snippets over 2,000 notes
// through the public route (JSON decode, ranking, snippet extraction).
func BenchmarkSearch2000(b *testing.B) {
	h := syntheticVault(b, 2000)
	h.must(b, "status", nil, nil)
	queries := []string{"lantern", "quiet harbor", "compass ridge meadow", "sol", "note 1500"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := h.call("search", map[string]any{"query": queries[i%len(queries)], "limit": 20}, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkIndexBuild2000 measures a full rebuild from 2,000 sources.
func BenchmarkIndexBuild2000(b *testing.B) {
	h := syntheticVault(b, 2000)
	h.must(b, "status", nil, nil)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.must(b, "rebuild", nil, nil)
	}
}

func TestTodayOpensOrCreatesDailyNote(t *testing.T) {
	h := openHarness(t, newMockFS())
	var out struct {
		Title   string `json:"title"`
		Created bool   `json:"created"`
	}
	h.must(t, "today", nil, &out)
	if out.Title != "2026-10-06" || !out.Created {
		t.Fatalf("today = %+v", out)
	}
	h.must(t, "save", map[string]string{"title": "2026-10-06", "content": "# 2026-10-06\n\nWrote [[Compendium]] code.\n"}, nil)
	h.must(t, "today", nil, &out)
	if out.Created {
		t.Fatal("second today created a new note")
	}
	var n noteOut
	h.must(t, "open", map[string]string{"title": "2026-10-06"}, &n)
	if !strings.Contains(n.Content, "Wrote [[Compendium]]") {
		t.Fatalf("today clobbered the daily note: %q", n.Content)
	}
	// The `today` command in the presentation opens the same note.
	v := h.act(t, "run", "", map[string]string{queryField(t, h.view(t)): "today"})
	if !strings.Contains(v.Title, "2026-10-06") {
		t.Fatalf("today command view title = %q", v.Title)
	}
}

func TestTagsIndexedAndBrowsable(t *testing.T) {
	h := openHarness(t, newMockFS())
	h.must(t, "create", map[string]string{"title": "One", "content": "# Heading is not a tag\n#garden and #Garden/herbs, issue #42 is not a tag.\n`#code` neither.\n```\n#fenced\n```\nmid#word no\n"}, nil)
	h.must(t, "create", map[string]string{"title": "Two", "content": "(#garden) #ideas-2026\n"}, nil)
	var tags struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		} `json:"tags"`
	}
	h.must(t, "tags", nil, &tags)
	got := map[string]int{}
	for _, tg := range tags.Tags {
		got[tg.Tag] = tg.Count
	}
	want := map[string]int{"garden": 2, "garden/herbs": 1, "ideas-2026": 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tags = %v, want %v", got, want)
	}
	var tagged struct {
		Notes []string `json:"notes"`
	}
	h.must(t, "tag", map[string]string{"tag": "#Garden"}, &tagged)
	if !reflect.DeepEqual(tagged.Notes, []string{"One", "Two"}) {
		t.Fatalf("notes tagged garden = %v", tagged.Notes)
	}
	// Browsable in the presentation: tags view, then a tag's notes.
	v := h.act(t, "tags", "", nil)
	var gardenItem string
	for _, it := range v.Items {
		if strings.HasPrefix(it.Label, "#garden ") || it.Label == "#garden" {
			gardenItem = it.ID
		}
	}
	if gardenItem == "" {
		t.Fatalf("tags view lacks #garden: %+v", v.Items)
	}
	v = h.act(t, "open", gardenItem, nil)
	if len(v.Items) != 2 {
		t.Fatalf("tag view items = %+v", v.Items)
	}
}

func TestGraphPublishesNodesEdgesDeadEndsOrphans(t *testing.T) {
	h := openHarness(t, newMockFS())
	h.must(t, "create", map[string]string{"title": "A", "content": "[[B]] [[B]]\n"}, nil)
	h.must(t, "create", map[string]string{"title": "B", "content": "[[C]] #x\n"}, nil)
	h.must(t, "create", map[string]string{"title": "C", "content": "end\n"}, nil)
	h.must(t, "create", map[string]string{"title": "D", "content": "alone\n"}, nil)
	h.must(t, "create", map[string]string{"title": "E", "content": "[[Missing]]\n"}, nil)
	var g graphOut
	h.must(t, "graph", nil, &g)
	if len(g.Nodes) != 5 || len(g.Edges) != 2 {
		t.Fatalf("graph nodes=%d edges=%d, want 5/2: %+v", len(g.Nodes), len(g.Edges), g)
	}
	if !reflect.DeepEqual(g.DeadEnds, []string{"C"}) {
		t.Fatalf("dead ends = %v, want [C]", g.DeadEnds)
	}
	if !reflect.DeepEqual(g.Orphans, []string{"D", "E"}) {
		t.Fatalf("orphans = %v, want [D E]", g.Orphans)
	}
	if len(g.Unresolved) != 1 || g.Unresolved[0].Target != "Missing" || !reflect.DeepEqual(g.Unresolved[0].From, []string{"E"}) {
		t.Fatalf("unresolved = %+v", g.Unresolved)
	}

	// The same graph as presentation data: plain items the shell renders.
	v := h.act(t, "graph", "", nil)
	if !strings.Contains(v.Title, "Graph") {
		t.Fatalf("graph view title %q", v.Title)
	}
	var summary, deadEnd, orphan, missing bool
	for _, it := range v.Items {
		text := it.Label + " " + it.Detail
		summary = summary || (it.ID == "summary" && strings.Contains(text, "5 notes") && strings.Contains(text, "2 links"))
		deadEnd = deadEnd || (strings.Contains(it.Label, "C") && strings.Contains(text, "dead end"))
		orphan = orphan || (strings.Contains(it.Label, "D") && strings.Contains(text, "orphan"))
		missing = missing || (strings.Contains(it.Label, "Missing") && strings.Contains(text, "missing"))
	}
	if !summary || !deadEnd || !orphan || !missing {
		t.Fatalf("graph view incomplete (summary=%v deadEnd=%v orphan=%v missing=%v): %+v", summary, deadEnd, orphan, missing, v.Items)
	}
}

func TestRecoveryPromptOnRelaunch(t *testing.T) {
	h := openHarness(t, newMockFS())
	h.must(t, "create", map[string]string{"title": "Draft Me", "content": "saved\n"}, nil)
	h.must(t, "edit", map[string]string{"title": "Draft Me", "content": "saved\nunsaved line\n"}, nil)

	h2 := openHarness(t, h.fs)
	v := h2.view(t)
	if !strings.Contains(v.Title, "Recovery") {
		t.Fatalf("relaunch with a draft should prompt recovery, got %q", v.Title)
	}
	var item string
	for _, it := range v.Items {
		if strings.Contains(it.Label, "Draft Me") {
			item = it.ID
		}
	}
	v = h2.act(t, "restore_draft", item, nil)
	if !strings.Contains(v.Title, "Draft Me") || !strings.HasSuffix(v.Title, "*") {
		t.Fatalf("restored draft view = %q, want dirty note", v.Title)
	}
	v = h2.act(t, "save", "", nil)
	var n noteOut
	h2.must(t, "open", map[string]string{"title": "Draft Me"}, &n)
	if n.Content != "saved\nunsaved line\n" {
		t.Fatalf("content after restore+save = %q", n.Content)
	}
	var d draftsOut
	h2.must(t, "drafts", nil, &d)
	if len(d.Drafts) != 0 {
		t.Fatalf("draft not cleared after save: %+v", d.Drafts)
	}
}

func TestPresentationModesValidateAndStayInBounds(t *testing.T) {
	h := syntheticVault(t, 150)
	h.must(t, "create", map[string]string{"title": "Big", "content": strings.Repeat("big note body [[Note 0001]] ", 400)}, nil)
	v := h.view(t)
	if len(v.Items) > 100 {
		t.Fatalf("home items %d exceed bound", len(v.Items))
	}
	q := queryField(t, v)
	for _, cmd := range []string{"lantern", "open Note 0001", "graph", "tags", "trash", "help", "open Big", "today", "new Fresh Idea"} {
		v = h.act(t, "run", "", map[string]string{q: cmd})
		if cmd == "open Big" {
			for _, f := range v.Fields {
				if strings.HasPrefix(f.ID, "body") {
					t.Fatal("note larger than a field must not expose a truncating body field")
				}
			}
		}
		v = h.act(t, "home", "", nil)
		q = queryField(t, v)
	}
	// Large backlink fan-in stays bounded.
	v = h.act(t, "run", "", map[string]string{q: "open Note 0001"})
	if len(v.Items) > 100 {
		t.Fatalf("note view items %d exceed bound", len(v.Items))
	}
}

func TestPresentationEditSaveRenameTrashFlow(t *testing.T) {
	h := openHarness(t, newMockFS())
	v := h.view(t)
	v = h.act(t, "run", "", map[string]string{queryField(t, v): "new Garden Plan"})
	if !strings.Contains(v.Title, "Garden Plan") {
		t.Fatalf("new command view = %q", v.Title)
	}
	var body, rename string
	for _, f := range v.Fields {
		switch {
		case strings.HasPrefix(f.ID, "body"):
			body = f.ID
		case strings.HasPrefix(f.ID, "rename"):
			rename = f.ID
		}
	}
	if body == "" || rename == "" {
		t.Fatalf("note view fields = %+v", v.Fields)
	}
	v = h.act(t, "save", "", map[string]string{body: "Plant [[Tomatoes]] #garden"})
	var n noteOut
	h.must(t, "open", map[string]string{"title": "Garden Plan"}, &n)
	if n.Content != "Plant [[Tomatoes]] #garden" {
		t.Fatalf("saved via view = %q", n.Content)
	}
	v = h.act(t, "rename", "", map[string]string{rename: "Garden 2027"})
	if !strings.Contains(v.Title, "Garden 2027") {
		t.Fatalf("rename via view title = %q", v.Title)
	}
	v = h.act(t, "trash", "", nil)
	v = h.act(t, "trash_bin", "", nil)
	if len(v.Items) != 1 {
		t.Fatalf("trash bin items = %+v", v.Items)
	}
	v = h.act(t, "restore", v.Items[0].ID, nil)
	h.must(t, "open", map[string]string{"title": "Garden 2027"}, &n)
}

func TestPresentationV3NewNoteFlowAndSecondaryActions(t *testing.T) {
	h := openHarness(t, newMockFS())
	v := h.viewV(t, sdk.PresentationVersion)

	if len(v.Actions) != 4 {
		t.Fatalf("home v3 primary actions = %+v, want 4 compact primary actions", v.Actions)
	}
	if v.Grid == nil || len(v.Grid.Cells) != 8 {
		t.Fatalf("home v3 secondary actions grid = %+v, want 8 cells", v.Grid)
	}
	for _, cell := range []struct {
		id    string
		title string
	}{
		{"graph", "Graph"},
		{"tags", "Tags"},
		{"trash_bin", "Trash"},
		{"help", "Help"},
	} {
		gv := h.actCellV(t, sdk.PresentationVersion, cell.id, cell.id, nil)
		if !strings.Contains(gv.Title, cell.title) {
			t.Fatalf("secondary cell %q view title = %q, want %q", cell.id, gv.Title, cell.title)
		}
		v = h.actV(t, sdk.PresentationVersion, "home", "", nil)
	}

	v = h.actV(t, sdk.PresentationVersion, "run", "", map[string]string{queryField(t, v): "new Visual Test"})
	if v.Title != "Compendium — Visual Test" {
		t.Fatalf("new note title = %q", v.Title)
	}
	if v.Region != nil {
		t.Fatalf("new empty note should omit empty rendered region, got %+v", v.Region)
	}
	if len(v.Fields) < 2 || !strings.HasPrefix(v.Fields[0].ID, "body") || !v.Fields[0].Multiline {
		t.Fatalf("new note fields = %+v, want multiline Markdown editor first", v.Fields)
	}
	if len(v.Actions) != 2 || v.Actions[0].ID != "save" || v.Actions[1].ID != "home" {
		t.Fatalf("new empty note primary actions = %+v, want [save, home]", v.Actions)
	}
	if v.Grid == nil || len(v.Grid.Cells) != 5 {
		t.Fatalf("new note secondary grid = %+v, want 5 cells", v.Grid)
	}
	if !strings.Contains(v.Status, "Created \"Visual Test\"") || !strings.Contains(v.Status, "Save") {
		t.Fatalf("new note status = %q, want clear next-step guidance", v.Status)
	}

	bodyID := v.Fields[0].ID
	renameID := v.Fields[1].ID
	v = h.actV(t, sdk.PresentationVersion, "save", "", map[string]string{bodyID: "# Visual Test\n\nSee [[Next Note]].\n"})
	if v.Region == nil || v.Region.ID != "rendered" || !strings.Contains(v.Region.Text, "Visual Test") {
		t.Fatalf("saved non-empty note missing rendered region: %+v", v.Region)
	}
	if len(v.Actions) != 3 || v.Actions[0].ID != "save" || v.Actions[1].ID != "follow" || v.Actions[2].ID != "home" {
		t.Fatalf("linked note primary actions = %+v, want [save, follow, home]", v.Actions)
	}

	v = h.actCellV(t, sdk.PresentationVersion, "rename", "rename", map[string]string{renameID: "Visual Renamed"})
	if v.Title != "Compendium — Visual Renamed" {
		t.Fatalf("renamed note title = %q", v.Title)
	}

	// Conflict surfaces both overwrite and reload in primary actions without duplicating reload in the grid.
	h.must(t, "save", map[string]string{"title": "Visual Renamed", "content": "external edit\n"}, nil)
	v = h.actV(t, sdk.PresentationVersion, "save", "", map[string]string{v.Fields[0].ID: "local edit\n"})
	if len(v.Actions) < 2 || v.Actions[0].ID != "overwrite" || v.Actions[1].ID != "reload" {
		t.Fatalf("conflict primary actions = %+v, want overwrite and reload first", v.Actions)
	}
	for _, c := range v.Grid.Cells {
		if c.ID == "reload" {
			t.Fatalf("conflict view duplicated reload in secondary grid: %+v", v.Grid.Cells)
		}
	}

	// Version 2 keeps the grid without version-3 multiline/region elements.
	v2 := h.viewV(t, 2)
	if v2.Grid == nil || v2.Region != nil {
		t.Fatalf("v2 note view grid=%v region=%v", v2.Grid != nil, v2.Region != nil)
	}

	// Today action from note grid creates today's daily note with clear guidance.
	v = h.actCellV(t, sdk.PresentationVersion, "today", "today", nil)
	if !strings.Contains(v.Title, "2026-10-06") || !strings.Contains(v.Status, "Save") {
		t.Fatalf("today action from grid = title:%q status:%q", v.Title, v.Status)
	}
}

func TestTitleValidation(t *testing.T) {
	h := openHarness(t, newMockFS())
	for _, bad := range []string{"", "   ", "a[b", "x]]", "a|b", "has#hash", "slash/name", "new\nline", " padded", strings.Repeat("x", 201)} {
		if err := h.call("create", map[string]string{"title": bad}, nil); err == nil {
			t.Errorf("title %q accepted", bad)
		}
	}
	h.must(t, "create", map[string]string{"title": "CON"}, nil) // reserved device name on Windows hosts
	h.must(t, "create", map[string]string{"title": "trailing dot."}, nil)
	h2 := openHarness(t, h.fs)
	h2.must(t, "open", map[string]string{"title": "CON"}, nil)
	h2.must(t, "open", map[string]string{"title": "trailing dot."}, nil)
	if err := h.call("create", map[string]string{"title": "con"}, nil); err == nil {
		t.Error("case-insensitive duplicate title accepted")
	}
}

func TestConcurrentRoutesAreRaceFree(t *testing.T) {
	h := openHarness(t, newMockFS())
	for i := 0; i < 10; i++ {
		h.must(t, "create", map[string]string{"title": fmt.Sprintf("N%d", i), "content": "seed [[N0]]"}, nil)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_ = h.call("search", map[string]any{"query": "seed"}, nil)
				_ = h.call("save", map[string]string{"title": fmt.Sprintf("N%d", i), "content": fmt.Sprintf("v%d [[N0]]", j)}, nil)
				_ = h.call("graph", nil, nil)
				_, _ = h.handlers["view"](context.Background(), []byte(`{"version":1}`))
			}
		}(i)
	}
	wg.Wait()
	var n noteOut
	h.must(t, "open", map[string]string{"title": "N0"}, &n)
	if len(n.Backlinks) != 10 {
		t.Fatalf("backlinks after concurrent saves = %d, want 10", len(n.Backlinks))
	}
}

func TestManifestDeclaresMinimalCapabilities(t *testing.T) {
	m := Manifest()
	if m.ID != ID || m.Name != "Compendium" || m.EffectiveIsolation() != sdk.IsolationInProc {
		t.Fatalf("manifest identity = %+v", m)
	}
	if !reflect.DeepEqual(m.Permissions, []string{sdk.CapIPC}) || len(m.PathGrants) != 0 {
		t.Fatalf("Compendium must need only ipc + its own private storage, got %v %v", m.Permissions, m.PathGrants)
	}
}

func TestRoutesStayInsideAppPrivateStorage(t *testing.T) {
	// The mock rejects any path outside /apps/data/<id>; a full lifecycle
	// must never trip it (the e2e test proves the same against the runtime).
	h := openHarness(t, newMockFS())
	h.must(t, "create", map[string]string{"title": "X", "content": "[[Y]]"}, nil)
	h.must(t, "rename", map[string]string{"title": "X", "new_title": "Z"}, nil)
	h.must(t, "trash", map[string]string{"title": "Z"}, nil)
	h.must(t, "export", map[string]string{}, nil)
	h.must(t, "today", nil, nil)
	if err := h.call("import", map[string]string{"path": "/users/guest/documents/x.md"}, nil); err == nil {
		t.Fatal("import from an ungranted path succeeded")
	}
}
