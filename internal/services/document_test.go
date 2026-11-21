package services

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

func TestDocumentAssociationsIPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()

	// 1. List associations
	resp := env.call(ctx, adminCaps, "doc/associations", nil)
	if !resp.OK {
		t.Fatalf("doc/associations failed: %s", resp.Error)
	}
	var listOut struct {
		Associations []sdk.DocumentTypeAssociation `json:"associations"`
		Total        int                           `json:"total"`
	}
	must(t, json.Unmarshal(resp.Data, &listOut))
	if listOut.Total == 0 {
		t.Fatalf("expected associations, got 0")
	}

	// 2. Resolve .txt
	resp = env.call(ctx, adminCaps, "doc/associations/resolve", map[string]string{
		"path": "/users/guest/documents/sample.txt",
	})
	if !resp.OK {
		t.Fatalf("doc/associations/resolve failed: %s", resp.Error)
	}
	var resolveOut struct {
		DefaultApp string                        `json:"default_app"`
		Handlers   []sdk.DocumentTypeAssociation `json:"handlers"`
		Found      bool                          `json:"found"`
	}
	must(t, json.Unmarshal(resp.Data, &resolveOut))
	if !resolveOut.Found || resolveOut.DefaultApp != "com.gostalgia.notes" {
		t.Fatalf("expected com.gostalgia.notes, got %+v", resolveOut)
	}

	// 3. Register custom association
	resp = env.call(ctx, adminCaps, "doc/associations/register", sdk.DocumentTypeAssociation{
		Extension: ".xyz",
		AppID:     "com.example.xyz",
		Name:      "XYZ Format",
		Default:   true,
	})
	if !resp.OK {
		t.Fatalf("register failed: %s", resp.Error)
	}

	resp = env.call(ctx, adminCaps, "doc/associations/resolve", map[string]string{"path": "file.xyz"})
	must(t, json.Unmarshal(resp.Data, &resolveOut))
	if !resolveOut.Found || resolveOut.DefaultApp != "com.example.xyz" {
		t.Fatalf("expected com.example.xyz, got %+v", resolveOut)
	}
}

func TestDocumentRecentsAndFavoritesIPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)

	docPath := "/users/guest/documents/hello.txt"
	must(t, dfs.SaveAtomic(docPath, []byte("hello"), 0o644))

	// 1. Recents: add
	resp := env.call(ctx, adminCaps, "doc/recents/add", map[string]string{
		"path":   docPath,
		"app_id": "com.gostalgia.notes",
	})
	if !resp.OK {
		t.Fatalf("recents/add failed: %s", resp.Error)
	}

	// 2. Recents: list
	resp = env.call(ctx, adminCaps, "doc/recents", map[string]bool{"verify_exists": true})
	if !resp.OK {
		t.Fatalf("recents failed: %s", resp.Error)
	}
	var recentsOut struct {
		Entries []sdk.RecentDocument `json:"entries"`
		Total   int                  `json:"total"`
	}
	must(t, json.Unmarshal(resp.Data, &recentsOut))
	if recentsOut.Total != 1 || recentsOut.Entries[0].Path != docPath || !recentsOut.Entries[0].Exists {
		t.Fatalf("unexpected recents: %+v", recentsOut)
	}

	// 3. Favorites: add
	resp = env.call(ctx, adminCaps, "doc/favorites/add", map[string]string{
		"path":  docPath,
		"label": "My Hello Doc",
	})
	if !resp.OK {
		t.Fatalf("favorites/add failed: %s", resp.Error)
	}

	// 4. Favorites: list
	resp = env.call(ctx, adminCaps, "doc/favorites", nil)
	if !resp.OK {
		t.Fatalf("favorites failed: %s", resp.Error)
	}
	var favsOut struct {
		Entries []sdk.FavoriteDocument `json:"entries"`
		Total   int                    `json:"total"`
	}
	must(t, json.Unmarshal(resp.Data, &favsOut))
	if favsOut.Total != 1 || favsOut.Entries[0].Label != "My Hello Doc" {
		t.Fatalf("unexpected favorites: %+v", favsOut)
	}

	// 5. Caller app permission filtering:
	// App without grant calling doc/recents should see 0 entries
	unauthApp := security.AppPrincipal("com.example.unauth", 99, "", security.User{Name: "guest"})
	resp = env.callAs(ctx, unauthApp, security.NewCapabilities(security.CapIPC), "doc/recents", nil)
	if !resp.OK {
		t.Fatalf("doc/recents as app failed: %s", resp.Error)
	}
	must(t, json.Unmarshal(resp.Data, &recentsOut))
	if recentsOut.Total != 0 {
		t.Fatalf("expected 0 recents for unauthorized app, got %d", recentsOut.Total)
	}

	// 6. Recents remove & clear
	resp = env.call(ctx, adminCaps, "doc/recents/remove", map[string]string{"path": docPath})
	if !resp.OK {
		t.Fatalf("recents/remove failed: %s", resp.Error)
	}
	resp = env.call(ctx, adminCaps, "doc/recents", nil)
	must(t, json.Unmarshal(resp.Data, &recentsOut))
	if recentsOut.Total != 0 {
		t.Fatalf("expected 0 recents after remove")
	}
}

func TestDocumentSearchAndLookupIPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)

	f1 := "/users/guest/documents/plan.txt"
	f2 := "/users/guest/documents/budget.csv"
	must(t, dfs.SaveAtomic(f1, []byte("project plan"), 0o644))
	must(t, dfs.SaveAtomic(f2, []byte("cost,dollars"), 0o644))

	// 1. Search operator
	resp := env.call(ctx, adminCaps, "doc/search", sdk.DocumentSearchQuery{
		Path:      "/users/guest/documents",
		Extension: ".txt",
	})
	if !resp.OK {
		t.Fatalf("search failed: %s", resp.Error)
	}
	var sResp sdk.DocumentSearchResponse
	must(t, json.Unmarshal(resp.Data, &sResp))
	if sResp.Total != 1 || sResp.Results[0].Path != f1 {
		t.Fatalf("expected plan.txt, got %+v", sResp)
	}

	// 2. App search with permission check
	appPrincipal := security.AppPrincipal("com.gostalgia.app1", 88, "", security.User{Name: "guest"})
	// Ungranted directory search should fail
	resp = env.callAs(ctx, appPrincipal, nil, "doc/search", sdk.DocumentSearchQuery{
		Path: "/users/guest/documents",
	})
	if resp.OK {
		t.Fatalf("expected permission error for ungranted app search")
	}

	// Grant access to /users/guest/documents
	v := env.ctx.VFS.(*vfs.VFS)
	_, err := v.Grants().Issue("com.gostalgia.app1", "/users/guest/documents", vfs.AccessRead, true)
	must(t, err)

	// Now app search succeeds
	resp = env.callAs(ctx, appPrincipal, nil, "doc/search", sdk.DocumentSearchQuery{
		Path: "/users/guest/documents",
	})
	if !resp.OK {
		t.Fatalf("app search with grant failed: %s", resp.Error)
	}
	must(t, json.Unmarshal(resp.Data, &sResp))
	if sResp.Total != 2 {
		t.Fatalf("expected 2 documents in search, got %d", sResp.Total)
	}

	// 3. Lookup indexed documents
	resp = env.call(ctx, adminCaps, "doc/lookup", map[string]any{
		"query": "budget",
	})
	if !resp.OK {
		t.Fatalf("lookup failed: %s", resp.Error)
	}
	var lResp sdk.DocumentSearchResponse
	must(t, json.Unmarshal(resp.Data, &lResp))
	// In testEnv, lookup index was seeded during Init; let's index doc explicitly if needed
	_ = env.call(ctx, adminCaps, "doc/recents/add", map[string]string{"path": f2})
	resp = env.call(ctx, adminCaps, "doc/lookup", map[string]any{"query": "budget"})
	must(t, json.Unmarshal(resp.Data, &lResp))
	if lResp.Total != 1 || lResp.Results[0].Path != f2 {
		t.Fatalf("expected budget.csv in lookup, got %+v", lResp)
	}
}

