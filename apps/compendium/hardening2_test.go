package compendium

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Hardening pass 2: regression tests that accompany the reviewer's
// round-2 reproductions in review_v2_test.go. They cover the same
// findings on the in-memory VFS model (so a finding that only shows on
// macOS APFS is red on Linux too) and the further instances of each
// pattern found by self-audit.

// [P1-5] On a normalization-insensitive host (APFS), "Caf\u00e9" and
// "Cafe\u0301" name the same file. Neither drafts nor notes may silently
// replace each other, whether the earlier title is known to memory or
// only present on disk.
func TestNormalizationCollisionOnAPFSModel(t *testing.T) {
	const pre, dec = "Caf\u00e9", "Cafe\u0301"
	newFS := func() *mockFS { fs := newMockFS(); fs.foldNorm = true; fs.foldCase = true; return fs }

	t.Run("draft", func(t *testing.T) {
		fs := newFS()
		h := openHarness(t, fs)
		h.must(t, "edit", map[string]string{"title": pre, "content": "first draft"}, nil)
		t.Logf("second edit: %v", h.call("edit", map[string]string{"title": dec, "content": "second draft"}, nil))
		got := draftContents(t, mustReopen(t, fs))
		found := false
		for _, c := range got {
			found = found || c == "first draft"
		}
		if !found {
			t.Fatalf("normalization-equivalent title destroyed the first draft: %+v", got)
		}
	})
	t.Run("note", func(t *testing.T) {
		fs := newFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": pre, "content": "one"}, nil)
		if err := h.call("create", map[string]string{"title": dec, "content": "two"}, nil); err == nil {
			t.Fatal("a normalization-equivalent second note was accepted")
		}
		if err := h.call("save", map[string]string{"title": dec, "content": "two"}, nil); err == nil {
			t.Fatal("saving under a normalization-equivalent title was accepted")
		}
		if got := openContent(t, mustReopen(t, fs), pre); got != "one" {
			t.Fatalf("first note now holds %q", got)
		}
	})
	t.Run("rename", func(t *testing.T) {
		fs := newFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": pre, "content": "one"}, nil)
		h.must(t, "create", map[string]string{"title": "X", "content": "x"}, nil)
		if err := h.call("rename", map[string]string{"title": "X", "new_title": dec}, nil); err == nil {
			t.Fatal("rename onto a normalization-equivalent title was accepted")
		}
		h2 := mustReopen(t, fs)
		if openContent(t, h2, pre) != "one" || openContent(t, h2, "X") != "x" {
			t.Fatal("rename collision changed a note")
		}
	})
	t.Run("draft_vs_note", func(t *testing.T) {
		fs := newFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": pre, "content": "one"}, nil)
		h.must(t, "edit", map[string]string{"title": pre, "content": "one edited"}, nil)
		_ = h.call("edit", map[string]string{"title": dec, "content": "other"}, nil)
		got := draftContents(t, mustReopen(t, fs))
		if got[pre] != "one edited" {
			t.Fatalf("draft for the existing note replaced: %+v", got)
		}
	})
	t.Run("physical", func(t *testing.T) {
		// The first draft exists on disk only (written by another process
		// after this one loaded): the host itself must refuse the alias.
		fs := newFS()
		h := openHarness(t, fs)
		h.must(t, "status", nil, nil)
		raw, _ := json.Marshal(draft{Title: pre, Content: "on disk only"})
		fs.put(testDrafts+"/"+pre+".json", raw)
		_ = h.call("edit", map[string]string{"title": dec, "content": "second"}, nil)
		data, _ := fs.get(testDrafts + "/" + pre + ".json")
		if !strings.Contains(string(data), "on disk only") {
			t.Fatalf("draft known only to the host was overwritten: %s", data)
		}
	})
	t.Run("fold", func(t *testing.T) {
		for _, c := range [][2]string{{pre, dec}, {"\u00c5ngstr\u00f6m", "A\u030angstro\u0308m"}, {"\u212b", "\u00e5"}, {"\u1e69", "s\u0307\u0323"}, {"CAF\u00c9", "cafe\u0301"}} {
			if normKey(c[0]) != normKey(c[1]) {
				t.Errorf("normKey(%q) != normKey(%q)", c[0], c[1])
			}
		}
		for _, c := range [][2]string{{"Cafe", pre}, {"resume", "r\u00e9sum\u00e9"}} {
			if normKey(c[0]) == normKey(c[1]) {
				t.Errorf("normKey(%q) == normKey(%q): plain letters must stay distinct", c[0], c[1])
			}
		}
	})
}

