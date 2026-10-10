// Package markview is Gostalgia's read-only Markdown viewer: the open-with
// companion to Notes. It declares .md and .markdown document associations
// so Files open-with and doc/handoff can route documents to it with a
// scoped, session-bound read grant — the only filesystem access the app
// ever holds. The manifest asks for ipc and nothing else: no fs.read, no
// fs.write, no path grants, no events. Opening a path the grants do not
// cover is answered by the platform's ordinary capability denial.
//
// Document structure (headings, lists, quotes, fenced code, tables, links)
// renders into the presentation contract's item vocabulary — every row is
// a labelled item with structure and source line in the detail slot —
// never raw terminal markup. Nothing is editable and nothing is written
// back; paging through long documents is the only navigation.
package markview

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"gostalgia/sdk"
)

// ID is the canonical application identifier.
const ID = "com.gostalgia.markview"

const (
	// maxDocumentBytes bounds the fs/read window; larger files render
	// truncated with an explicit indicator row.
	maxDocumentBytes = 1 << 20
	// pageRows bounds items per page, leaving headroom under the contract's
	// 64-item budget for the document info row and bound indicators.
	pageRows = 56
	// maxRows bounds parsed rows held in memory; beyond it the document is
	// marked row-capped and the tail is reported rather than rendered.
	maxRows = 8192
	// paraWrapRunes is the wrap width for paragraph rows.
	paraWrapRunes = 110
	// maxTableColRunes bounds pipe-table column padding so one wide cell
	// cannot push every row toward the label bound.
	maxTableColRunes = 64
	// maxLabelBytes is the contract's label bound.
	maxLabelBytes = 256
)

//go:embed manifest.json
var manifestJSON []byte

// Manifest returns the parsed application manifest.
func Manifest() sdk.Manifest {
	m, err := sdk.ParseManifest(manifestJSON)
	if err != nil {
		panic(err)
	}
	return m
}

// ManifestJSON returns the raw embedded manifest bytes.
func ManifestJSON() []byte { return manifestJSON }

// Markview renders one handed-off Markdown document at a time as a paged
// item view. It keeps no filesystem grants of its own; every open and
// reload goes through fs/read under whatever scoped grant the handoff
// issued, and denial surfaces honestly in the view's error slot.
type Markview struct {
	app *sdk.Context
	mu  sync.Mutex

	path      string
	name      string
	modTime   string
	grantID   string
	size      int64 // bytes actually read
	totalSize int64 // full file size per fs/read
	lines     int   // source lines
	truncated bool  // file exceeds maxDocumentBytes
	rows      []row
	rowCapped bool // parsed rows exceeded maxRows

	page       int
	statusNote string
	lastError  string
}

// Factory creates a new Markview instance.
func Factory() (sdk.Instance, error) { return &Markview{}, nil }

// Init registers the app's routes and presentation contract.
func (m *Markview) Init(app *sdk.Context) error {
	m.app = app
	for _, r := range []struct {
		name string
		h    sdk.Handler
	}{
		{"open", m.openRoute},
		{"doc", m.docRoute},
		{"reload", m.reloadRoute},
	} {
		if err := app.Handle(r.name, r.h); err != nil {
			return err
		}
	}
	return app.Present(m.view, m.act)
}

// Run blocks until the instance is stopped.
func (m *Markview) Run(ctx context.Context) error {
	m.app.Log.Info("markview running")
	<-ctx.Done()
	return nil
}

// Stop releases the app.
func (m *Markview) Stop(context.Context) error { return nil }

// ---- routes --------------------------------------------------------------

type openParams struct {
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	GrantID string `json:"grant_id"`
	Force   bool   `json:"force"`
}

// openRoute is the document handoff entry point; it is also reachable
// directly. Access is entirely grant-mediated: without a scoped grant for
// the path, fs/stat and fs/read inside openLocked are denied.
func (m *Markview) openRoute(ctx context.Context, raw json.RawMessage) (any, error) {
	var p openParams
	if err := sdk.DecodeParams(raw, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Path) == "" {
		return nil, fmt.Errorf("params.path is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.openLocked(ctx, p.Path, p.GrantID); err != nil {
		m.lastError = err.Error()
		return nil, err
	}
	return m.docInfo(), nil
}

// docRoute reports the currently open document and its render shape.
func (m *Markview) docRoute(_ context.Context, _ json.RawMessage) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.docInfo(), nil
}

