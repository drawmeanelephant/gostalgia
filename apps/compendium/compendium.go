// Package compendium is Gostalgia's local-first personal knowledge base:
// Markdown notes with [[wikilinks]], backlinks, #tags, ranked full-text
// search, and a note graph published as presentation data.
//
// Notes live as plain Markdown files in Compendium's app-private partition
// (/apps/data/com.gostalgia.compendium/vault). The manifest asks for ipc and
// nothing else: no fs.read/fs.write, no path grants. Import from or export
// to a user-visible folder works only where an operator has granted
// Compendium that path (fs/grant or a document handoff).
package compendium

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gostalgia/sdk"
)

// ID is the reverse-DNS identifier for Compendium.
const ID = "com.gostalgia.compendium"

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the validated manifest declaration.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err) // invalid builtin manifest is a programming error
	}
	return m
}

// Factory constructs a fresh, uninitialized Compendium instance.
func Factory() (sdk.Instance, error) { return &Compendium{now: time.Now}, nil }

// Compendium is one running instance. All state is guarded by mu; every
// route and presentation callback takes it, so operations are serialized.
type Compendium struct {
	app *sdk.Context
	now func() time.Time

	mu      sync.Mutex
	v       *vault
	loaded  bool
	loadErr string
	ui      uiState
}

// setClock injects a deterministic clock (tests and daily notes).
func setClock(c *Compendium, now func() time.Time) { c.now = now }

// Init registers routes and the presentation. Storage is loaded lazily on
// first use so Init stays prompt.
func (c *Compendium) Init(app *sdk.Context) error {
	c.app = app
	c.ui = uiState{mode: modeHome, rev: 1}
	routes := map[string]func(context.Context, json.RawMessage) (any, error){
		"create": c.createRoute, "open": c.openRoute, "save": c.saveRoute, "edit": c.editRoute,
		"rename": c.renameRoute, "trash": c.trashRoute, "trash_list": c.trashListRoute,
		"restore": c.restoreRoute, "history": c.historyRoute, "search": c.searchRoute,
		"graph": c.graphRoute, "tags": c.tagsRoute, "tag": c.tagRoute, "today": c.todayRoute,
		"list": c.listRoute, "export": c.exportRoute, "import": c.importRoute,
		"drafts": c.draftsRoute, "recover": c.recoverRoute, "rebuild": c.rebuildRoute,
		"status": c.statusRoute,
	}
	for name, h := range routes {
		h := h
		if err := app.Handle(name, func(ctx context.Context, raw json.RawMessage) (any, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if err := c.ensureLoadedLocked(ctx); err != nil {
				return nil, err
			}
			c.settlePendingLocked(ctx)
			return h(ctx, raw)
		}); err != nil {
			return err
		}
	}
	return app.Present(c.view, c.act)
}

// Run blocks until canceled; there is no background work to join.
func (c *Compendium) Run(ctx context.Context) error {
	c.app.Log.Info("compendium running")
	<-ctx.Done()
	return nil
}

// Stop has nothing to flush: every edit is staged as a draft and every
// commit is durable before its call returns.
func (c *Compendium) Stop(context.Context) error { return nil }

func (c *Compendium) ensureLoadedLocked(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	if c.v == nil {
		c.v = newVault(store{call: c.app.Call}, c.now)
	}
	if err := c.v.load(ctx, false); err != nil {
		c.loadErr = err.Error()
		return fmt.Errorf("compendium: open vault: %w", err)
	}
	c.loaded, c.loadErr = true, ""
	if c.app.Log != nil {
		c.app.Log.Info("compendium vault loaded", "notes", len(c.v.notes),
			"reparsed", c.v.stats.Reparsed, "journal_replayed", c.v.stats.JournalReplayed)
	}
	return nil
}

// settlePendingLocked finishes an operation left pending by an earlier
// failure before a route or action looks anything up, because finishing it
// reloads memory from storage: a note or draft pointer taken before the
// reload would describe the old state (a draft the interrupted trash
// already moved away, a draft the interrupted rename already rewrote).
// When it still cannot finish, memory is unchanged and mutating calls
// report the pending operation themselves.
func (c *Compendium) settlePendingLocked(ctx context.Context) {
	if c.v != nil && c.v.pending {
		_ = c.v.ready(ctx)
	}
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	return sdk.DecodeParams(raw, out)
}

type titleParams struct {
	Title   string `json:"title"`
	Content string `json:"content"`
}

type openParams struct {
	titleParams
	IncludeContent  *bool `json:"include_content"`
	LinksOffset     int   `json:"links_offset"`
	BacklinksOffset int   `json:"backlinks_offset"`
	TagsOffset      int   `json:"tags_offset"`
}

