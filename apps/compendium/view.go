package compendium

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"gostalgia/sdk"
)

// The presentation is data only: lists of items, single-line fields, and
// semantic actions in an sdk.View. The shell owns rendering, focus, keys,
// and styling; Compendium never draws, positions, or colors anything.

type viewMode string

const (
	modeHome     viewMode = "home"
	modeNote     viewMode = "note"
	modeGraph    viewMode = "graph"
	modeTags     viewMode = "tags"
	modeTag      viewMode = "tag"
	modeTrash    viewMode = "trash"
	modeRecovery viewMode = "recovery"
	modeHelp     viewMode = "help"

	fieldLimit = 4096
	// areaLimit is the version-3 multiline field budget.
	areaLimit = 32768
	maxItems  = 100
)

type uiState struct {
	mode      viewMode
	rev       int // bumps field IDs so the shell resets inputs
	query     string
	results   []searchResult
	total     int
	searched  bool
	note      string // open note title
	buffer    string
	dirty     bool
	baseSHA   string // hash of the saved version the buffer is based on ("" = none existed)
	conflict  bool   // a Save found the note changed underneath a dirty buffer
	tag       string
	status    string
	err       string
	recovered bool // recovery prompt already offered this launch
}

func hid(prefix, key string) string {
	sum := sha256.Sum256([]byte(key))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}

func (u *uiState) qField() string      { return fmt.Sprintf("q%d", u.rev) }
func (u *uiState) bodyField() string   { return fmt.Sprintf("body%d", u.rev) }
func (u *uiState) renameField() string { return fmt.Sprintf("rename%d", u.rev) }

func label(s string) string {
	s = flatten(s)
	if s == "" {
		s = "(untitled)"
	}
	return clip(s, 256)
}

func detail(s string) string { return clip(s, fieldLimit) }

func (c *Compendium) view(ctx context.Context) (sdk.View, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(ctx); err != nil {
		return c.errorView(err), nil
	}
	c.settlePendingLocked(ctx)
	if !c.ui.recovered {
		c.ui.recovered = true
		if len(c.v.drafts) > 0 {
			c.ui.mode = modeRecovery
		}
	}
	return c.render(sdk.PresentationRequestVersion(ctx)), nil
}

func (c *Compendium) errorView(err error) sdk.View {
	return sdk.View{
		Title: "Compendium", State: sdk.ViewError, Error: detail(err.Error()),
		Items:   []sdk.Item{{ID: "storage", Label: "Storage", Detail: rootDir}},
		Actions: []sdk.Action{{ID: "retry", Label: "Retry"}},
		Status:  "The vault could not be opened. Nothing was modified.",
	}
}

func (c *Compendium) act(ctx context.Context, p sdk.ActionRequest) (sdk.View, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLoadedLocked(ctx); err != nil {
		return c.errorView(err), nil
	}
	c.settlePendingLocked(ctx)
	u := &c.ui
	u.err, u.status = "", ""
	if u.mode == modeNote {
		if val, ok := p.Values[u.bodyField()]; ok && val != u.buffer {
			u.buffer = val
			u.dirty = digest(val) != u.baseSHA
			base := u.baseSHA
			if err := c.v.stageDraftOn(ctx, u.note, val, &base); err != nil {
				u.err = "Could not stage draft: " + err.Error()
			}
		}
	}
	if err := c.dispatch(ctx, p); err != nil {
		u.err = err.Error()
	}
	return c.render(sdk.PresentationRequestVersion(ctx)), nil
}