// reloadRoute re-reads the open document under the still-held grant.
func (m *Markview) reloadRoute(ctx context.Context, _ json.RawMessage) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path == "" {
		return nil, fmt.Errorf("no document to reload")
	}
	if err := m.reloadLocked(ctx); err != nil {
		m.lastError = err.Error()
		return nil, err
	}
	return m.docInfo(), nil
}

func (m *Markview) docInfo() map[string]any {
	if m.path == "" {
		return map[string]any{"path": "", "document": false}
	}
	return map[string]any{
		"path":       m.path,
		"name":       m.name,
		"document":   true,
		"size":       m.size,
		"total_size": m.totalSize,
		"lines":      m.lines,
		"rows":       len(m.rows),
		"row_capped": m.rowCapped,
		"truncated":  m.truncated,
		"page":       m.page + 1,
		"pages":      m.pageCount(),
		"grant_id":   m.grantID,
		"modified":   m.modTime,
	}
}

// ---- document ------------------------------------------------------------

// openLocked reads pth under the app's current grants and renders it into
// paged rows. Extension, directory, binary, and size checks reject
// honestly; grant denial propagates from the fs calls untouched.
func (m *Markview) openLocked(ctx context.Context, pth, grantID string) error {
	ext := strings.ToLower(path.Ext(pth))
	if ext != "" && ext != ".md" && ext != ".markdown" {
		return fmt.Errorf("not a Markdown document: %s", pth)
	}
	var st struct {
		Name    string `json:"name"`
		IsDir   bool   `json:"is_dir"`
		ModTime string `json:"mod_time"`
	}
	if err := m.app.Call(ctx, "fs/stat", map[string]string{"path": pth}, &st); err != nil {
		return fmt.Errorf("open %s: %w", pth, err)
	}
	if st.IsDir {
		return fmt.Errorf("open %s: path is a directory", pth)
	}
	var rd struct {
		DataBase64 string `json:"data_base64"`
		TotalSize  int64  `json:"total_size"`
	}
	if err := m.app.Call(ctx, "fs/read", map[string]any{
		"path":   pth,
		"offset": 0,
		"limit":  maxDocumentBytes,
	}, &rd); err != nil {
		return fmt.Errorf("open %s: %w", pth, err)
	}
	data, err := base64.StdEncoding.DecodeString(rd.DataBase64)
	if err != nil {
		return fmt.Errorf("open %s: %w", pth, err)
	}
	if !looksText(data) {
		return fmt.Errorf("open %s: not a Markdown document (binary content)", pth)
	}
	src := string(data)
	rows, capped := renderMarkdown(src, maxRows)
	lines := strings.Count(src, "\n")
	if len(src) > 0 && !strings.HasSuffix(src, "\n") {
		lines++
	}

	m.path, m.name = pth, st.Name
	m.modTime, m.grantID = st.ModTime, grantID
	m.size, m.totalSize = int64(len(data)), rd.TotalSize
	m.lines = lines
	m.truncated = rd.TotalSize > int64(len(data))
	m.rows, m.rowCapped = rows, capped
	m.page = 0
	m.statusNote = "Opened " + st.Name
	m.lastError = ""
	return nil
}

// reloadLocked re-opens the current path, preserving the page if it still
// exists.
func (m *Markview) reloadLocked(ctx context.Context) error {
	pth, page, grant := m.path, m.page, m.grantID
	if err := m.openLocked(ctx, pth, grant); err != nil {
		return err
	}
	if page < m.pageCount() {
		m.page = page
	}
	m.statusNote = "Reloaded " + m.name
	return nil
}

// looksText reports whether data plausibly contains Markdown text rather
// than binary content: valid UTF-8, no NUL bytes, and few control
// characters other than newline, tab, and carriage return.
func looksText(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	ctrl := 0
	for _, b := range data {
		if b == 0 {
			return false
		}
		if b < 0x20 && b != '\n' && b != '\t' && b != '\r' {
			ctrl++
		}
	}
	return ctrl <= len(data)/10
}

func (m *Markview) pageCount() int {
	pages := (len(m.rows) + pageRows - 1) / pageRows
	if pages < 1 {
		return 1
	}
	return pages
}

// ---- presentation --------------------------------------------------------

