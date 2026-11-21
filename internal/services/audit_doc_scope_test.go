package services

// Regression tests for GH-4: doc/recents and doc/favorites mutation routes
// checked only fs.write, so an app holding fs.write with no path grants
// could remove any operator entry or wipe both lists outright. App removals
// and clears are now scoped to entries the caller can read — or, for
// recents, entries tagged with the caller's own app id — while operator and
// admin callers keep unrestricted clears.

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

var docWriteCaps = security.NewCapabilities(security.CapIPC, security.CapFileWrite)

func seedDoc(t *testing.T, env *testEnv, p string) {
	t.Helper()
	must(t, env.ctx.VFS.WriteFile(p, []byte("contents of "+p), 0o644))
}

func addRecent(t *testing.T, env *testEnv, p, appID string) {
	t.Helper()
	resp := env.call(context.Background(), adminCaps, "doc/recents/add",
		map[string]any{"path": p, "app_id": appID})
	if !resp.OK {
		t.Fatalf("seed recents/add %s failed: %s", p, resp.Error)
	}
}

func addFavorite(t *testing.T, env *testEnv, p string) {
	t.Helper()
	resp := env.call(context.Background(), adminCaps, "doc/favorites/add",
		map[string]any{"path": p})
	if !resp.OK {
		t.Fatalf("seed favorites/add %s failed: %s", p, resp.Error)
	}
}

func recentsPaths(t *testing.T, env *testEnv) []string {
	t.Helper()
	resp := env.call(context.Background(), adminCaps, "doc/recents", nil)
	if !resp.OK {
		t.Fatalf("doc/recents failed: %s", resp.Error)
	}
	var out struct {
		Entries []sdk.RecentDocument `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	paths := make([]string, 0, len(out.Entries))
	for _, e := range out.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func favoritesPaths(t *testing.T, env *testEnv) []string {
	t.Helper()
	resp := env.call(context.Background(), adminCaps, "doc/favorites", nil)
	if !resp.OK {
		t.Fatalf("doc/favorites failed: %s", resp.Error)
	}
	var out struct {
		Entries []sdk.FavoriteDocument `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	paths := make([]string, 0, len(out.Entries))
	for _, e := range out.Entries {
		paths = append(paths, e.Path)
	}
	return paths
}

func contains(paths []string, p string) bool {
	for _, e := range paths {
		if e == p {
			return true
		}
	}
	return false
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An app with fs.write but no path grants must not remove operator recents
// entries, whether singly or via clear.
func TestAuditRecentsMutationDeniedWithoutGrants(t *testing.T) {
	env := newTestEnv(t)
	docA := "/users/guest/documents/a.txt"
	docB := "/users/guest/documents/b.txt"
	seedDoc(t, env, docA)
	seedDoc(t, env, docB)
	addRecent(t, env, docA, "com.gostalgia.notes")
	addRecent(t, env, docB, "")

	resp := env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/recents/remove", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("recents/remove rejected fs.write app: %s", resp.Error)
	}
	if got := recentsPaths(t, env); !contains(got, docA) || !contains(got, docB) {
		t.Fatalf("BUG: ungranted app removed recents entries, remaining: %v", got)
	}

	resp = env.callAs(context.Background(), evilApp, docWriteCaps, "doc/recents/clear", nil)
	if !resp.OK {
		t.Fatalf("recents/clear rejected fs.write app: %s", resp.Error)
	}
	if got := recentsPaths(t, env); !contains(got, docA) || !contains(got, docB) {
		t.Fatalf("BUG: ungranted app wiped operator recents, remaining: %v", got)
	}
}

// An app may remove and clear only the recents entries it can read, plus
// entries tagged with its own app id even after the grant is gone.
func TestAuditRecentsMutationScopedToCaller(t *testing.T) {
	env := newTestEnv(t)
	docA := "/users/guest/documents/a.txt"
	docB := "/users/guest/documents/b.txt"
	seedDoc(t, env, docA)
	seedDoc(t, env, docB)
	addRecent(t, env, docA, "")
	addRecent(t, env, docB, "")

	v := env.ctx.VFS.(*vfs.VFS)
	grant, err := v.Grants().Issue(evilApp.AppID, docA, vfs.AccessRead, false)
	must(t, err)

	// Removing a granted entry works; the ungranted sibling survives.
	resp := env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/recents/remove", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("recents/remove of granted path failed: %s", resp.Error)
	}
	if got := recentsPaths(t, env); contains(got, docA) || !contains(got, docB) {
		t.Fatalf("scoped recents/remove wrong: %v", got)
	}

	// The app's own tagged entries remain removable after the grant is
	// revoked: it recorded them itself.
	resp = env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/recents/add", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("recents/add of granted path failed: %s", resp.Error)
	}
	must(t, v.Grants().Revoke(grant.ID))
	resp = env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/recents/remove", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("recents/remove of own tagged entry failed: %s", resp.Error)
	}
	if got := recentsPaths(t, env); contains(got, docA) || !contains(got, docB) {
		t.Fatalf("own-entry recents/remove wrong: %v", got)
	}

	// Clear drops only caller-visible entries; operator entries survive.
	_, err = v.Grants().Issue(evilApp.AppID, docA, vfs.AccessRead, false)
	must(t, err)
	resp = env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/recents/add", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("recents/add failed: %s", resp.Error)
	}
	resp = env.callAs(context.Background(), evilApp, docWriteCaps, "doc/recents/clear", nil)
	if !resp.OK {
		t.Fatalf("recents/clear failed: %s", resp.Error)
	}
	if got := recentsPaths(t, env); contains(got, docA) || !contains(got, docB) {
		t.Fatalf("scoped recents/clear wrong: %v", got)
	}

	// Operator clear still wipes everything.
	resp = env.call(context.Background(), adminCaps, "doc/recents/clear", nil)
	if !resp.OK {
		t.Fatalf("operator recents/clear failed: %s", resp.Error)
	}
	if got := recentsPaths(t, env); len(got) != 0 {
		t.Fatalf("operator recents/clear left entries: %v", got)
	}
}