func (c *Compendium) dispatch(ctx context.Context, p sdk.ActionRequest) error {
	u := &c.ui
	switch p.Action {
	case "retry", "home":
		c.goHome()
	case "run":
		return c.runCommand(ctx, strings.TrimSpace(p.Values[u.qField()]))
	case "new":
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.Values[u.qField()]), "new "))
		return c.newNote(ctx, title)
	case "today":
		n, _, err := c.v.today(ctx)
		if err != nil {
			return err
		}
		c.openNote(n.title)
	case "graph":
		u.mode = modeGraph
	case "tags":
		u.mode = modeTags
	case "trash_bin":
		u.mode = modeTrash
	case "recovery":
		u.mode = modeRecovery
	case "help":
		u.mode = modeHelp
	case "export_vault":
		res, err := c.v.exportVault(ctx, "")
		if err != nil {
			return err
		}
		u.status = fmt.Sprintf("Exported %d notes to %s (%d bytes).", res.Files, res.Path, res.Size)
	case "rebuild_index":
		if err := c.v.load(ctx, true); err != nil {
			c.loaded = false
			return err
		}
		u.status = fmt.Sprintf("Index rebuilt from %d Markdown files.", len(c.v.notes))
	case "open", "follow":
		return c.follow(ctx, p)
	case "save", "overwrite":
		return c.commitEditor(ctx, p.Action == "overwrite")
	case "reload":
		c.openNote(u.note)
		if c.v.drafts[fold(u.note)] != nil {
			u.status = "Loaded the saved version. Your edits are kept as a draft; open Drafts to bring them back."
		}
	case "rename":
		return c.renameCurrent(ctx, strings.TrimSpace(p.Values[u.renameField()]))
	case "trash":
		id, err := c.v.trashNote(ctx, u.note)
		if err != nil {
			return err
		}
		title := u.note
		c.goHome()
		u.status = fmt.Sprintf("Moved %q to trash (%s). Restore it from Trash.", title, id)
	case "export_note":
		res, err := c.v.exportNote(ctx, u.note, "")
		if err != nil {
			return err
		}
		u.status = fmt.Sprintf("Exported %s to %s (%d bytes, unchanged).", u.note, res.Path, res.Size)
	case "restore":
		id := strings.TrimPrefix(p.ItemID, "x-")
		n, err := c.v.restoreNote(ctx, id)
		if err != nil {
			return err
		}
		c.openNote(n.title)
		u.status = fmt.Sprintf("Restored %q with its links, tags, and history.", n.title)
	case "restore_draft", "discard_draft":
		d := c.draftByItem(p.ItemID)
		if d == nil {
			return fmt.Errorf("select a draft first")
		}
		if p.Action == "discard_draft" {
			if err := c.v.clearDraft(ctx, d.Title); err != nil {
				return err
			}
			u.status = fmt.Sprintf("Discarded the draft for %q.", d.Title)
			if len(c.v.drafts) == 0 {
				c.goHome()
			}
			return nil
		}
		c.openNote(d.Title)
		// The draft was made on top of BaseSHA; if the note has moved on
		// since, Save reports the conflict instead of overwriting.
		u.buffer, u.dirty, u.baseSHA = d.Content, true, d.BaseSHA
		u.status = "Draft restored into the editor. Save to commit it."
	default:
		return fmt.Errorf("unknown action %q", p.Action)
	}
	return nil
}

// commitEditor saves the editor buffer. A clean editor has nothing to
// save: Save just brings it up to date with the stored note (which may
// have been changed through the routes meanwhile). A dirty editor whose
// base version is no longer the stored one is a conflict: nothing is
// written, the edits stay staged as a draft, and the user chooses
// Overwrite (force) or Reload.
func (c *Compendium) commitEditor(ctx context.Context, force bool) error {
	u := &c.ui
	n := c.v.notes[fold(u.note)]
	cur := ""
	if n != nil {
		cur = n.sha
	}
	if !u.dirty {
		u.conflict = false
		if n != nil && cur != u.baseSHA {
			c.openNote(n.title)
			u.status = "Nothing to save. This note was changed elsewhere; the editor now shows the saved version."
		} else {
			u.status = "Nothing to save."
		}
		return nil
	}
	if cur != u.baseSHA && !force {
		u.conflict = true
		what := "changed elsewhere"
		if n == nil {
			what = "removed elsewhere"
		}
		return fmt.Errorf("not saved: %q was %s since you started editing. Your edits are kept as a draft. Overwrite saves your version anyway; Reload shows the saved one", u.note, what)
	}
	saved, err := c.v.save(ctx, u.note, u.buffer)
	if err != nil {
		return err
	}
	u.note, u.dirty, u.conflict, u.baseSHA = saved.title, false, false, saved.sha
	u.status = fmt.Sprintf("Saved %s (%d bytes).", saved.title, len(saved.content))
	return nil
}