func (m *Markview) view(ctx context.Context) (sdk.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viewLocked(), nil
}

func (m *Markview) viewLocked() sdk.View {
	v := sdk.View{
		Title: "Markview",
		State: sdk.ViewReady,
		Fields: []sdk.Field{
			{ID: "open_path", Label: "Open path"},
		},
		Error: m.lastError,
	}
	if m.path == "" {
		v.Items = []sdk.Item{
			{ID: "about", Label: "Markdown viewer (read-only)", Detail: "renders .md and .markdown documents as structured items"},
			{ID: "access", Label: "Grant-gated access", Detail: "no standing fs grant — open via Files open-with or doc/handoff; a scoped read grant travels with the document"},
		}
		v.Actions = []sdk.Action{
			{ID: "open", Label: "Open path"},
			{ID: "reload", Label: "Reload", Disabled: true},
			{ID: "prev_page", Label: "Prev page", Disabled: true},
			{ID: "next_page", Label: "Next page", Disabled: true},
		}
		v.Status = "no document — hand one off via Files open-with or grant a path and use Open"
		return v
	}

	v.Title = "Markview — " + m.name
	pages := m.pageCount()
	start := m.page * pageRows
	end := start + pageRows
	if end > len(m.rows) {
		end = len(m.rows)
	}
	items := make([]sdk.Item, 0, end-start+4)
	items = append(items, sdk.Item{
		ID:    "docinfo",
		Label: m.name,
		Detail: fmt.Sprintf("%s · %s · %d lines · modified %s",
			m.path, formatSize(m.totalSize), m.lines, nonEmpty(m.modTime, "unknown")),
	})
	for i := start; i < end; i++ {
		r := m.rows[i]
		items = append(items, sdk.Item{
			ID:     fmt.Sprintf("r%d", i),
			Label:  r.label,
			Detail: r.detail(),
		})
	}
	if len(m.rows) == 0 {
		items = append(items, sdk.Item{ID: "empty", Label: "(empty document)", Detail: m.path})
	}
	if end < len(m.rows) {
		items = append(items, sdk.Item{
			ID:     "more",
			Label:  fmt.Sprintf("… and %d more rows", len(m.rows)-end),
			Detail: "the Next page action continues the document",
		})
	}
	if m.rowCapped {
		items = append(items, sdk.Item{
			ID:     "rowcap",
			Label:  "… document exceeds the row cap",
			Detail: fmt.Sprintf("only the first %d rendered rows are held", maxRows),
		})
	}
	if m.truncated {
		items = append(items, sdk.Item{
			ID:     "truncated",
			Label:  "… document truncated",
			Detail: fmt.Sprintf("showing the first %s of %s", formatSize(maxDocumentBytes), formatSize(m.totalSize)),
		})
	}
	v.Items = items
	v.Actions = []sdk.Action{
		{ID: "open", Label: "Open path"},
		{ID: "reload", Label: "Reload"},
		{ID: "prev_page", Label: "Prev page", Disabled: m.page == 0},
		{ID: "next_page", Label: "Next page", Disabled: m.page >= pages-1},
	}
	v.Status = fmt.Sprintf("page %d/%d · %d rows · %s · read-only", m.page+1, pages, len(m.rows), formatSize(m.size))
	if m.statusNote != "" {
		v.Status = m.statusNote + " — " + v.Status
	}
	return v
}

func (m *Markview) act(ctx context.Context, p sdk.ActionRequest) (sdk.View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var err error
	switch p.Action {
	case "open":
		target := strings.TrimSpace(p.Values["open_path"])
		if target == "" {
			m.lastError = "open: a path is required"
			break
		}
		err = m.openLocked(ctx, target, "")
	case "reload":
		if m.path == "" {
			err = fmt.Errorf("no document to reload")
		} else {
			err = m.reloadLocked(ctx)
		}
	case "next_page":
		if m.page < m.pageCount()-1 {
			m.page++
		}
	case "prev_page":
		if m.page > 0 {
			m.page--
		}
	default:
		return sdk.View{}, fmt.Errorf("unknown action %q", p.Action)
	}
	if err != nil {
		m.lastError = err.Error()
	}
	return m.viewLocked(), nil
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// formatSize renders byte counts the way the other builtins do.
func formatSize(b int64) string {
	if b < 0 {
		return "0 B"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}
