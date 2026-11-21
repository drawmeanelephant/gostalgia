package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// mockAppLauncher tracks launched apps and handles IsRunning/Launch.
type mockAppLauncher struct {
	mu        sync.Mutex
	running   map[string]bool
	launched  []string
	launchErr map[string]error
}

func newMockLauncher() *mockAppLauncher {
	return &mockAppLauncher{
		running:   make(map[string]bool),
		launchErr: make(map[string]error),
	}
}

func (m *mockAppLauncher) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running[id]
}

func (m *mockAppLauncher) Launch(ctx context.Context, id string) (*process.Process, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.launchErr[id]; err != nil {
		return nil, err
	}
	m.running[id] = true
	m.launched = append(m.launched, id)
	return nil, nil
}

// mockDispatcher records dispatched IPC requests.
type mockDispatcher struct {
	mu         sync.Mutex
	dispatched []ipc.Request
	handler    func(req ipc.Request) ipc.Response
}

func newMockDispatcher() *mockDispatcher {
	return &mockDispatcher{}
}

func (m *mockDispatcher) Dispatch(ctx context.Context, req ipc.Request) ipc.Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dispatched = append(m.dispatched, req)
	if m.handler != nil {
		return m.handler(req)
	}
	return ipc.Response{ID: req.ID, OK: true}
}