func (c *Compendium) goHome() {
	u := &c.ui
	u.mode, u.query, u.results, u.searched = modeHome, "", nil, false
	u.rev++
}

func (c *Compendium) openNote(title string) {
	u := &c.ui
	u.mode, u.note, u.dirty, u.buffer, u.baseSHA, u.conflict = modeNote, title, false, "", "", false
	if n := c.v.notes[fold(title)]; n != nil {
		u.note, u.buffer, u.baseSHA = n.title, n.content, n.sha
		if c.v.drafts[fold(title)] != nil {
			u.status = "An unsaved draft of this note exists; open Drafts to restore it."
		}
	}
	u.rev++
}

func (c *Compendium) newNote(ctx context.Context, title string) error {
	if title == "" {
		return fmt.Errorf("type a title in the search field, then New")
	}
	n, err := c.v.create(ctx, title, "")
	if err != nil {
		return err
	}
	c.openNote(n.title)
	c.ui.status = fmt.Sprintf("Created %q.", n.title)
	return nil
}

// runCommand interprets the search field: a few verbs, otherwise search.
func (c *Compendium) runCommand(ctx context.Context, cmd string) error {
	u := &c.ui
	verb, arg, _ := strings.Cut(cmd, " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(verb) {
	case "":
		c.goHome()
	case "today":
		n, created, err := c.v.today(ctx)
		if err != nil {
			return err
		}
		c.openNote(n.title)
		if created {
			u.status = "Created today's note."
		}
	case "new", "create":
		return c.newNote(ctx, arg)
	case "open":
		n, err := c.v.get(arg)
		if err != nil {
			return fmt.Errorf("%v; type `new %s` to create it", err, arg)
		}
		c.openNote(n.title)
	case "graph":
		u.mode = modeGraph
	case "tags":
		u.mode = modeTags
	case "tag":
		u.mode, u.tag = modeTag, strings.ToLower(strings.TrimPrefix(arg, "#"))
	case "trash":
		u.mode = modeTrash
	case "drafts", "recovery":
		u.mode = modeRecovery
	case "help":
		u.mode = modeHelp
	default:
		u.mode, u.query, u.searched = modeHome, cmd, true
		start := time.Now()
		u.results, u.total = c.v.find(cmd, 50)
		u.status = fmt.Sprintf("%d matches for %q in %.1fms.", u.total, cmd, float64(time.Since(start).Microseconds())/1000)
		return nil
	}
	return nil
}

func (c *Compendium) follow(ctx context.Context, p sdk.ActionRequest) error {
	id := p.ItemID
	switch {
	case strings.HasPrefix(id, "n-") || strings.HasPrefix(id, "b-"):
		n := c.noteByHash(id[2:])
		if n == nil {
			return fmt.Errorf("that note no longer exists")
		}
		c.openNote(n.title)
	case strings.HasPrefix(id, "l-") || strings.HasPrefix(id, "u-"):
		target := c.linkTargetByHash(id[2:])
		if target == "" {
			return fmt.Errorf("that link no longer exists")
		}
		if n := c.v.notes[fold(target)]; n != nil {
			c.openNote(n.title)
			return nil
		}
		if err := c.newNote(ctx, target); err != nil {
			return err
		}
		c.ui.status = fmt.Sprintf("Created %q from a broken link.", target)
	case strings.HasPrefix(id, "t-"):
		for _, t := range c.v.tags() {
			if hid("t", t.Tag) == id {
				c.ui.mode, c.ui.tag = modeTag, t.Tag
				return nil
			}
		}
		return fmt.Errorf("that tag no longer exists")
	default:
		if q := strings.TrimSpace(p.Values[c.ui.qField()]); q != "" && c.ui.mode == modeHome {
			return c.runCommand(ctx, "open "+q)
		}
		return fmt.Errorf("select a note, link, backlink, or tag first")
	}
	return nil
}

