package services

// Regression for #136: config/unset and config/reset must pin caller
// app_id to the calling app, like config/set and the read routes do.
// Before the fix, any app holding config.write could delete individual
// preferences from another app's layer or wipe every installed app's
// app-layer store.

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/security"
)

func TestAuditConfigUnsetResetForeignAppID(t *testing.T) {
	env := newTestConfigService(t)

	victim := security.AppPrincipal("com.test.victim", 201, "sess-v", security.User{ID: "u-1", Name: "appuser"})
	attacker := security.AppPrincipal("com.test.attacker", 202, "sess-a", security.User{ID: "u-1", Name: "appuser"})
	writeCaps := security.NewCapabilities(security.CapIPC, security.CapConfigWrite)
	readCaps := security.NewCapabilities(security.CapIPC, security.CapConfigRead)

	// Victim stores two app-layer preferences via config/set (already pinned).
	for _, kv := range [][2]string{{"setting.one", "alpha"}, {"setting.two", "beta"}} {
		resp := env.callAs(context.Background(), victim, writeCaps, "config/set",
			map[string]any{"path": kv[0], "value": kv[1], "layer": "app"})
		if !resp.OK {
			t.Fatalf("victim config/set %s: %s", kv[0], resp.Error)
		}
	}

	getVictim := func(path string) getResp {
		t.Helper()
		resp := env.callAs(context.Background(), victim, readCaps, "config/get",
			map[string]any{"path": path})
		if !resp.OK {
			t.Fatalf("victim config/get %s: %s", path, resp.Error)
		}
		var out getResp
		if err := json.Unmarshal(resp.Data, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Part 1: an app must not delete a preference from another app's layer.
	resp := env.callAs(context.Background(), attacker, writeCaps, "config/unset",
		map[string]any{"path": "setting.one", "layer": "app", "app_id": "com.test.victim"})
	if resp.OK {
		t.Fatal("config/unset accepted a foreign app_id")
	}
	if got := getVictim("setting.one"); !got.Found || got.Value != "alpha" {
		t.Fatalf("victim setting.one after foreign unset = %+v, want alpha", got)
	}

	// Part 2: an app must not wipe another app's store by name.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/reset",
		map[string]any{"layer": "app", "app_id": "com.test.victim"})
	if resp.OK {
		t.Fatal("config/reset accepted a foreign app_id")
	}
	if got := getVictim("setting.two"); !got.Found || got.Value != "beta" {
		t.Fatalf("victim setting.two after foreign reset = %+v, want beta", got)
	}

	// Part 3: an app resetting the app layer without app_id only reaches its
	// own store — it must not wipe every installed app's preferences.
	resp = env.callAs(context.Background(), attacker, writeCaps, "config/reset",
		map[string]any{"layer": "app"})
	if !resp.OK {
		t.Fatalf("config/reset of own app layer failed: %s", resp.Error)
	}
	for _, kv := range [][2]string{{"setting.one", "alpha"}, {"setting.two", "beta"}} {
		if got := getVictim(kv[0]); !got.Found || got.Value != kv[1] {
			t.Fatalf("victim %s wiped by app-less reset = %+v, want %s", kv[0], got, kv[1])
		}
	}

	// Operators retain cross-app control of the app layer.
	resp = env.call(context.Background(), security.AdminCapabilities(), "config/unset",
		map[string]any{"path": "setting.two", "layer": "app", "app_id": "com.test.victim"})
	if !resp.OK {
		t.Fatalf("operator unset of victim pref failed: %s", resp.Error)
	}
	if got := getVictim("setting.two"); got.Found {
		t.Fatal("operator unset did not delete victim pref")
	}
	resp = env.call(context.Background(), security.AdminCapabilities(), "config/reset",
		map[string]any{"layer": "app", "app_id": "com.test.victim"})
	if !resp.OK {
		t.Fatalf("operator reset of victim app layer failed: %s", resp.Error)
	}
	if got := getVictim("setting.one"); got.Found {
		t.Fatal("operator reset did not wipe victim app layer")
	}
}