func setupTestVFS(t *testing.T) (*vfs.VFS, *vfs.GrantStore) {
	t.Helper()
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root")
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	host, err := vfs.NewHost(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	env := vfs.New(host)
	if err := env.MkdirAll("/users/guest/documents"); err != nil {
		t.Fatal(err)
	}
	if err := env.MkdirAll("/users/guest/config"); err != nil {
		t.Fatal(err)
	}
	if err := env.MkdirAll("/apps/data/com.gostalgia.app1"); err != nil {
		t.Fatal(err)
	}
	if err := env.MkdirAll("/apps/data/com.gostalgia.app2"); err != nil {
		t.Fatal(err)
	}

	return env, env.Grants()
}

func TestAssociations(t *testing.T) {
	table := NewAssociationTable()

	// 1. Built-in associations
	app, handlers, found := table.Resolve("/users/guest/documents/notes.txt")
	if !found || app != "com.gostalgia.notes" {
		t.Fatalf("expected com.gostalgia.notes for .txt, got %s (found: %v)", app, found)
	}
	if len(handlers) == 0 {
		t.Fatalf("expected handlers for .txt, got 0")
	}

	app, _, found = table.Resolve("readme.md")
	if !found || app != "com.gostalgia.notes" {
		t.Fatalf("expected com.gostalgia.notes for .md, got %s", app)
	}

	app, _, found = table.Resolve("directory")
	if !found || app != "com.gostalgia.files" {
		t.Fatalf("expected com.gostalgia.files for directory, got %s", app)
	}

	// 2. Custom registration and overriding default
	customAssoc := sdk.DocumentTypeAssociation{
		Extension: ".custom",
		MimeType:  "application/x-custom",
		AppID:     "com.example.custom",
		Name:      "Custom Viewer",
		Default:   true,
	}
	if err := table.Register(customAssoc); err != nil {
		t.Fatal(err)
	}

	app, handlers, found = table.Resolve("file.CUSTOM")
	if !found || app != "com.example.custom" {
		t.Fatalf("expected com.example.custom, got %s", app)
	}
	if len(handlers) != 1 || handlers[0].AppID != "com.example.custom" {
		t.Fatalf("unexpected handlers: %+v", handlers)
	}

	// 3. RegisterFromManifest
	man := sdk.Manifest{
		ID:            "com.example.editor",
		Name:          "Editor Pro",
		DocumentTypes: []string{".custom", ".doc"},
	}
	table.RegisterFromManifest(man)

	_, handlers, found = table.Resolve(".custom")
	if !found || len(handlers) != 2 {
		t.Fatalf("expected 2 handlers for .custom, got %d", len(handlers))
	}

	// 4. List sorted
	list := table.List()
	if len(list) < 5 {
		t.Fatalf("expected at least 5 associations in list, got %d", len(list))
	}
}

func TestRecentsStore(t *testing.T) {
	v, grants := setupTestVFS(t)
	storePath := "/users/guest/config/recents.json"
	store := NewRecentsStore(v, grants, storePath)

	docPath := "/users/guest/documents/test1.txt"
	if err := v.SaveAtomic(docPath, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Add recent entry
	entry, err := store.Add(docPath, "com.gostalgia.notes")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Path != docPath || !entry.Exists || entry.Size != 11 {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	// 2. Add multiple and verify order (newest first)
	doc2 := "/users/guest/documents/test2.txt"
	_ = v.SaveAtomic(doc2, []byte("second"), 0o644)
	_, _ = store.Add(doc2, "com.gostalgia.notes")

	list := store.List("", true)
	if len(list) != 2 {
		t.Fatalf("expected 2 recents, got %d", len(list))
	}
	if list[0].Path != doc2 || list[1].Path != docPath {
		t.Fatalf("expected newest first: %+v", list)
	}

	// 3. Restart recovery / persistence
	store2 := NewRecentsStore(v, grants, storePath)
	if err := store2.Load(); err != nil {
		t.Fatal(err)
	}
	list2 := store2.List("", false)
	if len(list2) != 2 {
		t.Fatalf("expected 2 recents after reload, got %d", len(list2))
	}

	// 4. Stale path / missing file detection
	_ = v.Remove(doc2)
	listWithVerify := store.List("", true)
	var doc2Entry *sdk.RecentDocument
	for i := range listWithVerify {
		if listWithVerify[i].Path == doc2 {
			doc2Entry = &listWithVerify[i]
		}
	}
	if doc2Entry == nil || doc2Entry.Exists {
		t.Fatalf("expected doc2 to have Exists=false after deletion, got %+v", doc2Entry)
	}

	// 5. Prune missing
	pruned, err := store.PruneMissing()
	if err != nil || pruned != 1 {
		t.Fatalf("expected 1 pruned, got %d, err=%v", pruned, err)
	}
	if len(store.List("", false)) != 1 {
		t.Fatalf("expected 1 recent remaining after prune")
	}

	// 6. Permission-aware filtering:
	// If caller is an app without access, entry should be filtered out
	appCaller := "com.example.unauthorized"
	if len(store.List(appCaller, false)) != 0 {
		t.Fatalf("expected 0 recents for unauthorized app")
	}

	// Grant read access to app
	_, err = grants.Issue(appCaller, docPath, vfs.AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.List(appCaller, false)) != 1 {
		t.Fatalf("expected 1 recent for authorized app")
	}

	// 7. Bounded capacity
	smallStore := NewRecentsStore(v, grants, "/users/guest/config/recents_small.json")
	smallStore.maxEntries = 3
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("/users/guest/documents/b_%d.txt", i)
		_ = v.SaveAtomic(p, []byte("data"), 0o644)
		_, _ = smallStore.Add(p, "app")
	}
	if len(smallStore.List("", false)) != 3 {
		t.Fatalf("expected bounded to 3 entries, got %d", len(smallStore.List("", false)))
	}
}

func TestFavoritesStore(t *testing.T) {
	v, grants := setupTestVFS(t)
	storePath := "/users/guest/config/favorites.json"
	store := NewFavoritesStore(v, grants, storePath)

	docPath := "/users/guest/documents/fav1.txt"
	_ = v.SaveAtomic(docPath, []byte("favorite content"), 0o644)

	// 1. Add favorite with label
	fav, err := store.Add(docPath, "My Favorite Note")
	if err != nil {
		t.Fatal(err)
	}
	if fav.Label != "My Favorite Note" || fav.Rank != 0 || !fav.Exists {
		t.Fatalf("unexpected favorite: %+v", fav)
	}

	// 2. Add second favorite and reorder
	doc2 := "/users/guest/documents/fav2.txt"
	_ = v.SaveAtomic(doc2, []byte("second fav"), 0o644)
	_, _ = store.Add(doc2, "Second Note")

	list := store.List("", true)
	if len(list) != 2 || list[0].Path != docPath || list[1].Path != doc2 {
		t.Fatalf("unexpected list: %+v", list)
	}

	if err := store.Reorder([]string{doc2, docPath}); err != nil {
		t.Fatal(err)
	}
	reordered := store.List("", false)
	if reordered[0].Path != doc2 || reordered[1].Path != docPath {
		t.Fatalf("reordering failed: %+v", reordered)
	}

	// 3. Persistence
	store2 := NewFavoritesStore(v, grants, storePath)
	if err := store2.Load(); err != nil {
		t.Fatal(err)
	}
	if len(store2.List("", false)) != 2 {
		t.Fatalf("failed to reload favorites: %+v", store2.List("", false))
	}

	// 4. Permission filtering
	appCaller := "com.example.app"
	if len(store.List(appCaller, false)) != 0 {
		t.Fatalf("expected 0 favorites for app without grant")
	}
	_, _ = grants.Issue(appCaller, doc2, vfs.AccessRead, false)
	appFavs := store.List(appCaller, false)
	if len(appFavs) != 1 || appFavs[0].Path != doc2 {
		t.Fatalf("expected only granted favorite, got %+v", appFavs)
	}

	// 5. Remove
	if err := store.Remove(docPath); err != nil {
		t.Fatal(err)
	}
	if len(store.List("", false)) != 1 {
		t.Fatalf("remove failed")
	}
}

func TestSearcherPermissionAwareAndBounded(t *testing.T) {
	v, grants := setupTestVFS(t)
	searcher := NewSearcher(v, grants, nil, nil)
	ctx := context.Background()

	// Seed directory structure
	// /users/guest/documents/secret.txt
	// /users/guest/documents/public.txt
	// /users/guest/documents/sub/deep.txt
	// /apps/data/com.gostalgia.app1/priv1.txt
	// /apps/data/com.gostalgia.app2/priv2.txt
	_ = v.SaveAtomic("/users/guest/documents/secret.txt", []byte("secret text"), 0o644)
	_ = v.SaveAtomic("/users/guest/documents/public.txt", []byte("public text"), 0o644)
	_ = v.MkdirAll("/users/guest/documents/sub")
	_ = v.SaveAtomic("/users/guest/documents/sub/deep.txt", []byte("deep text"), 0o644)
	_ = v.SaveAtomic("/apps/data/com.gostalgia.app1/priv1.txt", []byte("app1 private"), 0o644)
	_ = v.SaveAtomic("/apps/data/com.gostalgia.app2/priv2.txt", []byte("app2 private"), 0o644)

	opPrincipal := security.OperatorPrincipal(security.User{Name: "guest"})
	app1Principal := security.AppPrincipal("com.gostalgia.app1", 101, "", security.User{Name: "guest"})

	// 1. Operator search can find all non-private files
	res, err := searcher.Search(ctx, opPrincipal, sdk.DocumentSearchQuery{
		Path:  "/users/guest/documents",
		Query: "txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("expected 3 files, got %d: %+v", len(res.Results), res.Results)
	}

	// 2. App caller without grant searching /users/guest/documents must be rejected
	_, err = searcher.Search(ctx, app1Principal, sdk.DocumentSearchQuery{
		Path:  "/users/guest/documents",
		Query: "txt",
	})
	if err == nil {
		t.Fatalf("expected permission error for ungranted search")
	}

	// 3. Grant app1 read access ONLY to public.txt (single file grant, recursive=false)
	_, err = grants.Issue("com.gostalgia.app1", "/users/guest/documents/public.txt", vfs.AccessRead, false)
	if err != nil {
		t.Fatal(err)
	}

	// Search on exact granted file succeeds
	res, err = searcher.Search(ctx, app1Principal, sdk.DocumentSearchQuery{
		Path: "/users/guest/documents/public.txt",
	})
	if err != nil || len(res.Results) != 1 {
		t.Fatalf("expected 1 result for granted single file: err=%v, res=%+v", err, res)
	}

	// 4. Grant directory access to /users/guest/documents (recursive: true)
	grantDir, err := grants.Issue("com.gostalgia.app1", "/users/guest/documents", vfs.AccessRead, true)
	if err != nil {
		t.Fatal(err)
	}

	res, err = searcher.Search(ctx, app1Principal, sdk.DocumentSearchQuery{
		Path:      "/users/guest/documents",
		Query:     "deep",
		Extension: ".txt",
	})
	if err != nil || len(res.Results) != 1 || res.Results[0].Name != "deep.txt" {
		t.Fatalf("expected deep.txt, got %+v (err: %v)", res, err)
	}

	// 5. Revocation: revoke the directory grant -> app1 can no longer search directory
	if err := grants.Revoke(grantDir.ID); err != nil {
		t.Fatal(err)
	}
	_, err = searcher.Search(ctx, app1Principal, sdk.DocumentSearchQuery{
		Path:  "/users/guest/documents",
		Query: "deep",
	})
	if err == nil {
		t.Fatalf("expected permission error after grant revocation")
	}

	// 6. Cross-app isolation: app1 searching without path can search its own private storage, but not app2's
	res, err = searcher.Search(ctx, app1Principal, sdk.DocumentSearchQuery{
		Query: "priv",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Results {
		if r.Name == "priv2.txt" {
			t.Fatalf("app1 accessed app2 private storage: %+v", r)
		}
	}

	// 7. Indexed lookups and stale file handling
	_ = searcher.IndexDocument("/users/guest/documents/public.txt")
	_ = searcher.IndexDocument("/users/guest/documents/secret.txt")

	// Operator lookup
	lRes, err := searcher.Lookup(ctx, opPrincipal, "secret", ".txt", 10)
	if err != nil || len(lRes.Results) != 1 {
		t.Fatalf("expected secret in lookup: %+v (err: %v)", lRes, err)
	}

	// App1 lookup should NOT see secret.txt (no grant)
	lResApp, err := searcher.Lookup(ctx, app1Principal, "secret", ".txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lResApp.Results) != 0 {
		t.Fatalf("app1 should not see ungranted indexed file: %+v", lResApp.Results)
	}

	// Stale file check: remove public.txt and lookup
	_ = v.Remove("/users/guest/documents/public.txt")
	lResStale, err := searcher.Lookup(ctx, opPrincipal, "public", ".txt", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lResStale.Results) != 0 {
		t.Fatalf("expected stale missing file to be excluded from lookup: %+v", lResStale)
	}
}

func TestHandoffIsolationAndScoping(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	launcher := newMockLauncher()
	dispatcher := newMockDispatcher()

	handoff := NewHandoffManager(v, grants, assoc, recents, launcher, dispatcher)
	ctx := context.Background()

	// Setup doc file
	docDir := "/users/guest/documents"
	doc1 := "/users/guest/documents/report.txt"
	doc2 := "/users/guest/documents/other.txt"
	_ = v.SaveAtomic(doc1, []byte("annual report"), 0o644)
	_ = v.SaveAtomic(doc2, []byte("confidential notes"), 0o644)

	filesAppPrincipal := security.AppPrincipal("com.gostalgia.files", 10, "", security.User{Name: "guest"})
	notesAppID := "com.gostalgia.notes"

	// 1. Files app has access to /users/guest/documents
	_, err := grants.Issue("com.gostalgia.files", docDir, vfs.AccessReadWrite, true)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Perform open-with handoff from Files -> Notes for report.txt
	hResult, err := handoff.Handoff(ctx, filesAppPrincipal, sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    doc1,
		AppID:   notesAppID,
		Mode:    "read-write",
	})
	if err != nil {
		t.Fatalf("handoff failed: %v", err)
	}
	if !hResult.Success || hResult.GrantID == "" || hResult.AppID != notesAppID {
		t.Fatalf("unexpected handoff result: %+v", hResult)
	}

	// 3. Verify target app was launched
	if !launcher.IsRunning(notesAppID) {
		t.Fatalf("expected notes to be launched")
	}

	// 4. Verify open dispatch took place
	if len(dispatcher.dispatched) != 1 {
		t.Fatalf("expected 1 dispatched IPC request, got %d", len(dispatcher.dispatched))
	}
	var openParams struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		GrantID string `json:"grant_id"`
	}
	if err := json.Unmarshal(dispatcher.dispatched[0].Params, &openParams); err != nil {
		t.Fatal(err)
	}
	if openParams.Path != doc1 || openParams.GrantID != hResult.GrantID {
		t.Fatalf("unexpected dispatch params: %+v", openParams)
	}

	// 5. CRITICAL ISOLATION CHECK:
	// Verify Notes app received a grant ONLY for report.txt and NOT for the parent directory or doc2
	if err := grants.CheckAccess(notesAppID, doc1, vfs.AccessReadWrite); err != nil {
		t.Fatalf("notes should have access to report.txt: %v", err)
	}
	if err := grants.CheckAccess(notesAppID, docDir, vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: notes was granted parent directory access")
	}
	if err := grants.CheckAccess(notesAppID, doc2, vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: notes was granted access to sibling file %s", doc2)
	}

	// 6. Verify recorded in recents
	recentEntries := recents.List("", false)
	if len(recentEntries) != 1 || recentEntries[0].Path != doc1 || recentEntries[0].AppID != notesAppID {
		t.Fatalf("recents not updated properly: %+v", recentEntries)
	}

	// 7. Stale / missing file rejection
	_, err = handoff.Handoff(ctx, filesAppPrincipal, sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    "/users/guest/documents/missing.txt",
		AppID:   notesAppID,
	})
	if err == nil {
		t.Fatalf("expected error for non-existent file")
	}

	// 8. Revocation check: revoke Files' grant and ensure handoff fails closed
	unauthApp := security.AppPrincipal("com.gostalgia.unauthorized", 20, "", security.User{Name: "guest"})
	_, err = handoff.Handoff(ctx, unauthApp, sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    doc1,
		AppID:   notesAppID,
	})
	if err == nil {
		t.Fatalf("expected permission error when caller lacks document access")
	}
}

// #75: an omitted mode must default to read, not read-write — the
// operator's plain "open" cannot silently hand a write grant to whatever
// app resolves the type.
func TestHandoffDefaultsToReadOnly(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	launcher := newMockLauncher()
	dispatcher := newMockDispatcher()
	handoff := NewHandoffManager(v, grants, assoc, recents, launcher, dispatcher)

	doc := "/users/guest/documents/plain.txt"
	_ = v.SaveAtomic(doc, []byte("data"), 0o644)

	res, err := handoff.Handoff(context.Background(),
		security.OperatorPrincipal(security.User{Name: "guest"}),
		sdk.HandoffRequest{Version: sdk.DocumentHandoffVersion, Path: doc})
	if err != nil {
		t.Fatalf("handoff failed: %v", err)
	}
	if res.Mode != string(vfs.AccessRead) {
		t.Fatalf("default mode = %q, want read", res.Mode)
	}
	g, ok := grants.Get(res.GrantID)
	if !ok {
		t.Fatal("handoff grant missing")
	}
	if g.Access != vfs.AccessRead {
		t.Fatalf("grant access = %q, want read", g.Access)
	}
	if err := grants.CheckAccess(g.AppID, doc, vfs.AccessRead); err != nil {
		t.Fatalf("read grant not honored: %v", err)
	}
	if err := grants.CheckAccess(g.AppID, doc, vfs.AccessReadWrite); err == nil {
		t.Fatal("default-mode handoff conferred read-write access")
	}
}

// #76: the scoped grant is issued only after the target launches, and is
// revoked if the open dispatch fails — a failed handoff never leaves a
// live grant.
func TestHandoffGrantRevokedOnFailure(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	launcher := newMockLauncher()
	dispatcher := newMockDispatcher()
	handoff := NewHandoffManager(v, grants, assoc, recents, launcher, dispatcher)

	doc := "/users/guest/documents/leak.txt"
	_ = v.SaveAtomic(doc, []byte("data"), 0o644)
	op := security.OperatorPrincipal(security.User{Name: "guest"})

	// Launch failure: no grant is ever issued.
	launcher.launchErr["com.gostalgia.notes"] = errors.New("boom")
	_, err := handoff.Handoff(context.Background(), op, sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion, Path: doc, AppID: "com.gostalgia.notes",
	})
	if err == nil {
		t.Fatal("handoff to a failing launch succeeded")
	}
	for _, g := range grants.List("com.gostalgia.notes") {
		if !g.Revoked {
			t.Fatalf("launch failure leaked a live grant: %+v", g)
		}
	}

	// Dispatch failure after a successful launch: the issued grant is revoked.
	delete(launcher.launchErr, "com.gostalgia.notes")
	dispatcher.handler = func(req ipc.Request) ipc.Response {
		return ipc.Response{ID: req.ID, OK: false, Error: "app rejected the open"}
	}
	_, err = handoff.Handoff(context.Background(), op, sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion, Path: doc, AppID: "com.gostalgia.notes",
	})
	if err == nil {
		t.Fatal("handoff with a failing dispatch succeeded")
	}
	for _, g := range grants.List("com.gostalgia.notes") {
		if !g.Revoked {
			t.Fatalf("dispatch failure leaked a live grant: %+v", g)
		}
	}
}