func (c *Compendium) noteDoc(n *note, p openParams) (map[string]any, error) {
	links, backlinks, tags := c.v.linksOf(n.content), c.v.backlinks(n.title), nonNil(n.tags)
	for _, page := range []struct {
		name          string
		offset, total int
	}{{"links", p.LinksOffset, len(links)}, {"backlinks", p.BacklinksOffset, len(backlinks)}, {"tags", p.TagsOffset, len(tags)}} {
		if page.offset < 0 || page.offset > page.total {
			return nil, fmt.Errorf("params.%s_offset must be between 0 and %d", page.name, page.total)
		}
	}
	doc := map[string]any{
		"title": n.title, "size": len(n.content), "sha256": n.sha,
		"draft": c.v.drafts[fold(n.title)] != nil,
	}
	if p.IncludeContent == nil || *p.IncludeContent {
		// Reserve room for metadata even when note text consumes its budget.
		budget := putText(doc, "content", n.content, maxTextResponseBytes-(1<<20))
		rendered := render(n.content, c.v.resolve)
		if jsonTextLen(rendered) <= budget {
			doc["rendered"] = rendered
		} else {
			doc["rendered"], doc["rendered_omitted"] = "", true
		}
	} else {
		doc["content_omitted"] = true
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Leave room for collection keys, counts, cursors and the IPC envelope.
	budget := maxTextResponseBytes - len(encoded) - 1024
	notePage(doc, "links", links, p.LinksOffset, &budget)
	notePage(doc, "backlinks", backlinks, p.BacklinksOffset, &budget)
	notePage(doc, "tags", tags, p.TagsOffset, &budget)
	return doc, nil
}

func notePage[T any](doc map[string]any, name string, items []T, offset int, budget *int) {
	page := []T{}
	omitted := 0
	next := offset
	for next < len(items) {
		data, _ := json.Marshal(items[next])
		size := len(data) + 1
		if size > maxTextResponseBytes-1024 {
			// Malformed link targets can be as large as the entire note.
			// Their exact bytes remain available in the source content.
			omitted++
			next++
			continue
		}
		if size > *budget {
			break
		}
		page = append(page, items[next])
		*budget -= size
		next++
	}
	doc[name], doc[name+"_total"] = page, len(items)
	if next < len(items) {
		doc[name+"_next_offset"] = next
	}
	if omitted > 0 {
		doc[name+"_omitted_items"] = omitted
	}
}

// maxTextResponseBytes bounds the note text one response carries once
// JSON-encoded. The IPC frame is 6 MiB and encoding/json escapes <, >, &
// and control bytes as six bytes each, so a 1 MiB note can encode to 6 MiB.
const maxTextResponseBytes = 3 << 20

// putText stores text under key when its JSON encoding fits budget, and as
// base64 under key+"_base64" (at most 4/3 of its size, under 1.4 MiB for
// any note) when it does not. It returns the budget left.
func putText(doc map[string]any, key, text string, budget int) int {
	if n := jsonTextLen(text); n <= budget {
		doc[key] = text
		return budget - n
	}
	doc[key], doc[key+"_base64"] = "", base64.StdEncoding.EncodeToString([]byte(text))
	return budget - base64.StdEncoding.EncodedLen(len(text))
}

// jsonTextLen is the length of s as encoding/json writes it in a string.
func jsonTextLen(s string) int {
	n := 2
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			switch {
			case b == '"' || b == '\\' || b == '\n' || b == '\r' || b == '\t':
				n += 2
			case b < 0x20 || b == '<' || b == '>' || b == '&':
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 || r == '\u2028' || r == '\u2029' {
			n += 6
		} else {
			n += size
		}
		i += size
	}
	return n
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (c *Compendium) createRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p titleParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	n, err := c.v.create(ctx, p.Title, p.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"title": n.title, "created": true, "size": len(n.content)}, nil
}

func (c *Compendium) openRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p openParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	n, err := c.v.get(p.Title)
	if err != nil {
		return nil, err
	}
	return c.noteDoc(n, p)
}

