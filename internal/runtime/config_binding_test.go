package runtime

// Issue #258: profile switch, reboot, and restore must each bind the user
// configuration layer to the active profile's settings file under the
// host root — the same path, through the same mechanism.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gostalgia/internal/ipc"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
)

func adminCtx() context.Context {
	return ipc.WithCapabilities(context.Background(), security.AdminCapabilities())
}

func dispatch(t *testing.T, rt *Runtime, method string, params any) ipc.Response {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return rt.Router.Dispatch(adminCtx(), ipc.Request{ID: 1, Method: method, Params: raw})
}

func settingsHostPath(root, id string) string {
	return filepath.Join(root, "vfs", "users", id, "config", "settings.json")
}

// Switch used to bind the user layer to the literal VFS path
// /users/<id>/config/settings.json — outside the host root — so reads
// ignored the profile's saved settings and writes failed (or landed
// outside the root on a writable host).
func TestProfileSwitchBindsUserStoreHostPath(t *testing.T) {
	root := t.TempDir()
	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer rt.Shutdown("test done")

	if _, err := rt.Profiles.Create("zzreview9", "ZZ Review"); err != nil {
		t.Fatal(err)
	}
	want := settingsHostPath(root, "zzreview9")
	if err := os.WriteFile(want, []byte(`{"theme": "midnight"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if resp := dispatch(t, rt, "profile/switch", map[string]string{"id": "zzreview9"}); !resp.OK {
		t.Fatalf("profile/switch: %s", resp.Error)
	}

	if got := rt.LayeredCfg.UserStore().Path(); got != want {
		t.Fatalf("user store path after profile/switch = %q, want %q", got, want)
	}

	resp := dispatch(t, rt, "config/get", map[string]string{"path": "theme"})
	if !resp.OK {
		t.Fatalf("config/get: %s", resp.Error)
	}
	var getOut struct {
		Value any    `json:"value"`
		Layer string `json:"layer"`
	}
	if err := json.Unmarshal(resp.Data, &getOut); err != nil {
		t.Fatal(err)
	}
	if getOut.Value != "midnight" || getOut.Layer != "user" {
		t.Fatalf("config/get theme after switch = %v (%s), want midnight (user)", getOut.Value, getOut.Layer)
	}

	resp = dispatch(t, rt, "config/set", map[string]any{"path": "accessibility.high_contrast", "value": true})
	if !resp.OK {
		t.Fatalf("config/set after profile/switch: %s", resp.Error)
	}
}

// Boot hardcoded the guest settings path, so a persisted non-guest
// active_id had its settings ignored after every restart and its writes
// landed in guest's settings.json.
func TestRebootBindsActiveProfileUserStore(t *testing.T) {
	root := t.TempDir()
	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if _, err := rt.Profiles.Create("alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Profiles.Switch("alice"); err != nil {
		t.Fatal(err)
	}
	rt.Shutdown("persist active profile")

	rt, err = Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("reboot: %v", err)
	}
	defer rt.Shutdown("test done")

	want := settingsHostPath(root, "alice")
	if got := rt.LayeredCfg.UserStore().Path(); got != want {
		t.Fatalf("user store path after reboot = %q, want %q", got, want)
	}

	resp := dispatch(t, rt, "config/set", map[string]any{"path": "accessibility.high_contrast", "value": true})
	if !resp.OK {
		t.Fatalf("config/set after reboot: %s", resp.Error)
	}
	data, err := os.ReadFile(settingsHostPath(root, "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var alice map[string]any
	if err := json.Unmarshal(data, &alice); err != nil {
		t.Fatal(err)
	}
	acc, _ := alice["accessibility"].(map[string]any)
	if acc["high_contrast"] != true {
		t.Fatalf("alice's settings.json = %s, want high_contrast true", data)
	}
	if data, err := os.ReadFile(settingsHostPath(root, "guest")); err == nil {
		var guest map[string]any
		if json.Unmarshal(data, &guest) == nil && len(guest) > 0 {
			t.Fatalf("guest's settings.json received the active profile's write: %s", data)
		}
	}
}

// backup/restore calls Profiles.Reload, which used to change the active
// profile with no side effects at all: the user config layer, the runtime
// identity, and the operator credential stayed on the old profile.
func TestReloadRebindsUserStoreAndIdentity(t *testing.T) {
	root := t.TempDir()
	rt, err := Boot(context.Background(), Options{Root: root, LogOutput: io.Discard})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	defer rt.Shutdown("test done")

	if _, err := rt.Profiles.Create("alice", "Alice"); err != nil {
		t.Fatal(err)
	}

	// A restored registry stands in for backup/restore rewriting
	// /config/profiles.json: persist active_id=alice through a second
	// manager, then Reload on the live one.
	other, err := profile.NewManager(rt.VFS, "", nil, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Switch("alice"); err != nil {
		t.Fatal(err)
	}
	if err := rt.Profiles.Reload(); err != nil {
		t.Fatal(err)
	}

	want := settingsHostPath(root, "alice")
	if got := rt.LayeredCfg.UserStore().Path(); got != want {
		t.Fatalf("user store path after reload = %q, want %q", got, want)
	}
	if u := rt.CurrentUser(); u.Name != "alice" {
		t.Fatalf("runtime user after reload = %q, want alice", u.Name)
	}
}