func (c *Compendium) noteByHash(h string) *note {
	for k, n := range c.v.notes {
		if hid("", k)[1:] == h {
			return n
		}
	}
	return nil
}

func (c *Compendium) linkTargetByHash(h string) string {
	cands := parseNote(c.ui.buffer).Links
	if c.ui.mode == modeGraph {
		for _, u := range c.v.graph().Unresolved {
			cands = append(cands, u.Target)
		}
	}
	for _, t := range cands {
		if hid("", fold(t))[1:] == h {
			return t
		}
	}
	return ""
}

func (c *Compendium) draftByItem(id string) *draft {
	for k, d := range c.v.drafts {
		if hid("d", k) == id {
			return d
		}
	}
	return nil
}

func (c *Compendium) renameCurrent(ctx context.Context, newTitle string) error {
	u := &c.ui
	if newTitle == "" {
		return fmt.Errorf("type the new title in “Rename to”, then Rename")
	}
	if u.dirty {
		if err := c.commitEditor(ctx, false); err != nil {
			return err
		}
	}
	res, err := c.v.rename(ctx, u.note, newTitle)
	if err != nil {
		return err
	}
	c.openNote(res.To)
	u.status = fmt.Sprintf("Renamed %q → %q; rewrote %d links in %d notes.", res.From, res.To, res.RewrittenLinks, len(res.RewrittenNotes))
	if len(res.Orphaned) > 0 {
		var names []string
		for _, o := range res.Orphaned {
			names = append(names, o.Note)
		}
		u.status += fmt.Sprintf(" %d trashed notes still link to %q: %s.", len(res.Orphaned), res.From, strings.Join(names, ", "))
	}
	return nil
}

// --- rendering --------------------------------------------------------------

func (c *Compendium) render(version int) sdk.View {
	u := &c.ui
	var v sdk.View
	switch u.mode {
	case modeNote:
		if c.v.notes[fold(u.note)] == nil && !u.dirty && c.v.drafts[fold(u.note)] == nil {
			c.goHome()
			return c.render(version)
		}
		v = c.noteView(version)
	case modeGraph:
		v = c.graphView()
	case modeTags:
		v = c.tagsView()
	case modeTag:
		v = c.tagView()
	case modeTrash:
		v = c.trashView()
	case modeRecovery:
		v = c.recoveryView()
	case modeHelp:
		v = c.helpView()
	default:
		v = c.homeView()
	}
	v.State = sdk.ViewReady
	if u.status != "" {
		v.Status = u.status
	}
	v.Status, v.Error = detail(v.Status), detail(u.err)
	if len(v.Items) > maxItems {
		v.Items = v.Items[:maxItems]
	}
	return v
}

func (c *Compendium) noteSummary(n *note) string {
	parts := []string{fmt.Sprintf("→ %d links", len(n.links)), fmt.Sprintf("← %d backlinks", len(c.v.back[fold(n.title)]))}
	for i, t := range n.tags {
		if i == 6 {
			break
		}
		parts = append(parts, "#"+t)
	}
	return strings.Join(parts, " · ")
}