func (c *Compendium) saveRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title   string  `json:"title"`
		Content *string `json:"content"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Content == nil {
		return nil, fmt.Errorf("params.content is required")
	}
	n, err := c.v.save(ctx, p.Title, *p.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"saved": true, "title": n.title, "size": len(n.content), "sha256": n.sha}, nil
}

func (c *Compendium) editRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p titleParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if err := validTitle(p.Title); err != nil {
		return nil, err
	}
	if err := c.v.stageDraft(ctx, p.Title, p.Content); err != nil {
		return nil, err
	}
	return map[string]any{"staged": c.v.drafts[fold(p.Title)] != nil, "title": p.Title}, nil
}

func (c *Compendium) renameRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title    string `json:"title"`
		NewTitle string `json:"new_title"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	res, err := c.v.rename(ctx, p.Title, p.NewTitle)
	if err != nil {
		return nil, err
	}
	if fold(c.ui.note) == fold(res.From) {
		c.ui.note = res.To
	}
	return res, nil
}

func (c *Compendium) trashRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p titleParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	id, err := c.v.trashNote(ctx, p.Title)
	if err != nil {
		return nil, err
	}
	return map[string]any{"trashed": true, "trash_id": id}, nil
}

type trashItem struct {
	TrashID   string `json:"trash_id"`
	Title     string `json:"title"`
	TrashedAt string `json:"trashed_at"`
}

func trashTime(id string) string {
	ns, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return ""
	}
	return time.Unix(0, ns).UTC().Format(time.RFC3339)
}

func (c *Compendium) trashListRoute(context.Context, json.RawMessage) (any, error) {
	items := []trashItem{}
	for _, t := range c.v.trash {
		items = append(items, trashItem{t.ID, t.Title, trashTime(t.ID)})
	}
	return map[string]any{"items": items}, nil
}

func (c *Compendium) restoreRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		TrashID string `json:"trash_id"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	n, err := c.v.restoreNote(ctx, p.TrashID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"restored": true, "title": n.title}, nil
}

func (c *Compendium) historyRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p titleParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	n, err := c.v.get(p.Title)
	if err != nil {
		return nil, err
	}
	ids, err := c.v.historyIDs(ctx, historyDir+"/"+n.stem)
	if err != nil {
		return nil, err
	}
	type version struct {
		ID      string `json:"id"`
		SavedAt string `json:"saved_at"`
		Size    int    `json:"size"`
		SHA256  string `json:"sha256"`
	}
	out := []version{}
	for _, id := range ids {
		data, err := c.v.st.read(ctx, historyDir+"/"+n.stem+"/"+id+".md")
		if err != nil {
			return nil, err
		}
		out = append(out, version{id, trashTime(id), len(data), digest(string(data))})
	}
	return map[string]any{"title": n.title, "versions": out}, nil
}

func (c *Compendium) searchRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Limit <= 0 || p.Limit > 100 {
		p.Limit = 20
	}
	start := time.Now()
	results, total := c.v.find(p.Query, p.Limit)
	took := float64(time.Since(start).Microseconds()) / 1000
	return map[string]any{"query": p.Query, "total": total, "took_ms": took, "results": results}, nil
}

func (c *Compendium) graphRoute(context.Context, json.RawMessage) (any, error) {
	return c.v.graph(), nil
}

func (c *Compendium) tagsRoute(context.Context, json.RawMessage) (any, error) {
	return map[string]any{"tags": c.v.tags()}, nil
}

func (c *Compendium) tagRoute(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Tag string `json:"tag"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	tag := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(p.Tag), "#"))
	return map[string]any{"tag": tag, "notes": c.v.tagged(tag)}, nil
}

func (c *Compendium) todayRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	n, created, err := c.v.today(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"title": n.title, "created": created}, nil
}

func (c *Compendium) listRoute(context.Context, json.RawMessage) (any, error) {
	type row struct {
		Title     string   `json:"title"`
		Size      int      `json:"size"`
		Links     int      `json:"links"`
		Backlinks int      `json:"backlinks"`
		Tags      []string `json:"tags"`
	}
	out := []row{}
	for _, n := range c.v.sortedNotes() {
		out = append(out, row{n.title, len(n.content), len(n.links), len(c.v.back[fold(n.title)]), nonNil(n.tags)})
	}
	return map[string]any{"notes": out}, nil
}