// [P1-1] A failed restore leaves the whole trash entry (note, history,
// draft) in the trash, listed and restorable once storage works again.
func TestRestoreSalvageKeepsTrashEntryWhole(t *testing.T) {
	fs := newMockFS()
	h := openHarness(t, fs)
	h.must(t, "create", map[string]string{"title": "A", "content": "v1"}, nil)
	h.must(t, "save", map[string]string{"title": "A", "content": "v2"}, nil)
	h.must(t, "edit", map[string]string{"title": "A", "content": "draft"}, nil)
	var tr struct {
		TrashID string `json:"trash_id"`
	}
	h.must(t, "trash", map[string]string{"title": "A"}, &tr)
	entry := testRoot + "/trash/" + tr.TrashID
	fs.fail = func(method, p string) error {
		if method == "fs/rename" && p == entry+"/A.md" {
			return errors.New("persistent failure")
		}
		return nil
	}
	if err := h.call("restore", map[string]string{"trash_id": tr.TrashID}, nil); err == nil {
		t.Fatal("expected restore failure")
	}
	var st warnStatus
	mustReopen(t, fs).must(t, "status", nil, &st)
	for _, p := range []string{entry + "/A.md", entry + "/draft.json"} {
		if _, ok := fs.get(p); !ok {
			t.Fatalf("%s left the trash entry after a failed restore; files=%v", p, fs.paths(testRoot))
		}
	}
	if len(fs.paths(entry+"/history/")) == 0 {
		t.Fatalf("history split from its trashed note; files=%v", fs.paths(testRoot))
	}
	if len(st.Index.Warnings) == 0 {
		t.Fatal("the failed restore was not reported")
	}
	fs.fail = nil
	h3 := mustReopen(t, fs)
	var list trashListOut
	h3.must(t, "trash_list", nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("trash entry not listed: %+v", list)
	}
	h3.must(t, "restore", map[string]string{"trash_id": tr.TrashID}, nil)
	if openContent(t, h3, "A") != "v2" || draftContents(t, h3)["A"] != "draft" {
		t.Fatal("restore after recovery lost content")
	}
}

// [P1-2] A draft .recover artifact is merged by recency: only a strictly
// newer artifact replaces the staged draft (as a later staging would); an
// older or equally old one is kept beside it under its own title.
func TestDraftArtifactMergedByRecency(t *testing.T) {
	stamp := time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC) // the harness clock
	for _, c := range []struct {
		name string
		at   time.Time
		main string
	}{
		{"older", stamp.Add(-time.Hour), "staged"},
		{"equal", stamp, "staged"},
		{"newer", stamp.Add(time.Hour), "artifact"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fs := newMockFS()
			h := openHarness(t, fs)
			h.must(t, "create", map[string]string{"title": "A", "content": "saved"}, nil)
			h.must(t, "edit", map[string]string{"title": "A", "content": "staged"}, nil)
			raw, _ := json.Marshal(draft{Title: "A", Content: "artifact", StagedAt: c.at})
			fs.put(testDrafts+"/A.json.recover", raw)
			got := draftContents(t, mustReopen(t, fs))
			if got["A"] != c.main {
				t.Fatalf("draft for A = %q, want %q (all: %+v)", got["A"], c.main, got)
			}
			var all []string
			for _, v := range got {
				all = append(all, v)
			}
			// A strictly newer artifact supersedes the older staged text of
			// the same draft, as any later staging would; otherwise both stay.
			if c.name != "newer" && !contains(all, "staged") || !contains(all, "artifact") {
				t.Fatalf("a draft version was discarded: %+v", got)
			}
			if _, ok := fs.get(testDrafts + "/A.json.recover"); ok {
				t.Fatal("artifact left behind after both versions were staged")
			}
		})
	}
}