func (c *Compendium) homeView() sdk.View {
	u := &c.ui
	v := sdk.View{
		Title:  "Compendium",
		Fields: []sdk.Field{{ID: u.qField(), Label: "Search or command (today, new <title>, open <title>, graph, tags, trash, help)", Value: u.query}},
		Status: fmt.Sprintf("%d notes · %d tags · %d in trash · %d drafts", len(c.v.notes), len(c.v.tagIdx), len(c.v.trash), len(c.v.drafts)),
	}
	if w := c.v.stats.Warnings; len(w) > 0 {
		v.Status += fmt.Sprintf(" · %d storage notice(s): %s", len(w), w[0])
	}
	if u.searched {
		v.Title = "Compendium — Search"
		for _, r := range u.results {
			if len(v.Items) == maxItems-1 {
				break
			}
			v.Items = append(v.Items, sdk.Item{ID: hid("n", fold(r.Title)), Label: label(r.Title), Detail: detail(fmt.Sprintf("%s  (score %.2f)", r.Snippet, r.Score))})
		}
		if len(v.Items) == 0 {
			v.Items = append(v.Items, sdk.Item{ID: "noresults", Label: "No matches", Detail: detail(fmt.Sprintf("Nothing matches %q. Try fewer or shorter words, or `new <title>`.", u.query))})
		} else if u.total > len(v.Items) {
			v.Items = append(v.Items, sdk.Item{ID: "more", Label: fmt.Sprintf("… %d more matches", u.total-len(v.Items)), Detail: "Refine the query to narrow the results."})
		}
	} else {
		notes := c.v.sortedNotes()
		for _, n := range notes {
			if len(v.Items) == maxItems-1 {
				v.Items = append(v.Items, sdk.Item{ID: "more", Label: fmt.Sprintf("… %d more notes", len(notes)-len(v.Items)), Detail: "Search or use `open <title>` to reach the rest."})
				break
			}
			v.Items = append(v.Items, sdk.Item{ID: hid("n", fold(n.title)), Label: label(n.title), Detail: detail(c.noteSummary(n))})
		}
		if len(notes) == 0 {
			v.Items = append(v.Items, sdk.Item{ID: "empty", Label: "No notes yet", Detail: "Type `today` for a daily note or `new My First Note`, then Go."})
		}
	}
	v.Actions = []sdk.Action{
		{ID: "run", Label: "Go"}, {ID: "open", Label: "Open"}, {ID: "new", Label: "New note"},
		{ID: "today", Label: "Today"}, {ID: "graph", Label: "Graph"}, {ID: "tags", Label: "Tags"},
		{ID: "trash_bin", Label: "Trash"}, {ID: "recovery", Label: "Drafts", Disabled: len(c.v.drafts) == 0},
		{ID: "export_vault", Label: "Export vault", Disabled: len(c.v.notes) == 0},
		{ID: "rebuild_index", Label: "Rebuild index"}, {ID: "help", Label: "Help"}, {ID: "home", Label: "Refresh"},
	}
	return v
}

// regionText bounds a rendered document to the version-3 scroll region
// budgets: 2048 lines and 64 KiB. A cut is marked, never silent.
func regionText(s string) string {
	const maxRegionLines, maxRegionBytes = 2048, 65536
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) > maxRegionLines {
		s = strings.Join(lines[:maxRegionLines-1], "\n") + "\n…"
	}
	return clip(s, maxRegionBytes)
}

