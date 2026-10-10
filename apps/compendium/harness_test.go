package compendium

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gostalgia/sdk"
)

const (
	testRoot  = "/apps/data/com.gostalgia.compendium"
	testIndex = testRoot + "/index.json"
	testVault = testRoot + "/vault"
)

var errCrashed = errors.New("mockfs: process killed")

// crashMode describes what a simulated kill -9 leaves behind at the
// crashing mutation. Every fs/* mutation is a single atomic VFS operation,
// so a kill lands before it, after it, or inside fs/save's own
// stage-then-rename sequence (torn staging file or .recover artifact).
type crashMode int

const (
	crashBefore  crashMode = iota // op never applied
	crashAfter                    // op applied, then the process died
	crashTorn                     // fs/save died mid-staging: partial .tmp file, target untouched
	crashRecover                  // fs/save staged fully but rename failed: .recover artifact, target untouched
)

func (m crashMode) String() string {
	return [...]string{"before", "after", "torn-stage", "rename-failed"}[m]
}

// mockFS is an in-memory model of the app-private VFS partition with the
// same semantics Compendium relies on: atomic fs/save that creates parent
// directories, same-mount fs/rename that refuses to overwrite, recursive
// fs/remove, and fs/list that fails for missing directories.
type mockFS struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool

	ops     int // mutating operations applied or attempted
	crashAt int // 1-based mutating op to crash on; 0 = never
	mode    crashMode
	dead    bool
	tmpSeq  int

	// foldCase models a case-insensitive, case-preserving host (macOS
	// APFS, Windows NTFS): every path resolves to an existing entry that
	// matches ignoring case, and fs/rename refuses a destination that
	// already resolves to an entry, exactly as vfs.RenameOpt's Stat check
	// does there, even when that entry is the source itself.
	foldCase bool
	// foldNorm models a normalization-insensitive, normalization-preserving
	// host (macOS APFS): names that are canonically equivalent Unicode
	// ("Caf\u00e9" and "Cafe\u0301") resolve to the same entry, and
	// fs/save with overwrite=false or fs/rename onto such a name fails as
	// the destination exists. Combined with foldCase it models default APFS.
	foldNorm bool
	// maxFrame, when set, models the IPC frame limit: an fs/save whose
	// base64 payload exceeds it fails, as does an fs/read of a file whose
	// base64 encoding would.
	maxFrame int
	// fail injects ordinary I/O errors (the process keeps running): when it
	// returns non-nil for a call, that call fails without being applied.
	fail func(method, path string) error
}

// canon resolves p against existing entries ignoring case when foldCase is
// set, component by component, keeping unmatched components as given.
func (m *mockFS) canon(p string) string {
	if !m.foldCase && !m.foldNorm || p == "" || p == "/" {
		return p
	}
	cur := "/"
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		next := path.Join(cur, part)
		if !m.exists(next) {
			for f := range m.files {
				if path.Dir(f) == cur && m.sameName(path.Base(f), part) {
					next = f
				}
			}
			for d := range m.dirs {
				if d != "/" && path.Dir(d) == cur && m.sameName(path.Base(d), part) {
					next = d
				}
			}
		}
		cur = next
	}
	return cur
}