// [P1-3] Routes resolve their state after a pending operation completes:
// restoring a draft after an interrupted rename commits the draft as the
// rename rewrote it, not a stale copy that resurrects the old link.
func TestRecoverAfterPendingRenameUsesCurrentDraft(t *testing.T) {
	fs := newMockFS()
	h := openHarness(t, fs)
	h.must(t, "create", map[string]string{"title": "B", "content": "b"}, nil)
	h.must(t, "create", map[string]string{"title": "A", "content": "[[B]]"}, nil)
	h.must(t, "edit", map[string]string{"title": "A", "content": "[[B]] edited"}, nil)
	once := true
	fs.fail = func(method, p string) error {
		if once && method == "fs/remove" && p == testJournal {
			once = false
			return errors.New("transient")
		}
		return nil
	}
	if err := h.call("rename", map[string]string{"title": "B", "new_title": "Beta"}, nil); err == nil {
		t.Fatal("expected the rename to stay pending")
	}
	if err := h.call("recover", map[string]any{"title": "A", "restore": true}, nil); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := openContent(t, h, "A"); got != "[[Beta]] edited" {
		t.Fatalf("restored a stale draft: %q", got)
	}
	if err := h.call("open", map[string]string{"title": "B"}, nil); err == nil {
		t.Fatal("the renamed-away title came back")
	}
}

// [P1-4] Rename preflights every derived write: a rewritten draft over the
// note limit, and a vault pushed over its byte limit, are rejected before
// anything changes, naming the notes involved.
func TestRenamePreflightsDerivedSizes(t *testing.T) {
	t.Run("draft", func(t *testing.T) {
		fs := newMockFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": "A"}, nil)
		h.must(t, "create", map[string]string{"title": "D", "content": "d"}, nil)
		body := strings.Repeat("y", MaxNoteBytes-len("[[A]]")) + "[[A]]"
		h.must(t, "edit", map[string]string{"title": "D", "content": body}, nil)
		err := h.call("rename", map[string]string{"title": "A", "new_title": "LongerTitle"}, nil)
		if err == nil || !strings.Contains(err.Error(), "D") {
			t.Fatalf("oversized draft rewrite not rejected by name: %v", err)
		}
		if draftContents(t, mustReopen(t, fs))["D"] != body {
			t.Fatal("rejected rename changed the draft")
		}
	})
	t.Run("vault", func(t *testing.T) {
		h := openHarness(t, newMockFS())
		h.must(t, "create", map[string]string{"title": "A"}, nil)
		h.must(t, "create", map[string]string{"title": "L", "content": "[[A]]"}, nil)
		h.app.v.bytes = MaxVaultBytes - 2
		if err := h.call("rename", map[string]string{"title": "A", "new_title": "Longer"}, nil); err == nil {
			t.Fatal("rename that grows the vault past its limit was accepted")
		}
	})
	t.Run("message", func(t *testing.T) {
		h := openHarness(t, newMockFS())
		h.must(t, "create", map[string]string{"title": "A"}, nil)
		body := strings.Repeat("x", MaxNoteBytes-len("[[A]]")) + "[[A]]"
		h.must(t, "create", map[string]string{"title": "Large", "content": body}, nil)
		err := h.call("rename", map[string]string{"title": "A", "new_title": "LongTitle"}, nil)
		if err == nil || !strings.Contains(err.Error(), "Large") {
			t.Fatalf("error does not name the note: %v", err)
		}
	})
}

// [P1-7] A note changed on disk outside Compendium is never overwritten
// silently: not by save (the edit is kept as a draft and the external
// version becomes the note), and not by a rename's link rewrite.
func TestExternalEditsNeverOverwritten(t *testing.T) {
	t.Run("save", func(t *testing.T) {
		fs := newMockFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": "A", "content": "initial"}, nil)
		fs.put(testVault+"/A.md", []byte("external edit"))
		err := h.call("save", map[string]string{"title": "A", "content": "app edit"}, nil)
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("expected an external-change conflict, got %v", err)
		}
		if got := openContent(t, h, "A"); got != "external edit" {
			t.Fatalf("note shows %q, want the external version", got)
		}
		if draftContents(t, h)["A"] != "app edit" {
			t.Fatal("the app's edit was not kept as a draft")
		}
		// Committing the draft now is a deliberate overwrite; the external
		// version goes to history.
		h.must(t, "recover", map[string]any{"title": "A", "restore": true}, nil)
		var hist historyOut
		h.must(t, "history", map[string]string{"title": "A"}, &hist)
		found := false
		for _, v := range hist.Versions {
			found = found || v.SHA256 == digest("external edit")
		}
		if !found {
			t.Fatal("external version not kept in history")
		}
	})
	t.Run("rename", func(t *testing.T) {
		fs := newMockFS()
		h := openHarness(t, fs)
		h.must(t, "create", map[string]string{"title": "B", "content": "b"}, nil)
		h.must(t, "create", map[string]string{"title": "A", "content": "[[B]]"}, nil)
		fs.put(testVault+"/A.md", []byte("[[B]] plus external text"))
		_ = h.call("rename", map[string]string{"title": "B", "new_title": "Beta"}, nil)
		data, _ := fs.get(testVault + "/A.md")
		if !strings.Contains(string(data), "plus external text") {
			t.Fatalf("rename rewrite destroyed an external edit: %q", data)
		}
	})
}