func (c *Compendium) noteView(version int) sdk.View {
	u := &c.ui
	n := c.v.notes[fold(u.note)]
	title := "Compendium — " + clip(u.note, 220)
	if u.dirty {
		title += "*"
	}
	rendered := render(u.buffer, c.v.resolve)
	if strings.TrimSpace(rendered) == "" {
		rendered = "(empty note)"
	}
	v := sdk.View{Title: title}
	if version >= 3 {
		// The rendered preview is long output: the version-3 scroll region
		// replaces the single item detail it used to be clipped into.
		v.Region = &sdk.Region{ID: "rendered", Label: "Rendered", Text: regionText(rendered)}
	} else {
		v.Items = append(v.Items, sdk.Item{ID: "rendered", Label: "Rendered", Detail: detail(rendered)})
	}
	p := parseNote(u.buffer)
	missing := 0
	for i, l := range p.Links {
		if i == 40 {
			v.Items = append(v.Items, sdk.Item{ID: "links_more", Label: fmt.Sprintf("… %d more links", len(p.Links)-40), Detail: "Use the open route for the full list."})
			break
		}
		if t, ok := c.v.resolve(l); ok {
			v.Items = append(v.Items, sdk.Item{ID: hid("l", fold(l)), Label: label("→ " + t), Detail: "Linked note. Follow opens it."})
		} else {
			missing++
			v.Items = append(v.Items, sdk.Item{ID: hid("l", fold(l)), Label: label("✗ " + l + " (missing)"), Detail: detail(fmt.Sprintf("Broken link: no note titled %q. Follow creates it and opens it.", l))})
		}
	}
	back := c.v.backlinks(u.note)
	for i, b := range back {
		if i == 40 {
			v.Items = append(v.Items, sdk.Item{ID: "backlinks_more", Label: fmt.Sprintf("… %d more backlinks", len(back)-40), Detail: "Use the open route for the full list."})
			break
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("b", fold(b.Title)), Label: label("← " + b.Title), Detail: detail(nonBlank(b.Context, "links here"))})
	}
	for i, t := range p.Tags {
		if i == 15 {
			break
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("t", t), Label: label("#" + t), Detail: fmt.Sprintf("%d notes", len(c.v.tagIdx[t]))})
	}
	if version >= 3 {
		if len(u.buffer) <= areaLimit {
			v.Fields = append(v.Fields, sdk.Field{ID: u.bodyField(), Label: "Markdown", Value: u.buffer, Multiline: true})
		}
	} else if len(u.buffer) <= fieldLimit {
		v.Fields = append(v.Fields, sdk.Field{ID: u.bodyField(), Label: "Markdown", Value: u.buffer})
	}
	v.Fields = append(v.Fields, sdk.Field{ID: u.renameField(), Label: "Rename to"})
	v.Actions = []sdk.Action{
		{ID: "save", Label: "Save"}, {ID: "follow", Label: "Follow / create link"},
		{ID: "reload", Label: "Reload saved version"},
		{ID: "rename", Label: "Rename"}, {ID: "trash", Label: "Move to trash", Disabled: n == nil},
		{ID: "export_note", Label: "Export", Disabled: n == nil}, {ID: "today", Label: "Today"},
		{ID: "home", Label: "Back"},
	}
	stale := n != nil && n.sha != u.baseSHA || n == nil && u.baseSHA != ""
	if u.conflict {
		v.Actions = append([]sdk.Action{{ID: "overwrite", Label: "Overwrite with mine"}}, v.Actions...)
	}
	switch {
	case u.conflict:
		v.Status = "Conflict: the note changed since you started editing. Overwrite saves your version; Reload shows the saved one (your edits stay in Drafts)."
	case version >= 3 && len(u.buffer) > areaLimit:
		v.Status = fmt.Sprintf("This note is %d bytes, larger than the %d-byte multiline field limit, so it is read-only here; edit it with the edit/save routes.", len(u.buffer), areaLimit)
	case version < 3 && len(u.buffer) > fieldLimit:
		v.Status = fmt.Sprintf("This note is %d bytes, larger than the %d-byte field limit, so it is read-only here; edit it with the edit/save routes.", len(u.buffer), fieldLimit)
	case u.dirty && stale:
		v.Status = "Unsaved changes, and the note was changed elsewhere meanwhile. Save will ask before overwriting."
	case u.dirty:
		v.Status = "Unsaved changes (staged as a recovery draft). Save to commit."
	case stale:
		v.Status = "This note was changed elsewhere. Reload (or Save) shows the saved version."
	default:
		v.Status = fmt.Sprintf("Saved · %d links (%d missing) · %d backlinks", len(p.Links), missing, len(back))
	}
	return v
}

