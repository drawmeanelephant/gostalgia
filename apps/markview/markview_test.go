package markview

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gostalgia/sdk"
)

// mockFS models the slice of the environment VFS surface Markview touches:
// fs/stat and fs/read. Both are gated on an explicit read grant for the
// path — the same decision GrantStore.FindMatchingGrant makes for an app
// principal, so a missing grant is a permission error, not a missing file.
type mockFS struct {
	mu     sync.Mutex
	files  map[string][]byte
	dirs   map[string]bool
	grants map[string]bool
}

func newMockFS() *mockFS {
	return &mockFS{files: map[string][]byte{}, dirs: map[string]bool{"/": true}, grants: map[string]bool{}}
}

func (m *mockFS) put(p string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for d := path.Dir(p); d != "." && d != ""; d = path.Dir(d) {
		m.dirs[d] = true
		if d == "/" {
			break
		}
	}
	m.files[p] = append([]byte{}, data...)
}

func (m *mockFS) mkdirAll(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ; p != "." && p != ""; p = path.Dir(p) {
		m.dirs[p] = true
		if p == "/" {
			break
		}
	}
}

func (m *mockFS) grant(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[p] = true
}

func (m *mockFS) revoke(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.grants, p)
}

func (m *mockFS) call(_ context.Context, method string, params, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		Path   string `json:"path"`
		Offset int64  `json:"offset"`
		Limit  int64  `json:"limit"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	if !m.grants[p.Path] {
		return fmt.Errorf("permission denied: path %q is outside app-private storage and has no grant for app %q", p.Path, ID)
	}
	switch method {
	case "fs/stat":
		if m.dirs[p.Path] && m.files[p.Path] == nil {
			return reply(map[string]any{"name": path.Base(p.Path), "is_dir": true, "mod_time": "2026-10-06T09:30:00Z"})
		}
		data, ok := m.files[p.Path]
		if !ok {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		return reply(map[string]any{
			"name": path.Base(p.Path), "is_dir": false,
			"size": len(data), "mod_time": "2026-10-06T09:30:00Z",
		})
	case "fs/read":
		data, ok := m.files[p.Path]
		if !ok {
			if m.dirs[p.Path] {
				return fmt.Errorf("read %s: path is a directory", p.Path)
			}
			return fmt.Errorf("not_found: %s", p.Path)
		}
		total := int64(len(data))
		if p.Offset > 0 {
			if p.Offset >= total {
				data = nil
			} else {
				data = data[p.Offset:]
			}
		}
		if p.Limit > 0 && int64(len(data)) > p.Limit {
			data = data[:p.Limit]
		}
		return reply(map[string]any{
			"path": p.Path, "offset": p.Offset, "limit": p.Limit,
			"size": len(data), "total_size": total,
			"data_base64": base64.StdEncoding.EncodeToString(data),
		})
	}
	return fmt.Errorf("mockfs: unsupported method %s", method)
}

// harness drives one Markview instance through its IPC routes exactly as a
// remote caller would: JSON params in, JSON results out.
type harness struct {
	fs       *mockFS
	app      *Markview
	handlers map[string]sdk.Handler
	seq      atomic.Uint64
}

func openHarness(t testing.TB, fs *mockFS) *harness {
	t.Helper()
	inst, err := Factory()
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{fs: fs, handlers: map[string]sdk.Handler{}}
	h.app = inst.(*Markview)
	ctx := sdk.NewContext(Manifest(), slog.New(slog.NewTextHandler(io.Discard, nil)), fs.call,
		func(name string, hd sdk.Handler) error { h.handlers[name] = hd; return nil })
	if err := h.app.Init(ctx); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) call(route string, params, out any) error {
	hd, ok := h.handlers[route]
	if !ok {
		return fmt.Errorf("route %q not registered", route)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	res, err := hd(context.Background(), raw)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (h *harness) must(t testing.TB, route string, params, out any) {
	t.Helper()
	if err := h.call(route, params, out); err != nil {
		t.Fatalf("%s: %v", route, err)
	}
}

// viewAt fetches and validates the view at a negotiated protocol version.
func (h *harness) viewAt(t testing.TB, version int) sdk.View {
	t.Helper()
	raw, _ := json.Marshal(map[string]int{"version": version})
	res, err := h.handlers["view"](context.Background(), raw)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view does not validate: %v", err)
	}
	return v
}

func (h *harness) view(t testing.TB) sdk.View { return h.viewAt(t, 1) }

func (h *harness) act(t testing.TB, action string, values map[string]string) sdk.View {
	t.Helper()
	return h.actAt(t, 1, action, values)
}

func (h *harness) actAt(t testing.TB, version int, action string, values map[string]string) sdk.View {
	t.Helper()
	cur := h.viewAt(t, version)
	req := sdk.ActionRequest{
		Version: version, Instance: cur.Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)), Action: action, Values: values,
	}
	raw, _ := json.Marshal(req)
	res, err := h.handlers["action"](context.Background(), raw)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view does not validate: %v", err)
	}
	return v
}

func itemLabels(v sdk.View) []string {
	var out []string
	for _, it := range v.Items {
		out = append(out, it.Label)
	}
	return out
}

func hasItem(v sdk.View, id string) bool {
	for _, it := range v.Items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func itemDetail(v sdk.View, id string) string {
	for _, it := range v.Items {
		if it.ID == id {
			return it.Detail
		}
	}
	return ""
}

// ---- manifest & access model ---------------------------------------------

func TestManifestDeclaresOnlyIPCAndMarkdownTypes(t *testing.T) {
	m := Manifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("manifest invalid: %v", err)
	}
	if m.ID != ID || m.Entrypoint != "markview" {
		t.Fatalf("manifest identity = %+v", m)
	}
	for _, p := range m.Permissions {
		if p == "fs.read" || p == "fs.write" {
			t.Fatalf("markview must not hold standing fs permission %q — handoff grants are the access model", p)
		}
	}
	if len(m.Permissions) != 1 || m.Permissions[0] != "ipc" {
		t.Fatalf("permissions = %v, want [ipc]", m.Permissions)
	}
	if len(m.DocumentTypes) != 2 || m.DocumentTypes[0] != ".md" || m.DocumentTypes[1] != ".markdown" {
		t.Fatalf("document_types = %v", m.DocumentTypes)
	}
}

// ---- opening & denial ------------------------------------------------------

func TestViewWithoutDocument(t *testing.T) {
	h := openHarness(t, newMockFS())
	v := h.view(t)
	if v.State != sdk.ViewReady || v.Title != "Markview" {
		t.Fatalf("view = %+v", v)
	}
	if !hasItem(v, "access") {
		t.Fatalf("empty view should explain the grant access model, items = %v", itemLabels(v))
	}
	for _, a := range v.Actions {
		if a.ID == "reload" || a.ID == "next_page" || a.ID == "prev_page" {
			if !a.Disabled {
				t.Fatalf("action %s should be disabled without a document", a.ID)
			}
		}
	}
	if v2 := h.viewAt(t, 2); v2.State != sdk.ViewReady {
		t.Fatalf("v2 view = %+v", v2)
	}
}

func TestOpenDeniedWithoutGrant(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	fs.put(doc, []byte("# Guide\n"))
	h := openHarness(t, fs)

	var out map[string]any
	err := h.call("open", map[string]string{"path": doc}, &out)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("open without grant = %v, want permission denial", err)
	}
	v := h.view(t)
	if v.Error == "" {
		t.Fatalf("denied open should surface in the view error slot")
	}
	if hasItem(v, "docinfo") {
		t.Fatalf("no document should be open")
	}
}

func TestHandoffStyleOpenRendersMarkdown(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	body := "# Guide\n\nIntro *text* with [a link](http://x).\n\n- one\n- two\n\n```go\nfmt.Println(1)\n```\n\n> a quote\n"
	fs.put(doc, []byte(body))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]any{"path": doc, "grant_id": "g-1", "mode": "read"}, &out)
	if out["path"] != doc || out["document"] != true || out["grant_id"] != "g-1" {
		t.Fatalf("open result = %v", out)
	}
	v := h.viewAt(t, 2)
	if v.Title != "Markview — guide.md" {
		t.Fatalf("title = %q", v.Title)
	}
	if !hasItem(v, "docinfo") {
		t.Fatalf("docinfo row missing")
	}
	for _, want := range []string{"# Guide", "Intro text with a link (http://x).", "• one", "┃ fmt.Println(1)", "│ a quote"} {
		found := false
		for _, l := range itemLabels(v) {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing label %q in %v", want, itemLabels(v))
		}
	}
	// The rendered document stays inside the contract's item budget.
	if len(v.Items) > 64 {
		t.Fatalf("items = %d exceeds the ~64 bound", len(v.Items))
	}
}

func TestOpenRejectsSiblingOutsideGrant(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	sib := "/users/guest/documents/secret.md"
	fs.put(doc, []byte("# Guide\n"))
	fs.put(sib, []byte("# Secret\n"))
	fs.grant(doc) // scoped to the document only, like the handoff grant
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	err := h.call("open", map[string]string{"path": sib}, &out)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("sibling open = %v, want denial", err)
	}
	// The open document is untouched by the failed attempt.
	v := h.view(t)
	if itemDetail(v, "docinfo") == "" || !strings.Contains(itemDetail(v, "docinfo"), doc) {
		t.Fatalf("open doc lost after denied sibling: %v", itemLabels(v))
	}
}

func TestReloadAfterGrantRevocationDenies(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	fs.put(doc, []byte("# Guide\n"))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	fs.revoke(doc) // simulates the session-bound grant dying with the session
	if err := h.call("reload", nil, &out); err == nil {
		t.Fatalf("reload after grant revocation should be denied")
	}
}

// ---- honest rejection ------------------------------------------------------

func TestRejectsBinaryContent(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/blob.md"
	fs.put(doc, []byte{'#', 'x', 0x00, 0x01, 0x02})
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	err := h.call("open", map[string]string{"path": doc}, &out)
	if err == nil || !strings.Contains(err.Error(), "binary") {
		t.Fatalf("binary open = %v", err)
	}
}

func TestRejectsNonUTF8Content(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/blob.md"
	fs.put(doc, []byte{0xff, 0xfe, 0xfd, 'a'})
	fs.grant(doc)
	h := openHarness(t, fs)
	if err := h.call("open", map[string]string{"path": doc}, nil); err == nil {
		t.Fatalf("invalid UTF-8 should be rejected")
	}
}

func TestRejectsNonMarkdownExtension(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/notes.txt"
	fs.put(doc, []byte("plain text\n"))
	fs.grant(doc)
	h := openHarness(t, fs)
	err := h.call("open", map[string]string{"path": doc}, nil)
	if err == nil || !strings.Contains(err.Error(), "not a Markdown document") {
		t.Fatalf("txt open = %v", err)
	}
}

func TestRejectsDirectory(t *testing.T) {
	fs := newMockFS()
	dir := "/users/guest/documents"
	fs.mkdirAll(dir)
	fs.grant(dir)
	h := openHarness(t, fs)
	err := h.call("open", map[string]string{"path": dir}, nil)
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("directory open = %v", err)
	}
}

func TestRejectsMissingPath(t *testing.T) {
	h := openHarness(t, newMockFS())
	if err := h.call("open", map[string]string{"path": "  "}, nil); err == nil {
		t.Fatalf("empty path should be rejected")
	}
	if err := h.call("open", nil, nil); err == nil {
		t.Fatalf("missing params should be rejected")
	}
}

// ---- bounds ----------------------------------------------------------------

func TestPagingBoundsAndMoreIndicator(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/long.md"
	var b strings.Builder
	for i := 0; i < pageRows*2+10; i++ {
		fmt.Fprintf(&b, "- item %d\n", i)
	}
	fs.put(doc, []byte(b.String()))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	if out["pages"].(float64) != 3 {
		t.Fatalf("pages = %v, want 3", out["pages"])
	}
	v := h.view(t)
	if !hasItem(v, "more") {
		t.Fatalf("first page should carry the more indicator")
	}
	if !strings.Contains(itemLabels(v)[len(itemLabels(v))-1], "and") {
		// find the indicator label text
	}
	if len(v.Items) > 64 {
		t.Fatalf("items = %d exceeds the bound", len(v.Items))
	}
	// The indicator names the honest remainder.
	if itemDetail(v, "more") == "" {
		t.Fatalf("more item missing detail")
	}
	v = h.act(t, "next_page", nil)
	if itemDetail(v, "more") == "" {
		t.Fatalf("page 2 should still have a remainder")
	}
	v = h.act(t, "next_page", nil)
	if hasItem(v, "more") {
		t.Fatalf("last page must not carry the more indicator")
	}
	for _, a := range v.Actions {
		if a.ID == "next_page" && !a.Disabled {
			t.Fatalf("next_page should be disabled on the last page")
		}
	}
	v = h.act(t, "prev_page", nil)
	if !hasItem(v, "more") {
		t.Fatalf("going back should restore the remainder indicator")
	}
}

func TestRowCapIndicator(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/huge.md"
	fs.put(doc, []byte(strings.Repeat("- x\n", maxRows+100)))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	if out["row_capped"] != true || out["rows"].(float64) != float64(maxRows) {
		t.Fatalf("row cap result = %v", out)
	}
	// Last page carries the explicit cap indicator.
	for i := 0; i < int(out["pages"].(float64))-1; i++ {
		h.act(t, "next_page", nil)
	}
	v := h.view(t)
	if !hasItem(v, "rowcap") {
		t.Fatalf("capped document should carry the rowcap indicator")
	}
}

func TestSizeCapIndicator(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/big.md"
	fs.put(doc, []byte(strings.Repeat("para text here\n", (maxDocumentBytes/15)+2)))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	if out["truncated"] != true {
		t.Fatalf("oversized file should mark truncated, got %v", out)
	}
	v := h.view(t)
	if !hasItem(v, "truncated") {
		t.Fatalf("oversized document should carry the truncated indicator, items=%v", itemLabels(v))
	}
}

// ---- reload & actions ------------------------------------------------------

func TestReloadPicksUpChanges(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	fs.put(doc, []byte("# v1\n"))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	fs.put(doc, []byte("# v2\n\n- new item\n"))
	h.must(t, "reload", nil, &out)
	v := h.view(t)
	found := false
	for _, l := range itemLabels(v) {
		if l == "# v2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reloaded content missing: %v", itemLabels(v))
	}
}

func TestOpenActionUsesFieldValue(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	fs.put(doc, []byte("# Guide\n"))
	fs.grant(doc)
	h := openHarness(t, fs)

	v := h.act(t, "open", map[string]string{"open_path": doc})
	if !hasItem(v, "docinfo") {
		t.Fatalf("open action did not open the document")
	}
	v = h.act(t, "open", map[string]string{"open_path": "/users/guest/documents/other.md"})
	if v.Error == "" {
		t.Fatalf("open without grant should surface an error banner")
	}
}

func TestEmptyDocument(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/empty.md"
	fs.put(doc, nil)
	fs.grant(doc)
	h := openHarness(t, fs)
	var out map[string]any
	h.must(t, "open", map[string]string{"path": doc}, &out)
	v := h.view(t)
	if !hasItem(v, "empty") {
		t.Fatalf("empty document should show the empty indicator")
	}
}

func TestDocRouteReportsOpenDocument(t *testing.T) {
	fs := newMockFS()
	doc := "/users/guest/documents/guide.md"
	fs.put(doc, []byte("# Guide\n"))
	fs.grant(doc)
	h := openHarness(t, fs)

	var out map[string]any
	h.must(t, "doc", nil, &out)
	if out["document"] != false {
		t.Fatalf("doc route before open = %v", out)
	}
	h.must(t, "open", map[string]string{"path": doc}, &out)
	h.must(t, "doc", nil, &out)
	if out["document"] != true || out["path"] != doc || out["name"] != "guide.md" {
		t.Fatalf("doc route = %v", out)
	}
}