// #92: handoff grants are session-bound — they die with the target app's
// run instead of persisting forever.
func TestHandoffGrantIsSessionBound(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	handoff := NewHandoffManager(v, grants, assoc, recents, newMockLauncher(), newMockDispatcher())

	doc := "/users/guest/documents/bound.txt"
	_ = v.SaveAtomic(doc, []byte("data"), 0o644)

	res, err := handoff.Handoff(context.Background(),
		security.OperatorPrincipal(security.User{Name: "guest"}),
		sdk.HandoffRequest{Version: sdk.DocumentHandoffVersion, Path: doc, AppID: "com.gostalgia.notes"})
	if err != nil {
		t.Fatalf("handoff failed: %v", err)
	}
	g, ok := grants.Get(res.GrantID)
	if !ok {
		t.Fatal("handoff grant missing")
	}
	if !g.SessionBound {
		t.Fatal("handoff grant is not session-bound")
	}

	// A standing grant to the same app is not affected by session cleanup.
	stable, err := grants.Issue("com.gostalgia.notes", "/users/guest/config", vfs.AccessRead, true)
	if err != nil {
		t.Fatal(err)
	}
	grants.RevokeAppSession("com.gostalgia.notes")

	if now, _ := grants.Get(res.GrantID); !now.Revoked {
		t.Fatal("session-bound grant not revoked on app exit")
	}
	if err := grants.CheckAccess("com.gostalgia.notes", doc, vfs.AccessRead); err == nil {
		t.Fatal("session-bound grant still authorizes access after app exit")
	}
	if now, _ := grants.Get(stable.ID); now.Revoked {
		t.Fatal("standing grant marked revoked by session cleanup")
	}
	if err := grants.CheckAccess("com.gostalgia.notes", "/users/guest/config", vfs.AccessRead); err != nil {
		t.Fatalf("standing grant was revoked by session cleanup: %v", err)
	}
}