func nonBlank(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func (c *Compendium) graphView() sdk.View {
	g := c.v.graph()
	outs, ins := map[string][]string{}, map[string][]string{}
	for _, e := range g.Edges {
		outs[e.From] = append(outs[e.From], e.To)
		ins[e.To] = append(ins[e.To], e.From)
	}
	v := sdk.View{Title: "Compendium — Graph"}
	v.Items = append(v.Items, sdk.Item{ID: "summary", Label: "Graph overview", Detail: fmt.Sprintf(
		"%d notes · %d links · %d dead ends · %d orphans · %d unresolved",
		len(g.Nodes), len(g.Edges), len(g.DeadEnds), len(g.Orphans), len(g.Unresolved))})
	nodes := append([]graphNode(nil), g.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].In+nodes[i].Out > nodes[j].In+nodes[j].Out })
	list := func(xs []string) string {
		if len(xs) > 8 {
			return strings.Join(xs[:8], ", ") + fmt.Sprintf(" +%d", len(xs)-8)
		}
		return strings.Join(xs, ", ")
	}
	for i, n := range nodes {
		if i == 80 {
			v.Items = append(v.Items, sdk.Item{ID: "nodes_more", Label: fmt.Sprintf("… %d more notes", len(nodes)-80), Detail: "The graph route returns every node and edge."})
			break
		}
		var parts []string
		switch {
		case n.In > 0 && n.Out == 0:
			parts = append(parts, "dead end")
		case n.In == 0 && n.Out == 0:
			parts = append(parts, "orphan")
		}
		if len(outs[n.Title]) > 0 {
			parts = append(parts, "→ "+list(outs[n.Title]))
		}
		if len(ins[n.Title]) > 0 {
			parts = append(parts, "← "+list(ins[n.Title]))
		}
		for _, t := range n.Tags {
			parts = append(parts, "#"+t)
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("n", fold(n.Title)), Label: label(fmt.Sprintf("%s  (in %d · out %d)", n.Title, n.In, n.Out)), Detail: detail(nonBlank(strings.Join(parts, " · "), "linked"))})
	}
	for i, un := range g.Unresolved {
		if i == 15 {
			v.Items = append(v.Items, sdk.Item{ID: "unresolved_more", Label: fmt.Sprintf("… %d more unresolved links", len(g.Unresolved)-15), Detail: "The graph route lists them all."})
			break
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("u", fold(un.Target)), Label: label("✗ " + un.Target + " (missing)"), Detail: detail("Linked from " + list(un.From) + " · Follow creates it.")})
	}
	v.Actions = []sdk.Action{{ID: "follow", Label: "Open / create"}, {ID: "home", Label: "Back"}}
	v.Status = "Nodes are notes; → outgoing and ← incoming links. Dead ends are linked to but link nowhere; orphans have no links either way."
	return v
}

func (c *Compendium) tagsView() sdk.View {
	v := sdk.View{Title: "Compendium — Tags"}
	tags := c.v.tags()
	for i, t := range tags {
		if i == maxItems-1 {
			v.Items = append(v.Items, sdk.Item{ID: "more", Label: fmt.Sprintf("… %d more tags", len(tags)-i), Detail: "Use `tag <name>`."})
			break
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("t", t.Tag), Label: label("#" + t.Tag), Detail: fmt.Sprintf("%d notes", t.Count)})
	}
	if len(tags) == 0 {
		v.Items = append(v.Items, sdk.Item{ID: "none", Label: "No tags yet", Detail: "Write #tag anywhere in a note."})
	}
	v.Actions = []sdk.Action{{ID: "open", Label: "Show notes"}, {ID: "home", Label: "Back"}}
	v.Status = fmt.Sprintf("%d tags", len(tags))
	return v
}

func (c *Compendium) tagView() sdk.View {
	v := sdk.View{Title: label("Compendium — #" + c.ui.tag)}
	notes := c.v.tagged(c.ui.tag)
	for i, t := range notes {
		if i == maxItems-1 {
			v.Items = append(v.Items, sdk.Item{ID: "more", Label: fmt.Sprintf("… %d more notes", len(notes)-i), Detail: "Use the tag route for the full list."})
			break
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("n", fold(t)), Label: label(t), Detail: detail(c.noteSummary(c.v.notes[fold(t)]))})
	}
	if len(notes) == 0 {
		v.Items = append(v.Items, sdk.Item{ID: "none", Label: "No notes", Detail: "Nothing is tagged #" + c.ui.tag + "."})
	}
	v.Actions = []sdk.Action{{ID: "open", Label: "Open"}, {ID: "tags", Label: "All tags"}, {ID: "home", Label: "Back"}}
	v.Status = fmt.Sprintf("%d notes tagged #%s", len(notes), c.ui.tag)
	return v
}