// sameName reports whether the host would resolve a and b to one entry.
func (m *mockFS) sameName(a, b string) bool {
	if m.foldNorm {
		a, b = testNFD(a), testNFD(b)
	}
	if m.foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// testNFD decomposes the precomposed letters the tests use, independently
// of the production fold, so the model does not share its bugs.
func testNFD(s string) string {
	return strings.NewReplacer(
		"\u00e9", "e\u0301", "\u00c9", "E\u0301", "\u00e8", "e\u0300", "\u00fc", "u\u0308",
		"\u00dc", "U\u0308", "\u00f1", "n\u0303", "\u00c5", "A\u030a", "\u00e5", "a\u030a",
		"\u212b", "A\u030a", "\u1e69", "s\u0323\u0307", "\u1e9b", "\u017f\u0307",
	).Replace(s)
}

func newMockFS() *mockFS {
	return &mockFS{files: map[string][]byte{}, dirs: map[string]bool{"/": true}}
}

func (m *mockFS) clone() *mockFS {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := newMockFS()
	c.foldCase, c.foldNorm, c.maxFrame = m.foldCase, m.foldNorm, m.maxFrame
	for k, v := range m.files {
		c.files[k] = append([]byte(nil), v...)
	}
	for k := range m.dirs {
		c.dirs[k] = true
	}
	return c
}

func (m *mockFS) mkdirAll(p string) {
	for p != "/" && p != "." && p != "" {
		m.dirs[p] = true
		p = path.Dir(p)
	}
}

func (m *mockFS) exists(p string) bool { return m.files[p] != nil || m.dirs[p] }

func (m *mockFS) get(p string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.files[p]
	return d, ok
}

func (m *mockFS) put(p string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mkdirAll(path.Dir(p))
	m.files[p] = append([]byte{}, data...)
}

func (m *mockFS) del(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, p)
}