func (c *Compendium) exportRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title string `json:"title"`
		Dest  string `json:"dest"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Title != "" {
		return c.v.exportNote(ctx, p.Title, p.Dest)
	}
	return c.v.exportVault(ctx, p.Dest)
}

func (c *Compendium) importRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title     string  `json:"title"`
		Content   *string `json:"content"`
		ZipBase64 string  `json:"zip_base64"`
		Path      string  `json:"path"`
		Overwrite bool    `json:"overwrite"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	var files []importFile
	var skipped []importSkip
	switch {
	case p.Path != "":
		// Read with Compendium's own grant: works only for paths an
		// operator granted it (fs/grant or doc/handoff).
		data, err := c.v.st.read(ctx, p.Path)
		if err != nil {
			return nil, fmt.Errorf("import %s: %w", p.Path, err)
		}
		if strings.HasSuffix(strings.ToLower(p.Path), ".zip") {
			if files, skipped, err = readZip(data); err != nil {
				return nil, err
			}
		} else {
			title := p.Title
			if title == "" {
				title = titleFromStem(strings.TrimSuffix(path.Base(p.Path), ".md"))
			}
			files = []importFile{{name: p.Path, title: title, data: data}}
		}
	case p.ZipBase64 != "":
		data, err := base64.StdEncoding.DecodeString(p.ZipBase64)
		if err != nil {
			return nil, fmt.Errorf("import: zip_base64: %w", err)
		}
		if files, skipped, err = readZip(data); err != nil {
			return nil, err
		}
	case p.Content != nil:
		files = []importFile{{name: p.Title, title: p.Title, data: []byte(*p.Content)}}
	default:
		return nil, fmt.Errorf("import needs path, zip_base64, or title+content")
	}
	res, err := c.v.importNotes(ctx, files, p.Overwrite)
	res.Skipped = append(res.Skipped, skipped...)
	return res, err
}

type draftInfo struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	StagedAt string `json:"staged_at"`
	Size     int    `json:"size"`
	Exists   bool   `json:"note_exists"`
	Omitted  bool   `json:"content_omitted,omitempty"`
	Base64   string `json:"content_base64,omitempty"`
}

func (c *Compendium) draftList() []draftInfo {
	out := []draftInfo{}
	for _, d := range c.v.drafts {
		out = append(out, draftInfo{Title: d.Title, Content: d.Content, StagedAt: d.StagedAt.UTC().Format(time.RFC3339), Size: len(d.Content), Exists: c.v.notes[fold(d.Title)] != nil})
	}
	sortBy(out, func(d draftInfo) string { return fold(d.Title) })
	return out
}

// draftsRoute lists drafts with their content while the encoded text fits
// maxTextResponseBytes; drafts past the budget are listed without content
// (content_omitted) and fetched one at a time with {"title": ...}.
func (c *Compendium) draftsRoute(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title string `json:"title"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	list := c.draftList()
	if p.Title != "" {
		d := c.v.drafts[fold(strings.TrimSpace(p.Title))]
		if d == nil {
			return nil, fmt.Errorf("no draft for %q", p.Title)
		}
		for _, x := range list {
			if fold(x.Title) == fold(d.Title) {
				if jsonTextLen(x.Content) > maxTextResponseBytes {
					x.Base64 = base64.StdEncoding.EncodeToString([]byte(x.Content))
					x.Content, x.Omitted = "", true
				}
				return map[string]any{"drafts": []draftInfo{x}}, nil
			}
		}
	}
	budget := maxTextResponseBytes
	for i := range list {
		if n := jsonTextLen(list[i].Content); n <= budget {
			budget -= n
			continue
		}
		list[i].Content, list[i].Omitted = "", true
	}
	return map[string]any{"drafts": list}, nil
}

// recoverRoute commits (restore) or discards a staged draft.
func (c *Compendium) recoverRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Title   string `json:"title"`
		Restore bool   `json:"restore"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	d := c.v.drafts[fold(p.Title)]
	if d == nil {
		return nil, fmt.Errorf("no draft for %q", p.Title)
	}
	if !p.Restore {
		if err := c.v.clearDraft(ctx, d.Title); err != nil {
			return nil, err
		}
		return map[string]any{"discarded": true, "title": d.Title}, nil
	}
	n, err := c.v.save(ctx, d.Title, d.Content)
	if err != nil {
		return nil, err
	}
	return map[string]any{"restored": true, "title": n.title, "size": len(n.content)}, nil
}

func (c *Compendium) rebuildRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	if err := c.v.load(ctx, true); err != nil {
		c.loaded = false
		return nil, err
	}
	return map[string]any{"notes": len(c.v.notes), "reparsed": c.v.stats.Reparsed}, nil
}

func (c *Compendium) statusRoute(context.Context, json.RawMessage) (any, error) {
	links := 0
	for _, n := range c.v.notes {
		links += len(n.links)
	}
	return map[string]any{
		"notes": len(c.v.notes), "links": links, "tags": len(c.v.tagIdx),
		"drafts": len(c.v.drafts), "trash": len(c.v.trash), "index": c.v.stats,
		"storage": rootDir,
	}, nil
}

func sortBy[T any](s []T, key func(T) string) {
	sort.Slice(s, func(i, j int) bool { return key(s[i]) < key(s[j]) })
}