func (c *Compendium) trashView() sdk.View {
	v := sdk.View{Title: "Compendium — Trash"}
	for i := len(c.v.trash) - 1; i >= 0 && len(v.Items) < maxItems; i-- {
		t := c.v.trash[i]
		v.Items = append(v.Items, sdk.Item{ID: "x-" + t.ID, Label: label(t.Title), Detail: "Trashed " + trashTime(t.ID) + " · Restore puts it back with its links, tags, and history."})
	}
	if len(v.Items) == 0 {
		v.Items = append(v.Items, sdk.Item{ID: "empty", Label: "Trash is empty", Detail: "Trashed notes keep their history and can be restored."})
	}
	v.Actions = []sdk.Action{{ID: "restore", Label: "Restore", Disabled: len(c.v.trash) == 0}, {ID: "home", Label: "Back"}}
	v.Status = fmt.Sprintf("%d trashed notes", len(c.v.trash))
	return v
}

func (c *Compendium) recoveryView() sdk.View {
	v := sdk.View{Title: "Compendium — Recovery drafts"}
	for _, d := range c.draftList() {
		if len(v.Items) == maxItems {
			break
		}
		state := "edits to an existing note"
		if !d.Exists {
			state = "a note that was never saved"
		}
		v.Items = append(v.Items, sdk.Item{ID: hid("d", fold(d.Title)), Label: label(d.Title), Detail: detail(fmt.Sprintf("Unsaved %s, staged %s: %s", state, d.StagedAt, clip(flatten(d.Content), 600)))})
	}
	if len(v.Items) == 0 {
		v.Items = append(v.Items, sdk.Item{ID: "none", Label: "No drafts", Detail: "Every edit is committed."})
	}
	v.Actions = []sdk.Action{
		{ID: "restore_draft", Label: "Restore draft", Disabled: len(c.v.drafts) == 0},
		{ID: "discard_draft", Label: "Discard draft", Disabled: len(c.v.drafts) == 0},
		{ID: "home", Label: "Later"},
	}
	v.Status = "Unsaved edits from an earlier session were kept. Restore one to review and save it, or discard it."
	return v
}

func (c *Compendium) helpView() sdk.View {
	rows := [][2]string{
		{"Search", "Type words and Go: ranked results with snippets; words of two or more letters also match as prefixes."},
		{"today", "Opens or creates today's daily note (YYYY-MM-DD)."},
		{"new <title>", "Creates a note. Titles may not contain [ ] | # / or \\."},
		{"open <title>", "Opens a note by title (case-insensitive)."},
		{"[[Title]]", "Wikilinks; [[Title|alias]] and [[Title#heading]] work too. Broken links can be created with Follow."},
		{"#tag", "Tags are indexed; `tags` lists them and `tag <name>` shows a tag's notes."},
		{"graph", "Nodes, links, dead ends, orphans, and unresolved links as a list."},
		{"trash", "Trashed notes keep links, tags, and history and can be restored."},
		{"drafts", "Unsaved edits are staged as drafts and survive crashes."},
		{"Storage", "Plain Markdown in Compendium's private storage: " + vaultDir + ". Not encrypted at rest."},
	}
	v := sdk.View{Title: "Compendium — Help"}
	for i, r := range rows {
		v.Items = append(v.Items, sdk.Item{ID: fmt.Sprintf("h%d", i), Label: r[0], Detail: r[1]})
	}
	v.Actions = []sdk.Action{{ID: "home", Label: "Back"}}
	return v
}