// [Self-audit] Restoring from trash cannot push the vault past the limits
// that make open skip notes.
func TestRestoreRespectsVaultLimits(t *testing.T) {
	fs := newMockFS()
	h := openHarness(t, fs)
	h.must(t, "create", map[string]string{"title": "A", "content": strings.Repeat("a", 1000)}, nil)
	var tr struct {
		TrashID string `json:"trash_id"`
	}
	h.must(t, "trash", map[string]string{"title": "A"}, &tr)
	h.app.v.bytes = MaxVaultBytes - 10
	if err := h.call("restore", map[string]string{"trash_id": tr.TrashID}, nil); err == nil {
		t.Fatal("restore over the vault byte limit was accepted")
	}
	h.app.v.bytes = 0
	var list trashListOut
	h.must(t, "trash_list", nil, &list)
	if len(list.Items) != 1 {
		t.Fatal("refused restore lost the trash entry")
	}
}

// [P2-8] Anything Compendium exports it can import again: export refuses
// a vault whose inflated size is over the import limit instead of writing
// an archive the import would reject.
func TestExportStaysWithinImportLimits(t *testing.T) {
	h := openHarness(t, newMockFS())
	for i := 0; i < 34; i++ {
		h.must(t, "create", map[string]string{"title": fmt.Sprintf("Z%02d", i), "content": strings.Repeat("0", 1<<20)}, nil)
	}
	var export exportOut
	if err := h.call("export", nil, &export); err == nil {
		fresh := openHarness(t, newMockFS())
		if err := fresh.call("import", map[string]string{"zip_base64": export.DataBase64}, nil); err != nil {
			t.Fatalf("export produced an archive its own import refuses: %v", err)
		}
	}
}

// [P2-9] Prefix conjunction stays exact however many terms a prefix
// matches.
func TestPrefixConjunctionManyExpansions(t *testing.T) {
	h := openHarness(t, newMockFS())
	for i := 0; i < 600; i++ {
		body := fmt.Sprintf("alpha%03d", i)
		if i%150 == 149 {
			body += " needle"
		}
		h.must(t, "create", map[string]string{"title": fmt.Sprintf("N%03d", i), "content": body}, nil)
	}
	var out searchOut
	h.must(t, "search", map[string]any{"query": "alpha needle", "limit": 100}, &out)
	if out.Total != 4 {
		t.Fatalf("total = %d, want 4: %+v", out.Total, out.Results)
	}
	h.must(t, "search", map[string]any{"query": "needle alph", "limit": 100}, &out)
	if out.Total != 4 {
		t.Fatalf("term order changed the result: total = %d", out.Total)
	}
	h.must(t, "search", map[string]any{"query": "alpha", "limit": 100}, &out)
	if out.Total != 600 {
		t.Fatalf("bare prefix total = %d, want 600", out.Total)
	}
}

// [P2-10] Snippets are bounded and rune-safe however long the matched word.
func TestSnippetBoundedForLongTokens(t *testing.T) {
	for _, body := range []string{
		"ab" + strings.Repeat("x", 300000),
		strings.Repeat("é", 100000) + " ab " + strings.Repeat("日", 100000),
		"ab" + strings.Repeat("ü", 200000) + " tail",
	} {
		s := snippet(body, []string{"ab"})
		if len(s) > maxSnippetBytes || !utf8.ValidString(s) || !strings.Contains(s, "«") {
			t.Fatalf("snippet len=%d valid=%v marked=%v", len(s), utf8.ValidString(s), strings.Contains(s, "«"))
		}
	}
}

