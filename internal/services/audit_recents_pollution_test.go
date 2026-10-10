package services

// Regression tests for #138: doc/recents/add and doc/favorites/add used to
// stat, persist, and index the requested path BEFORE any grant check — the
// response was redacted (#82) but the shared per-user store file and the
// search index still absorbed ungranted paths with real metadata. Adds for
// paths outside the caller's grants must be ignored entirely.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gostalgia/internal/security"
	"gostalgia/sdk"
)

func TestAuditRecentsAddPollutesSharedStore(t *testing.T) {
	env := newTestEnv(t)
	target := "/apps/data/com.other.app/credentials.json"
	must(t, env.ctx.VFS.MkdirAll("/apps/data/com.other.app"))
	must(t, env.ctx.VFS.WriteFile(target, []byte("sekrit-value"), 0o644))

	writeCaps := security.NewCapabilities(security.CapIPC, security.CapFileWrite)
	resp := env.callAs(context.Background(), evilApp, writeCaps,
		"doc/recents/add", map[string]any{"path": target})
	if resp.OK {
		var entry sdk.RecentDocument
		must(t, json.Unmarshal(resp.Data, &entry))
		if entry.Exists || entry.Size != 0 || entry.ModTime != "" {
			t.Fatalf("recents/add returned real metadata for an ungranted path: %+v", entry)
		}
	}

	// The operator-visible recents list must not contain the path at all.
	resp = env.call(context.Background(), adminCaps, "doc/recents",
		map[string]any{"verify_exists": true})
	if !resp.OK {
		t.Fatalf("doc/recents failed: %s", resp.Error)
	}
	var out struct {
		Entries []sdk.RecentDocument `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	for _, e := range out.Entries {
		if e.Path == target {
			t.Fatalf("BUG: operator recents list contains ungranted path %s (exists=%v size=%d)",
				target, e.Exists, e.Size)
		}
	}

	// Nor may the shared store file persist it.
	if data, err := env.ctx.VFS.ReadFile("/users/guest/config/recents.json"); err == nil &&
		strings.Contains(string(data), target) {
		t.Fatal("BUG: recents.json persisted an ungranted path")
	}

	// Nor the shared search index.
	resp = env.call(context.Background(), adminCaps, "doc/lookup",
		map[string]any{"query": "credentials"})
	if !resp.OK {
		t.Fatalf("doc/lookup failed: %s", resp.Error)
	}
	var l sdk.DocumentSearchResponse
	must(t, json.Unmarshal(resp.Data, &l))
	for _, r := range l.Results {
		if r.Path == target {
			t.Fatalf("BUG: search index contains ungranted path %s", target)
		}
	}
}

func TestAuditFavoritesAddPollutesSharedStore(t *testing.T) {
	env := newTestEnv(t)
	target := "/apps/data/com.other.app/credentials.json"
	must(t, env.ctx.VFS.MkdirAll("/apps/data/com.other.app"))
	must(t, env.ctx.VFS.WriteFile(target, []byte("sekrit-value"), 0o644))

	writeCaps := security.NewCapabilities(security.CapIPC, security.CapFileWrite)
	resp := env.callAs(context.Background(), evilApp, writeCaps,
		"doc/favorites/add", map[string]any{"path": target, "label": "loot"})
	if resp.OK {
		var entry sdk.FavoriteDocument
		must(t, json.Unmarshal(resp.Data, &entry))
		if entry.Exists || entry.Size != 0 || entry.ModTime != "" {
			t.Fatalf("favorites/add returned real metadata for an ungranted path: %+v", entry)
		}
	}

	resp = env.call(context.Background(), adminCaps, "doc/favorites",
		map[string]any{"verify_exists": true})
	if !resp.OK {
		t.Fatalf("doc/favorites failed: %s", resp.Error)
	}
	var out struct {
		Entries []sdk.FavoriteDocument `json:"entries"`
	}
	must(t, json.Unmarshal(resp.Data, &out))
	for _, e := range out.Entries {
		if e.Path == target {
			t.Fatalf("BUG: operator favorites list contains ungranted path %s (exists=%v size=%d)",
				target, e.Exists, e.Size)
		}
	}

	if data, err := env.ctx.VFS.ReadFile("/users/guest/config/favorites.json"); err == nil &&
		strings.Contains(string(data), target) {
		t.Fatal("BUG: favorites.json persisted an ungranted path")
	}

	resp = env.call(context.Background(), adminCaps, "doc/lookup",
		map[string]any{"query": "credentials"})
	if !resp.OK {
		t.Fatalf("doc/lookup failed: %s", resp.Error)
	}
	var l sdk.DocumentSearchResponse
	must(t, json.Unmarshal(resp.Data, &l))
	for _, r := range l.Results {
		if r.Path == target {
			t.Fatalf("BUG: search index contains ungranted path %s", target)
		}
	}
}