// #268: re-opening one document in a running app reuses the live session
// grant instead of minting a duplicate per handoff, and session teardown
// reaps the records so the store never accumulates them.
func TestHandoffSessionGrantReuseAndReaping(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	launcher := newMockLauncher()
	handoff := NewHandoffManager(v, grants, assoc, recents, launcher, newMockDispatcher())

	doc := "/users/guest/documents/reread.txt"
	_ = v.SaveAtomic(doc, []byte("data"), 0o644)
	op := security.OperatorPrincipal(security.User{Name: "guest"})

	const opens = 200
	var firstGrant string
	for i := 0; i < opens; i++ {
		res, err := handoff.Handoff(context.Background(), op, sdk.HandoffRequest{
			Version: sdk.DocumentHandoffVersion, Path: doc, AppID: "com.gostalgia.notes"})
		if err != nil {
			t.Fatalf("handoff %d failed: %v", i, err)
		}
		if firstGrant == "" {
			firstGrant = res.GrantID
		} else if res.GrantID != firstGrant {
			t.Fatalf("handoff %d minted a duplicate grant for the same document", i)
		}
	}

	live := 0
	for _, g := range grants.List("com.gostalgia.notes") {
		if !g.Revoked {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("after opening one document %d times in a running app: %d live grants for the same path", opens, live)
	}

	grants.RevokeAppSession("com.gostalgia.notes")
	live = 0
	total := 0
	for _, g := range grants.List("com.gostalgia.notes") {
		total++
		if !g.Revoked {
			live++
		}
	}
	if live != 0 {
		t.Fatal("session-bound grant still live after app exit")
	}
	if total > opens/2 {
		t.Fatalf("grants never reaped: store still holds %d records for one document", total)
	}
}

// doc/handoff issues non-recursive single-document grants; a directory
// handoff would mint a grant that cannot describe subtree access honestly,
// so directories are rejected outright.
func TestHandoffRejectsDirectory(t *testing.T) {
	v, grants := setupTestVFS(t)
	assoc := NewAssociationTable()
	recents := NewRecentsStore(v, grants, "/users/guest/config/recents.json")
	handoff := NewHandoffManager(v, grants, assoc, recents, newMockLauncher(), newMockDispatcher())

	_, err := handoff.Handoff(context.Background(),
		security.OperatorPrincipal(security.User{Name: "guest"}),
		sdk.HandoffRequest{Version: sdk.DocumentHandoffVersion, Path: "/users/guest/documents", AppID: "com.gostalgia.files"})
	if err == nil {
		t.Fatal("handoff of a directory succeeded")
	}
	if got := len(grants.List("com.gostalgia.files")); got != 0 {
		t.Fatalf("directory handoff minted %d grants", got)
	}
}

func TestDocumentStoreProfileSwitch(t *testing.T) {
	v := vfs.New(vfs.NewMem())
	_ = v.MkdirAll("/users/guest/documents")
	_ = v.MkdirAll("/users/guest/config")
	_ = v.MkdirAll("/users/alice/documents")
	_ = v.MkdirAll("/users/alice/config")

	_ = v.SaveAtomic("/users/guest/documents/guest_note.txt", []byte("guest"), 0o644)
	_ = v.SaveAtomic("/users/alice/documents/alice_note.txt", []byte("alice"), 0o644)

	store := NewStore(v, v.Grants(), nil, nil)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Add recent for guest
	if _, err := store.Recents().Add("/users/guest/documents/guest_note.txt", ""); err != nil {
		t.Fatal(err)
	}
	if len(store.Recents().List("", false)) != 1 {
		t.Fatalf("expected 1 recent for guest")
	}

	// Switch to alice
	if err := store.SwitchProfile("alice"); err != nil {
		t.Fatal(err)
	}
	// Alice must have 0 recents initially
	if len(store.Recents().List("", false)) != 0 {
		t.Fatalf("expected 0 recents for alice initially")
	}

	// Add recent for alice
	if _, err := store.Recents().Add("/users/alice/documents/alice_note.txt", ""); err != nil {
		t.Fatal(err)
	}
	if len(store.Recents().List("", false)) != 1 {
		t.Fatalf("expected 1 recent for alice")
	}

	// Switch back to guest: guest recent must still be 1, not alice's
	if err := store.SwitchProfile("guest"); err != nil {
		t.Fatal(err)
	}
	guestRecents := store.Recents().List("", false)
	if len(guestRecents) != 1 || guestRecents[0].Path != "/users/guest/documents/guest_note.txt" {
		t.Fatalf("expected guest's own recent, got: %+v", guestRecents)
	}

	// Verify Searcher scoping by caller profile
	aliceCaller := security.OperatorPrincipal(security.User{Name: "alice"})
	res, err := store.Searcher().Search(context.Background(), aliceCaller, sdk.DocumentSearchQuery{Query: "note"})
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range res.Results {
		if strings.Contains(match.Path, "/users/guest") {
			t.Fatalf("alice's search leaked guest document: %s", match.Path)
		}
	}
}
