package services

// GH-8: the app layer persists to host disk outside the VFS grant scope,
// yet config/set layer=app performed no capability check at all — an app
// holding only baseline ipc could persist arbitrary data. The app layer
// is persistent storage like the user layer, so it requires config.write.

import (
	"context"
	"testing"

	"gostalgia/internal/security"
)

func TestAuditConfigSetAppLayerRequiresWrite(t *testing.T) {
	env := newTestConfigService(t)
	app := security.AppPrincipal("com.test.app", 100, "sess-1", security.User{ID: "u-1", Name: "appuser"})

	resp := env.callAs(context.Background(), app,
		security.NewCapabilities(security.CapIPC), "config/set",
		map[string]any{"path": "accessibility.mouse", "value": false, "layer": "app"})
	if resp.OK {
		t.Fatal("config/set layer=app accepted an app without config.write")
	}

	resp = env.callAs(context.Background(), app,
		security.NewCapabilities(security.CapIPC, security.CapConfigWrite), "config/set",
		map[string]any{"path": "accessibility.mouse", "value": false, "layer": "app"})
	if !resp.OK {
		t.Fatalf("config/set layer=app with config.write failed: %s", resp.Error)
	}
}

// GH-8: app_id names a file on disk (<user config>/apps/<id>.json), so it
// must be a canonical app id for every principal — an operator-supplied
// identifier with separators or dot segments would escape the apps
// directory.
func TestAuditConfigAppIDValidatedForAllPrincipals(t *testing.T) {
	env := newTestConfigService(t)
	for _, bad := range []string{"..", "../escape", "a/b", `a\b`, "nope", "apps"} {
		resp := env.call(context.Background(), security.AdminCapabilities(), "config/set",
			map[string]any{"path": "accessibility.mouse", "value": false, "layer": "app", "app_id": bad})
		if resp.OK {
			t.Fatalf("config/set accepted app_id %q", bad)
		}
		resp = env.call(context.Background(), security.AdminCapabilities(), "config/unset",
			map[string]any{"path": "accessibility.mouse", "layer": "app", "app_id": bad})
		if resp.OK {
			t.Fatalf("config/unset accepted app_id %q", bad)
		}
		resp = env.call(context.Background(), security.AdminCapabilities(), "config/reset",
			map[string]any{"layer": "app", "app_id": bad})
		if resp.OK {
			t.Fatalf("config/reset accepted app_id %q", bad)
		}
		resp = env.call(context.Background(), security.AdminCapabilities(), "config/get",
			map[string]any{"path": "accessibility.mouse", "layer": "app", "app_id": bad})
		if resp.OK {
			t.Fatalf("config/get accepted app_id %q", bad)
		}
	}

	// Well-formed app ids still work.
	resp := env.call(context.Background(), security.AdminCapabilities(), "config/set",
		map[string]any{"path": "accessibility.mouse", "value": false, "layer": "app", "app_id": "com.test.ok"})
	if !resp.OK {
		t.Fatalf("config/set rejected a valid app_id: %s", resp.Error)
	}
}