// [Self-audit] Listing drafts stays inside the IPC frame however large the
// drafts; their full content stays reachable one at a time.
func TestDraftsResponseBounded(t *testing.T) {
	h := openHarness(t, newMockFS())
	body := strings.Repeat("d", 900<<10)
	for i := 0; i < 6; i++ {
		h.must(t, "edit", map[string]string{"title": fmt.Sprintf("D%d", i), "content": body}, nil)
	}
	res, err := h.handlers["drafts"](context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	if len(raw) > 4<<20 {
		t.Fatalf("drafts response is %d bytes", len(raw))
	}
	var one draftsOut
	h.must(t, "drafts", map[string]string{"title": "D5"}, &one)
	if len(one.Drafts) != 1 || one.Drafts[0].Content != body {
		t.Fatal("a single draft cannot be fetched in full")
	}
}

// [P1-6] Rewriting links keeps the text valid UTF-8 and changes nothing
// but the link targets: everything outside the renamed spans is
// byte-identical.
func FuzzRewriteLinksPreservesText(f *testing.F) {
	for _, s := range []string{"[[\u00a0B]]", "[[ B ]] x", "[[\u3000B\u2003|a]]", "é[[B#h]]ü", "[[b]] `[[B]]`", "[[\u0085B]]"} {
		f.Add(s, "B")
	}
	f.Fuzz(func(t *testing.T, s, target string) {
		if !utf8.ValidString(s) {
			return
		}
		var spans []span
		scanMarkdown(s, func(sp span) {
			if fold(sp.Name) == fold(target) {
				spans = append(spans, sp)
			}
		}, nil)
		const repl = "Rénamed"
		out, n := rewriteLinks(s, target, repl)
		if n != len(spans) {
			t.Fatalf("rewrote %d links, scanner found %d", n, len(spans))
		}
		if !utf8.ValidString(out) {
			t.Fatalf("rewrite of %q produced invalid UTF-8 %q", s, out)
		}
		// Reassemble the expected output from the untouched gaps.
		var want strings.Builder
		last := 0
		for _, sp := range spans {
			if !utf8.ValidString(s[last:sp.NameStart]) || !utf8.ValidString(s[sp.NameStart:sp.NameEnd]) {
				t.Fatalf("span %+v cuts a rune in %q", sp, s)
			}
			want.WriteString(s[last:sp.NameStart])
			want.WriteString(repl)
			last = sp.NameEnd
		}
		want.WriteString(s[last:])
		if out != want.String() {
			t.Fatalf("text outside the links changed: %q -> %q", s, out)
		}
	})
}

// [P1-2] On the real host VFS: once a later draft save succeeds, the
// artifact of the earlier failed save is gone, so nothing stale is offered.
func TestSuccessfulDraftSaveSupersedesArtifact(t *testing.T) {
	dir := t.TempDir()
	adapter := newHostAdapter(t, dir)
	h := openHostHarness(t, adapter)
	h.must(t, "create", map[string]string{"title": "A", "content": "saved"}, nil)
	h.must(t, "edit", map[string]string{"title": "A", "content": "draft v1"}, nil)
	adapter.host.SetSaveHooks(nil, func(string, string) error { return errors.New("rename failed") })
	if err := h.call("edit", map[string]string{"title": "A", "content": "draft v2"}, nil); err == nil {
		t.Fatal("expected failed draft save")
	}
	adapter.host.SetSaveHooks(nil, nil)
	h.must(t, "edit", map[string]string{"title": "A", "content": "draft v3"}, nil)
	if _, err := os.Stat(filepath.Join(dir, "apps/data", ID, "drafts", "A.json.recover")); !os.IsNotExist(err) {
		t.Fatalf("superseded artifact still present (stat err %v)", err)
	}
	var d draftsOut
	openHostHarness(t, newHostAdapter(t, dir)).must(t, "drafts", nil, &d)
	if len(d.Drafts) != 1 || d.Drafts[0].Content != "draft v3" {
		t.Fatalf("drafts after reopen = %+v", d.Drafts)
	}
}

// [P1-5] A rename that only changes Unicode composition of the same note
// goes through a temporary name, like a case-only rename, and succeeds on
// a normalization-insensitive host.
func TestNormalizationOnlyRenameOnAPFSModel(t *testing.T) {
	fs := newMockFS()
	fs.foldNorm, fs.foldCase = true, true
	h := openHarness(t, fs)
	h.must(t, "create", map[string]string{"title": "Caf\u00e9", "content": "[[Caf\u00e9]]"}, nil)
	h.must(t, "edit", map[string]string{"title": "Caf\u00e9", "content": "draft"}, nil)
	h.must(t, "rename", map[string]string{"title": "Caf\u00e9", "new_title": "Cafe\u0301"}, nil)
	h2 := mustReopen(t, fs)
	if got := openContent(t, h2, "Cafe\u0301"); got != "[[Cafe\u0301]]" {
		t.Fatalf("content after normalization-only rename = %q", got)
	}
	if draftContents(t, h2)["Cafe\u0301"] != "draft" {
		t.Fatalf("draft did not follow: %+v", draftContents(t, h2))
	}
	if _, ok := fs.get(testJournal); ok {
		t.Fatal("journal left behind")
	}
}

// [Self-audit] Opening a note whose JSON encoding would exceed the IPC
// frame (encoding/json writes <, > and & as six bytes) still answers
// within the frame, with the text intact.
func TestOpenResponseBoundedForEscapingContent(t *testing.T) {
	h := openHarness(t, newMockFS())
	body := strings.Repeat("<a&b>", (MaxNoteBytes-16)/5)
	h.must(t, "create", map[string]string{"title": "Html", "content": body}, nil)
	res, err := h.handlers["open"](context.Background(), json.RawMessage(`{"title":"Html"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(res)
	if len(raw) > 4<<20 {
		t.Fatalf("open response is %d bytes", len(raw))
	}
	var out struct {
		Content       string `json:"content"`
		ContentBase64 []byte `json:"content_base64"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Content != body && string(out.ContentBase64) != body {
		t.Fatal("note text not carried in either form")
	}
}

// [Self-audit] A draft is a derived write too: staging a valid 1 MiB note
// full of <, > and & must fit the IPC write frame once encoded (draft JSON
// is written without HTML escaping), and survive a reopen.
func TestDraftOfEscapingContentFitsFrame(t *testing.T) {
	fs := newMockFS()
	fs.maxFrame = 4 << 20
	h := openHarness(t, fs)
	body := strings.Repeat("<a&b>", (MaxNoteBytes-16)/5)
	h.must(t, "create", map[string]string{"title": "Html", "content": "x"}, nil)
	h.must(t, "edit", map[string]string{"title": "Html", "content": body}, nil)
	var one struct {
		Drafts []struct {
			Content string `json:"content"`
			Base64  []byte `json:"content_base64"`
		} `json:"drafts"`
	}
	mustReopen(t, fs).must(t, "drafts", map[string]string{"title": "Html"}, &one)
	if len(one.Drafts) != 1 || one.Drafts[0].Content != body && string(one.Drafts[0].Base64) != body {
		t.Fatal("draft not staged intact")
	}
}

// normKey is a fold: idempotent, coarser than fold, and blind to whether
// a table letter is precomposed or spelled as base plus marks.
func FuzzNormKey(f *testing.F) {
	for _, s := range []string{"Caf\u00e9", "Cafe\u0301", "\u212b", "\u1e69", "\ud55c\uae00", "\u1112\u1161\u11ab", "ΆΈ", "\u0130", "\u13a9", "\uab79", "plain"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return
		}
		k := normKey(s)
		if normKey(k) != k {
			t.Fatalf("normKey not idempotent: %q -> %q -> %q", s, k, normKey(k))
		}
		if normKey(fold(s)) != k || normKey(strings.ToUpper(s)) != normKey(strings.ToUpper(k)) {
			t.Fatalf("normKey disagrees with case folding for %q", s)
		}
		var dec strings.Builder
		for _, r := range s {
			if d := decompositions[r]; d != "" {
				dec.WriteString(d)
			} else {
				dec.WriteRune(r)
			}
		}
		if normKey(dec.String()) != k {
			t.Fatalf("decomposed spelling of %q has a different key", s)
		}
	})
}