// Favorites mutations get the same treatment: no grants, no removals.
func TestAuditFavoritesMutationDeniedWithoutGrants(t *testing.T) {
	env := newTestEnv(t)
	docA := "/users/guest/documents/a.txt"
	docB := "/users/guest/documents/b.txt"
	seedDoc(t, env, docA)
	seedDoc(t, env, docB)
	addFavorite(t, env, docA)
	addFavorite(t, env, docB)

	resp := env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/favorites/remove", map[string]any{"path": docA})
	if !resp.OK {
		t.Fatalf("favorites/remove rejected fs.write app: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !contains(got, docA) || !contains(got, docB) {
		t.Fatalf("BUG: ungranted app removed favorites, remaining: %v", got)
	}

	resp = env.callAs(context.Background(), evilApp, docWriteCaps, "doc/favorites/clear", nil)
	if !resp.OK {
		t.Fatalf("favorites/clear rejected fs.write app: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !contains(got, docA) || !contains(got, docB) {
		t.Fatalf("BUG: ungranted app wiped operator favorites, remaining: %v", got)
	}
}

// Scoped favorites mutations: remove and clear drop only readable entries,
// and reorder can only rearrange the caller-visible subset — hidden entries
// keep their positions, so naming a single visible path cannot demote the
// operator's other favorites to the bottom.
func TestAuditFavoritesMutationScopedToCaller(t *testing.T) {
	env := newTestEnv(t)
	docA := "/users/guest/documents/a.txt"
	docB := "/users/guest/documents/b.txt"
	docC := "/users/guest/documents/c.txt"
	seedDoc(t, env, docA)
	seedDoc(t, env, docB)
	seedDoc(t, env, docC)
	addFavorite(t, env, docA)
	addFavorite(t, env, docB)
	addFavorite(t, env, docC)

	v := env.ctx.VFS.(*vfs.VFS)
	_, err := v.Grants().Issue(evilApp.AppID, docB, vfs.AccessRead, false)
	must(t, err)
	_, err = v.Grants().Issue(evilApp.AppID, docC, vfs.AccessRead, false)
	must(t, err)

	// Reorder naming only the visible c.txt may swap c and b — both are
	// caller-visible — but the hidden a.txt keeps its rank at the front.
	resp := env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/favorites/reorder", map[string]any{"paths": []string{docC}})
	if !resp.OK {
		t.Fatalf("favorites/reorder rejected fs.write app: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !equalOrder(got, []string{docA, docC, docB}) {
		t.Fatalf("BUG: app reorder moved hidden favorites, order: %v", got)
	}

	// Reordering the visible subset back swaps b and c around the pinned a.
	resp = env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/favorites/reorder", map[string]any{"paths": []string{docB, docC}})
	if !resp.OK {
		t.Fatalf("favorites/reorder failed: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !equalOrder(got, []string{docA, docB, docC}) {
		t.Fatalf("scoped reorder wrong, order: %v", got)
	}

	// Naming a hidden path is ignored, not honored and not an error oracle.
	resp = env.callAs(context.Background(), evilApp, docWriteCaps,
		"doc/favorites/reorder", map[string]any{"paths": []string{docA, docC, docB}})
	if !resp.OK {
		t.Fatalf("favorites/reorder failed: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !equalOrder(got, []string{docA, docC, docB}) {
		t.Fatalf("hidden path influenced reorder, order: %v", got)
	}

	// Clear drops only readable favorites; a.txt survives.
	resp = env.callAs(context.Background(), evilApp, docWriteCaps, "doc/favorites/clear", nil)
	if !resp.OK {
		t.Fatalf("favorites/clear failed: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); !equalOrder(got, []string{docA}) {
		t.Fatalf("scoped favorites/clear wrong: %v", got)
	}

	// Operator reorder and clear remain unrestricted.
	resp = env.call(context.Background(), adminCaps,
		"doc/favorites/reorder", map[string]any{"paths": []string{docA}})
	if !resp.OK {
		t.Fatalf("operator favorites/reorder failed: %s", resp.Error)
	}
	resp = env.call(context.Background(), adminCaps, "doc/favorites/clear", nil)
	if !resp.OK {
		t.Fatalf("operator favorites/clear failed: %s", resp.Error)
	}
	if got := favoritesPaths(t, env); len(got) != 0 {
		t.Fatalf("operator favorites/clear left entries: %v", got)
	}
}
