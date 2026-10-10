package services

// Round-2 audit regression (#177): the #136 fix pinned the app_id
// PARAMETER on config/unset and config/reset, but raw paths into the
// shared user layer still crossed the app boundary. The user-layer
// representation of an app's preferences (the "apps.<appID>.*" subtree,
// which valueLocked resolves as that app's LayerApp values) was readable,
// writable, and deletable by any app holding config.read/config.write,
// and config/list disclosed every app's subtree in bulk.
//
// configuration.md states "applications may only address their own app
// layer". Application callers are now confined to "apps.<own-id>[.*]";
// every other path beneath "apps" is rejected, and snapshots filter
// foreign subtrees.

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/security"
)

func TestAudit2CrossAppRawPathBypass(t *testing.T) {
	env := newTestConfigService(t)

	victim := security.AppPrincipal("com.test.victim", 201, "sess-v", security.User{ID: "u-1", Name: "appuser"})
	attacker := security.AppPrincipal("com.test.attacker", 202, "sess-a", security.User{ID: "u-1", Name: "appuser"})
	writeCaps := security.NewCapabilities(security.CapIPC, security.CapConfigWrite)
	readCaps := security.NewCapabilities(security.CapIPC, security.CapConfigRead)

	// The victim seeds its own preference in the user-layer app
	// representation — the alternate app-layer location that valueLocked
	// resolves for LayerApp reads. (Writing its own namespace is allowed.)
	resp := env.callAs(context.Background(), victim, writeCaps, "config/set",
		map[string]any{"path": "apps.com.test.victim.secret", "value": "s3cret", "layer": "user"})
	if !resp.OK {
		t.Fatalf("victim seed set: %s", resp.Error)
	}

	// Sanity: the key is part of the victim's app layer — an app-scoped
	// read of "secret" resolves it.
	victimEffective := func() getResp {
		t.Helper()
		resp := env.callAs(context.Background(), victim, readCaps, "config/get",
			map[string]any{"path": "secret", "layer": "app"})
		if !resp.OK {
			t.Fatalf("victim get: %s", resp.Error)
		}
		var out getResp
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := victimEffective(); !got.Found || got.Value != "s3cret" {
		t.Fatalf("seed: victim effective secret = %+v, want s3cret", got)
	}

	// Part 1: cross-app READ via raw paths is rejected — the foreign leaf,
	// the foreign subtree root, the shared "apps" root, partial app-ID
	// prefixes, and explain.
	for _, path := range []string{
		"apps.com.test.victim.secret",
		"apps.com.test.victim",
		"apps.com.test",
		"apps.com",
		"apps",
		"apps.com.test.victimx.secret",
	} {
		resp := env.callAs(context.Background(), attacker, readCaps, "config/get",
			map[string]any{"path": path})
		if resp.OK {
			t.Errorf("attacker config/get %q succeeded", path)
		}
	}
	resp = env.callAs(context.Background(), attacker, readCaps, "config/explain",
		map[string]any{"path": "apps.com.test.victim.secret"})
	if resp.OK {
		t.Error("attacker config/explain on foreign path succeeded")
	}

	// Part 2: cross-app WRITE via raw path + layer "user" is rejected; the
	// victim's value is untouched.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/set",
		map[string]any{"path": "apps.com.test.victim.secret", "value": "hacked", "layer": "user"})
	if resp.OK {
		t.Error("attacker config/set on foreign path succeeded")
	}
	if got := victimEffective(); !got.Found || got.Value != "s3cret" {
		t.Errorf("victim secret after foreign set = %+v, want s3cret", got)
	}

	// Staging a foreign path as a preview override is likewise rejected —
	// both the single-path and batch forms.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/preview",
		map[string]any{"path": "apps.com.test.victim.secret", "value": "hacked"})
	if resp.OK {
		t.Error("attacker config/preview on foreign path succeeded")
	}
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/preview",
		map[string]any{"settings": map[string]any{
			"theme": "midnight",
			"apps":  map[string]any{"com": map[string]any{"test": map[string]any{"victim": map[string]any{"secret": "hacked"}}}},
		}})
	if resp.OK {
		t.Error("attacker config/preview batch with foreign path succeeded")
	}

	// Part 3: cross-app DELETE via config/unset and config/reset layer
	// "user" — the routes #136 pinned; the raw-path form is rejected too.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/unset",
		map[string]any{"path": "apps.com.test.victim.secret", "layer": "user"})
	if resp.OK {
		t.Error("attacker config/unset on foreign path succeeded")
	}
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/reset",
		map[string]any{"path": "apps.com.test.victim.secret", "layer": "user"})
	if resp.OK {
		t.Error("attacker config/reset on foreign path succeeded")
	}
	if got := victimEffective(); !got.Found || got.Value != "s3cret" {
		t.Errorf("victim secret after foreign unset/reset = %+v, want s3cret", got)
	}

	// Part 4: config/list confines the "apps" subtree to the caller's own
	// namespace in both the Effective tree and the user-layer snapshot.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/set",
		map[string]any{"path": "apps.com.test.attacker.token", "value": "mine", "layer": "user"})
	if !resp.OK {
		t.Fatalf("attacker seed of own namespace: %s", resp.Error)
	}
	resp = env.callAs(context.Background(), attacker, readCaps, "config/list", nil)
	if !resp.OK {
		t.Fatalf("attacker list: %s", resp.Error)
	}
	var listOut struct {
		Effective map[string]any `json:"effective"`
		Layers    map[string]any `json:"layers"`
	}
	if err := json.Unmarshal(resp.Data, &listOut); err != nil {
		t.Fatal(err)
	}
	for name, tree := range map[string]map[string]any{"effective": listOut.Effective, "layers.user": nil} {
		if name == "layers.user" {
			raw, ok := listOut.Layers["user"].(map[string]any)
			if !ok {
				t.Fatalf("list response missing user layer: %v", listOut.Layers)
			}
			tree = raw
		}
		if subtree, ok := nestedMap(tree, "apps", "com", "test", "victim"); ok {
			t.Errorf("attacker list %s disclosed victim subtree: %v", name, subtree)
		}
		subtree, ok := nestedMap(tree, "apps", "com", "test", "attacker")
		if !ok || subtree["token"] != "mine" {
			t.Errorf("attacker list %s missing own subtree, apps = %v", name, tree["apps"])
		}
	}

	// Apps still address their own namespace through the raw-path form,
	// and operators retain full access.
	resp = env.callAs(context.Background(), victim, readCaps, "config/get",
		map[string]any{"path": "apps.com.test.victim.secret"})
	if !resp.OK {
		t.Fatalf("victim get of own path: %s", resp.Error)
	}
	var own getResp
	if err := json.Unmarshal(resp.Data, &own); err != nil {
		t.Fatal(err)
	}
	if !own.Found || own.Value != "s3cret" {
		t.Fatalf("victim get of own path = %+v, want s3cret", own)
	}
	resp = env.call(context.Background(), security.AdminCapabilities(), "config/get",
		map[string]any{"path": "apps.com.test.victim.secret"})
	if !resp.OK {
		t.Fatalf("operator get of victim path: %s", resp.Error)
	}
}

func nestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}