func (m *mockFS) paths(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for p := range m.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

func (m *mockFS) call(_ context.Context, method string, params, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dead {
		return errCrashed
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var p struct {
		Path      string `json:"path"`
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Data      string `json:"data_base64"`
		Overwrite *bool  `json:"overwrite"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if !strings.HasPrefix(p.Path+p.Src, testRoot) && method != "sys/ping" {
		return fmt.Errorf("permission denied: %s outside app-private storage", p.Path+p.Src)
	}
	p.Path, p.Src, p.Dst = m.canon(p.Path), m.canon(p.Src), m.canon(p.Dst)
	if m.fail != nil {
		if err := m.fail(method, p.Path+p.Src); err != nil {
			return err
		}
	}
	if m.maxFrame > 0 && len(p.Data) > m.maxFrame {
		return fmt.Errorf("payload size %d exceeds write limit of %d bytes", len(p.Data), m.maxFrame)
	}
	reply := func(v any) error {
		if out == nil {
			return nil
		}
		b, _ := json.Marshal(v)
		return json.Unmarshal(b, out)
	}
	mutating := method == "fs/save" || method == "fs/rename" || method == "fs/remove" ||
		method == "fs/mkdir" || method == "fs/write"
	if mutating {
		m.ops++
		if m.crashAt > 0 && m.ops == m.crashAt {
			switch m.mode {
			case crashBefore:
				m.dead = true
				return errCrashed
			case crashTorn, crashRecover:
				if method == "fs/save" {
					data, _ := base64.StdEncoding.DecodeString(p.Data)
					m.mkdirAll(path.Dir(p.Path))
					m.tmpSeq++
					if m.mode == crashTorn {
						m.files[fmt.Sprintf("%s.tmp.%d.%d", p.Path, 1700000000000000000+m.tmpSeq, m.tmpSeq)] = data[:len(data)/2]
					} else {
						m.files[p.Path+".recover"] = data
					}
				}
				m.dead = true
				return errCrashed
			case crashAfter:
				defer func() { m.dead = true }()
			}
		}
	}
	switch method {
	case "fs/mkdir":
		m.mkdirAll(p.Path)
		return reply(map[string]any{"path": p.Path, "created": true})
	case "fs/list":
		if !m.dirs[p.Path] {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		type entry struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
			Size  int64  `json:"size"`
		}
		var entries []entry
		for f, d := range m.files {
			if path.Dir(f) == p.Path {
				entries = append(entries, entry{Name: path.Base(f), Size: int64(len(d))})
			}
		}
		for d := range m.dirs {
			if d != p.Path && path.Dir(d) == p.Path {
				entries = append(entries, entry{Name: path.Base(d), IsDir: true})
			}
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		return reply(map[string]any{"path": p.Path, "entries": entries})
	case "fs/read":
		data, ok := m.files[p.Path]
		if !ok {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		if m.maxFrame > 0 && base64.StdEncoding.EncodedLen(len(data)) > m.maxFrame {
			return fmt.Errorf("file size %d exceeds default read limit", len(data))
		}
		return reply(map[string]any{"path": p.Path, "size": len(data), "data_base64": base64.StdEncoding.EncodeToString(data)})
	case "fs/save", "fs/write":
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err != nil {
			return err
		}
		if m.dirs[p.Path] {
			return fmt.Errorf("is_dir: %s", p.Path)
		}
		if p.Overwrite != nil && !*p.Overwrite && m.files[p.Path] != nil {
			return fmt.Errorf("exists: %s", p.Path)
		}
		m.mkdirAll(path.Dir(p.Path))
		m.files[p.Path] = append([]byte{}, data...)
		return reply(map[string]any{"path": p.Path})
	case "fs/rename":
		if !m.exists(p.Src) {
			return fmt.Errorf("not_found: %s", p.Src)
		}
		if m.exists(p.Dst) && (m.foldCase || m.foldNorm || !strings.EqualFold(p.Src, p.Dst)) {
			return fmt.Errorf("exists: %s", p.Dst)
		}
		if !m.dirs[path.Dir(p.Dst)] {
			return fmt.Errorf("not_found: parent of %s", p.Dst)
		}
		if d, ok := m.files[p.Src]; ok {
			delete(m.files, p.Src)
			m.files[p.Dst] = d
		} else {
			moved := map[string][]byte{}
			for f, d := range m.files {
				if strings.HasPrefix(f, p.Src+"/") {
					moved[p.Dst+strings.TrimPrefix(f, p.Src)] = d
					delete(m.files, f)
				}
			}
			for f, d := range moved {
				m.files[f] = d
			}
			var dirs []string
			for d := range m.dirs {
				if d == p.Src || strings.HasPrefix(d, p.Src+"/") {
					dirs = append(dirs, d)
				}
			}
			for _, d := range dirs {
				delete(m.dirs, d)
				m.dirs[p.Dst+strings.TrimPrefix(d, p.Src)] = true
			}
		}
		return reply(map[string]any{"src": p.Src, "dst": p.Dst, "renamed": true})
	case "fs/remove":
		if !m.exists(p.Path) {
			return fmt.Errorf("not_found: %s", p.Path)
		}
		if m.dirs[p.Path] {
			for f := range m.files {
				if strings.HasPrefix(f, p.Path+"/") {
					if !p.Recursive {
						return fmt.Errorf("not_empty: %s", p.Path)
					}
					delete(m.files, f)
				}
			}
			for d := range m.dirs {
				if d == p.Path || strings.HasPrefix(d, p.Path+"/") {
					delete(m.dirs, d)
				}
			}
		}
		delete(m.files, p.Path)
		return reply(map[string]any{"path": p.Path, "removed": true})
	}
	return fmt.Errorf("mockfs: unsupported method %s", method)
}

// harness drives one Compendium instance through its IPC routes exactly as
// a remote caller would: JSON params in, JSON results out.
type harness struct {
	t        *testing.T
	fs       *mockFS
	app      *Compendium
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
	if tt, ok := t.(*testing.T); ok {
		h.t = tt
	}
	h.app = inst.(*Compendium)
	setClock(h.app, func() time.Time { return time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC) })
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
		shown, _ := json.Marshal(params)
		t.Fatalf("%s %s: %v", route, clip(string(shown), 120), err)
	}
}

func (h *harness) view(t testing.TB) sdk.View {
	t.Helper()
	return h.viewV(t, 1)
}

func (h *harness) viewV(t testing.TB, version int) sdk.View {
	t.Helper()
	res, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	v := res.(sdk.View)
	if err := v.Validate(); err != nil {
		t.Fatalf("view does not validate: %v", err)
	}
	return v
}

func (h *harness) act(t testing.TB, action, item string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryActV(1, action, item, values)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) actV(t testing.TB, version int, action, item string, values map[string]string) sdk.View {
	t.Helper()
	v, err := h.tryActV(version, action, item, values)
	if err != nil {
		t.Fatalf("action %s: %v", action, err)
	}
	return v
}

func (h *harness) tryActV(version int, action, item string, values map[string]string) (sdk.View, error) {
	cur, err := h.handlers["view"](context.Background(), json.RawMessage(fmt.Sprintf(`{"version":%d}`, version)))
	if err != nil {
		return sdk.View{}, err
	}
	req := sdk.ActionRequest{
		Version: version, Instance: cur.(sdk.View).Instance,
		RequestID: fmt.Sprintf("req_%d", h.seq.Add(1)), Action: action, ItemID: item, Values: values,
	}
	raw, _ := json.Marshal(req)
	res, err := h.handlers["action"](context.Background(), raw)
	if err != nil {
		return sdk.View{}, err
	}
	v := res.(sdk.View)
	return v, v.Validate()
}

// Result shapes of the public routes, decoded the way an IPC client would.
type linkOut struct {
	Target   string `json:"target"`
	Resolved bool   `json:"resolved"`
}

type backlinkOut struct {
	Title   string `json:"title"`
	Context string `json:"context"`
}

type noteOut struct {
	Title     string        `json:"title"`
	Content   string        `json:"content"`
	Size      int           `json:"size"`
	SHA256    string        `json:"sha256"`
	Links     []linkOut     `json:"links"`
	Backlinks []backlinkOut `json:"backlinks"`
	Tags      []string      `json:"tags"`
	Rendered  string        `json:"rendered"`
}

type renameOut struct {
	From           string   `json:"from"`
	To             string   `json:"to"`
	RewrittenNotes []string `json:"rewritten_notes"`
	RewrittenLinks int      `json:"rewritten_links"`
	Orphaned       []struct {
		Note     string `json:"note"`
		Location string `json:"location"`
		Target   string `json:"target"`
	} `json:"orphaned"`
}

type searchOut struct {
	Total   int     `json:"total"`
	TookMS  float64 `json:"took_ms"`
	Results []struct {
		Title   string  `json:"title"`
		Score   float64 `json:"score"`
		Snippet string  `json:"snippet"`
	} `json:"results"`
}

type graphOut struct {
	Nodes []struct {
		Title string   `json:"title"`
		In    int      `json:"in"`
		Out   int      `json:"out"`
		Tags  []string `json:"tags"`
	} `json:"nodes"`
	Edges []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"edges"`
	DeadEnds   []string `json:"dead_ends"`
	Orphans    []string `json:"orphans"`
	Unresolved []struct {
		Target string   `json:"target"`
		From   []string `json:"from"`
	} `json:"unresolved"`
}

type statusOut struct {
	Notes int `json:"notes"`
	Index struct {
		Rebuilt         bool `json:"rebuilt"`
		Reparsed        int  `json:"reparsed"`
		JournalReplayed bool `json:"journal_replayed"`
	} `json:"index"`
	Drafts int `json:"drafts"`
	Trash  int `json:"trash"`
}

type draftsOut struct {
	Drafts []struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	} `json:"drafts"`
}

type trashListOut struct {
	Items []struct {
		TrashID string `json:"trash_id"`
		Title   string `json:"title"`
	} `json:"items"`
}

type historyOut struct {
	Versions []struct {
		ID     string `json:"id"`
		Size   int    `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"versions"`
}

type exportOut struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	DataBase64 string `json:"data_base64"`
	Files      int    `json:"files"`
}

func titles(bl []backlinkOut) []string {
	var out []string
	for _, b := range bl {
		out = append(out, b.Title)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