func TestDocumentHandoffIPC(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)
	v := env.ctx.VFS.(*vfs.VFS)

	targetDoc := "/users/guest/documents/secret_memo.txt"
	otherDoc := "/users/guest/documents/other_memo.txt"
	must(t, dfs.SaveAtomic(targetDoc, []byte("confidential content"), 0o644))
	must(t, dfs.SaveAtomic(otherDoc, []byte("other content"), 0o644))

	// Grant Files access to /users/guest/documents
	_, err := v.Grants().Issue("com.gostalgia.files", "/users/guest/documents", vfs.AccessReadWrite, true)
	must(t, err)

	filesPrincipal := security.AppPrincipal("com.gostalgia.files", 77, "", security.User{Name: "guest"})

	// 1. Files hands off targetDoc to Notes
	resp := env.callAs(ctx, filesPrincipal, security.NewCapabilities(security.CapIPC), "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    targetDoc,
		AppID:   "com.gostalgia.notes",
		Mode:    "read-write",
	})
	if !resp.OK {
		t.Fatalf("doc/handoff failed: %s", resp.Error)
	}
	var hResult sdk.HandoffResult
	must(t, json.Unmarshal(resp.Data, &hResult))
	if !hResult.Success || hResult.Path != targetDoc || hResult.AppID != "com.gostalgia.notes" {
		t.Fatalf("unexpected handoff result: %+v", hResult)
	}

	// 2. Verify Notes is now running and opened targetDoc
	if !env.ctx.Apps.IsRunning("com.gostalgia.notes") {
		t.Fatalf("expected Notes app to be running after handoff")
	}

	// 3. VERIFY GRANT SCOPING: Notes must ONLY have grant for targetDoc, NOT otherDoc or directory!
	if err := v.Grants().CheckAccess("com.gostalgia.notes", targetDoc, vfs.AccessReadWrite); err != nil {
		t.Fatalf("Notes should have access to targetDoc: %v", err)
	}
	if err := v.Grants().CheckAccess("com.gostalgia.notes", otherDoc, vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: Notes was granted access to sibling file %s", otherDoc)
	}
	if err := v.Grants().CheckAccess("com.gostalgia.notes", "/users/guest/documents", vfs.AccessRead); err == nil {
		t.Fatalf("SECURITY VIOLATION: Notes was granted access to parent directory")
	}

	// 4. Stale/missing file handoff returns error
	resp = env.call(ctx, adminCaps, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    "/users/guest/documents/nonexistent.txt",
	})
	if resp.OK {
		t.Fatalf("expected error handing off missing file")
	}
	if !strings.Contains(resp.Error, "not found") {
		t.Fatalf("expected not found error, got %s", resp.Error)
	}
}

// #82: doc/recents and doc/favorites mutation routes require fs.write, and
// recents/add must not return stat metadata for paths outside the caller's
// grants (existence/size/mtime oracle).
// (PoC: internal/services/audit_findings_test.go on the audit branch.)
func TestAuditRecentsMetadataOracle(t *testing.T) {
	env := newTestEnv(t)
	evilApp := security.AppPrincipal("com.test.evil", 7, "", security.User{Name: "guest"})
	ipcOnly := security.NewCapabilities(security.CapIPC)
	must(t, env.ctx.VFS.MkdirAll("/users/guest/documents"))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/secret.txt", []byte("hidden"), 0o644))
	secret := "/users/guest/documents/secret.txt"

	// Baseline ipc is not enough for any recents/favorites mutation route.
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"doc/recents/add", map[string]any{"path": secret}},
		{"doc/recents/remove", map[string]any{"path": secret}},
		{"doc/recents/clear", nil},
		{"doc/favorites/add", map[string]any{"path": secret}},
		{"doc/favorites/remove", map[string]any{"path": secret}},
		{"doc/favorites/reorder", map[string]any{"paths": []string{secret}}},
		{"doc/favorites/clear", nil},
	} {
		resp := env.callAs(context.Background(), evilApp, ipcOnly, tc.method, tc.params)
		if resp.OK {
			t.Fatalf("%s accepted an app with only 'ipc' cap", tc.method)
		}
	}

	// An app holding fs.write may record recents, but the response must not
	// reveal exists/size/mtime for a path outside its grants.
	writeCaps := security.NewCapabilities(security.CapIPC, security.CapFileWrite)
	resp := env.callAs(context.Background(), evilApp, writeCaps,
		"doc/recents/add", map[string]any{"path": secret})
	if !resp.OK {
		t.Fatalf("recents/add rejected an app with fs.write: %s", resp.Error)
	}
	var entry struct {
		Path    string `json:"path"`
		Exists  bool   `json:"exists"`
		Size    int64  `json:"size"`
		ModTime string `json:"mod_time"`
	}
	must(t, json.Unmarshal(resp.Data, &entry))
	if entry.Exists || entry.Size != 0 || entry.ModTime != "" {
		t.Fatalf("stat metadata leaked for ungranted path: %+v", entry)
	}

	resp = env.callAs(context.Background(), evilApp, writeCaps,
		"doc/favorites/add", map[string]any{"path": secret})
	if !resp.OK {
		t.Fatalf("favorites/add rejected an app with fs.write: %s", resp.Error)
	}
	must(t, json.Unmarshal(resp.Data, &entry))
	if entry.Exists || entry.Size != 0 || entry.ModTime != "" {
		t.Fatalf("stat metadata leaked for ungranted path: %+v", entry)
	}

	// With a read grant, the app sees real metadata again.
	v := env.ctx.VFS.(*vfs.VFS)
	_, err := v.Grants().Issue("com.test.evil", "/users/guest/documents", vfs.AccessRead, true)
	must(t, err)
	resp = env.callAs(context.Background(), evilApp, writeCaps,
		"doc/recents/add", map[string]any{"path": secret})
	if !resp.OK {
		t.Fatalf("recents/add with grant failed: %s", resp.Error)
	}
	must(t, json.Unmarshal(resp.Data, &entry))
	if !entry.Exists || entry.Size == 0 {
		t.Fatalf("expected real metadata for granted path, got %+v", entry)
	}
}

// #92: handoff grants are bound to the target app's run — stopping the
// app revokes them, while install-time/operator grants survive.
func TestDocumentHandoffGrantRevokedOnAppExit(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	adminCaps := security.AdminCapabilities()
	dfs := env.ctx.VFS.(vfs.DocumentFS)
	v := env.ctx.VFS.(*vfs.VFS)

	doc := "/users/guest/documents/ephemeral.txt"
	must(t, dfs.SaveAtomic(doc, []byte("temp content"), 0o644))

	// A standing (non-session) grant to the same app must survive.
	must(t, env.ctx.VFS.MkdirAll("/users/guest/config"))
	stable, err := v.Grants().Issue("com.gostalgia.notes", "/users/guest/config", vfs.AccessRead, true)
	must(t, err)

	resp := env.call(ctx, adminCaps, "doc/handoff", sdk.HandoffRequest{
		Version: sdk.DocumentHandoffVersion,
		Path:    doc,
		AppID:   "com.gostalgia.notes",
	})
	if !resp.OK {
		t.Fatalf("doc/handoff failed: %s", resp.Error)
	}
	var hResult sdk.HandoffResult
	must(t, json.Unmarshal(resp.Data, &hResult))
	if hResult.GrantID == "" {
		t.Fatal("handoff returned no grant id")
	}
	if !env.ctx.Apps.IsRunning("com.gostalgia.notes") {
		t.Fatal("notes was not launched by handoff")
	}
	if g, ok := v.Grants().Get(hResult.GrantID); !ok || !g.SessionBound || g.Revoked {
		t.Fatalf("handoff grant = %+v (ok=%v), want live session-bound grant", g, ok)
	}

	must(t, env.ctx.Apps.Stop("com.gostalgia.notes", 5*time.Second))

	if g, ok := v.Grants().Get(hResult.GrantID); !ok || !g.Revoked {
		t.Fatalf("handoff grant = %+v after app exit, want revoked", g)
	}
	if err := v.Grants().CheckAccess("com.gostalgia.notes", doc, vfs.AccessRead); err == nil {
		t.Fatal("handoff grant still authorizes access after target app exit")
	}
	if g, _ := v.Grants().Get(stable.ID); g.Revoked {
		t.Fatal("standing grant marked revoked on app exit")
	}
	if err := v.Grants().CheckAccess("com.gostalgia.notes", "/users/guest/config", vfs.AccessRead); err != nil {
		t.Fatalf("standing grant revoked by app exit: %v", err)
	}
}
