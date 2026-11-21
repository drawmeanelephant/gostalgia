package services

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gostalgia/internal/profile"
	"gostalgia/internal/recovery"
	"gostalgia/internal/security"
	"gostalgia/internal/session"
)

// #273: a restore that reports success must also land in the live stores —
// config layers, workspace history, recents/favorites, and the profile
// switch path all cache whole documents and would otherwise keep (and
// later re-write) stale state.
func TestRestoreReloadsLiveStores(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	admin := security.AdminCapabilities()

	// Prime the caches the way a running session does.
	sessions := env.ctx.Sessions.ForUser("guest")
	if len(sessions) == 0 {
		t.Fatal("no guest session")
	}
	ws := env.ctx.Sessions.Workspace(sessions[0].ID)
	if ws == nil {
		t.Fatal("workspace store unavailable")
	}
	must(t, ws.AppendHistory("pre-restore-cmd"))
	resp := env.call(ctx, admin, "config/set", map[string]any{
		"path": "notifications.dnd", "value": true,
	})
	if !resp.OK {
		t.Fatalf("config/set failed: %s", resp.Error)
	}

	// The archive restores different settings and a different workspace
	// history than the live caches currently hold.
	archive := buildArchiveMulti(t, map[string]string{
		"/users/guest/config/settings.json":  `{"accessibility":{"reduced_motion":true}}`,
		"/users/guest/config/workspace.json": `{"history":["restored-cmd"]}`,
	})
	must(t, env.ctx.VFS.WriteFile("/users/guest/downloads/restore.gbar", archive, 0o644))

	resp = env.call(ctx, admin, "backup/restore", map[string]string{
		"path": "/users/guest/downloads/restore.gbar", "strategy": "overwrite",
	})
	if !resp.OK {
		t.Fatalf("backup/restore failed: %s", resp.Error)
	}
	var rep recovery.RestoreReport
	must(t, json.Unmarshal(resp.Data, &rep))
	if rep.RestoredCount == 0 {
		t.Fatal("restore reported zero restored files")
	}
	if len(rep.Reloaded) == 0 {
		t.Fatal("restore report does not record live-store reloads")
	}

	// Restored setting is visible through config/get...
	resp = env.call(ctx, admin, "config/get", map[string]string{
		"path": "accessibility.reduced_motion",
	})
	if !resp.OK {
		t.Fatalf("config/get failed: %s", resp.Error)
	}
	var gotVal struct {
		Value any `json:"value"`
	}
	must(t, json.Unmarshal(resp.Data, &gotVal))
	if gotVal.Value != true {
		t.Fatalf("restored setting not visible through config/get: %v", gotVal.Value)
	}

	// ...and survives the next unrelated config/set instead of being
	// clobbered by the stale cached document.
	resp = env.call(ctx, admin, "config/set", map[string]any{
		"path": "notifications.toast_limit", "value": 5,
	})
	if !resp.OK {
		t.Fatalf("config/set after restore failed: %s", resp.Error)
	}
	raw, err := env.ctx.VFS.ReadFile("/users/guest/config/settings.json")
	must(t, err)
	var persisted map[string]any
	must(t, json.Unmarshal(raw, &persisted))
	if acc, ok := persisted["accessibility"].(map[string]any); !ok || acc["reduced_motion"] != true {
		t.Fatalf("restored setting clobbered by post-restore write: %s", raw)
	}

	// Restored workspace history is loaded, not overwritten.
	state := ws.Get()
	found := false
	for _, h := range state.History {
		if h == "restored-cmd" {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored workspace history not live: %+v", state.History)
	}
	must(t, ws.AppendHistory("next-cmd"))
	raw, err = env.ctx.VFS.ReadFile("/users/guest/config/workspace.json")
	must(t, err)
	var wsDoc session.WorkspaceState
	must(t, json.Unmarshal(raw, &wsDoc))
	found = false
	for _, h := range wsDoc.History {
		if h == "restored-cmd" {
			found = true
		}
	}
	if !found {
		t.Fatalf("post-restore history write lost the restored entry: %+v", wsDoc.History)
	}
}

// #273: when a restore changes the active profile, Profiles.Reload must
// run the same transition as Switch — hooks fire and the switched event is
// published, so runtime identity and per-profile stores rebind.
func TestRestoreActiveProfileRunsSwitchPath(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	admin := security.AdminCapabilities()

	// Register the same kind of hook the runtime installs.
	var switched struct {
		prev, next string
	}
	env.ctx.Profiles.OnSwitch(func(prev, next profile.Profile) {
		switched.prev, switched.next = prev.ID, next.ID
	})

	registry := `{"version":1,"active_id":"alice","profiles":{` +
		`"guest":{"id":"guest","name":"Guest","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"},` +
		`"alice":{"id":"alice","name":"Alice","created_at":"2024-01-01T00:00:00Z","updated_at":"2024-01-01T00:00:00Z"}}}`
	archive := buildArchiveMulti(t, map[string]string{
		"/config/profiles.json": registry,
	})
	must(t, env.ctx.VFS.WriteFile("/users/guest/downloads/switch.gbar", archive, 0o644))

	resp := env.call(ctx, admin, "backup/restore", map[string]string{
		"path": "/users/guest/downloads/switch.gbar", "strategy": "overwrite",
	})
	if !resp.OK {
		t.Fatalf("backup/restore failed: %s", resp.Error)
	}
	if got := env.ctx.Profiles.ActiveID(); got != "alice" {
		t.Fatalf("active profile = %q, want alice", got)
	}
	if switched.prev != "guest" || switched.next != "alice" {
		t.Fatalf("switch hooks did not run on restored profile change: %+v", switched)
	}
}

// #273/#275: a restored /config/system.json reaches the live host system
// store and the config layer reloads it — config/get layer=system shows
// the restored value.
func TestRestoreSystemConfigReachesLiveStore(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	admin := security.AdminCapabilities()

	// Set a live system preference first.
	resp := env.call(ctx, admin, "config/set", map[string]any{
		"path": "notifications.dnd", "value": true, "layer": "system",
	})
	if !resp.OK {
		t.Fatalf("config/set system failed: %s", resp.Error)
	}

	// Export must carry the live system store, not VFS dead bytes.
	resp = env.call(ctx, admin, "backup/export", map[string]string{
		"path": "/users/guest/downloads/sys.gbar",
	})
	if !resp.OK {
		t.Fatalf("backup/export failed: %s", resp.Error)
	}
	data, err := env.ctx.VFS.ReadFile("/users/guest/downloads/sys.gbar")
	must(t, err)
	va, err := recovery.ParseArchive(bytes.NewReader(data))
	must(t, err)
	sysDoc, ok := va.Files["data/config/system.json"]
	if !ok {
		t.Fatal("system.json missing from exported backup")
	}
	if !strings.Contains(string(sysDoc), `"dnd":true`) {
		t.Fatalf("exported system.json missing live setting: %s", sysDoc)
	}

	// Restore it into a different value and confirm the live store changes.
	archive := buildArchiveMulti(t, map[string]string{
		"/config/system.json": `{"notifications":{"dnd":false,"toast_limit":9}}`,
	})
	must(t, env.ctx.VFS.WriteFile("/users/guest/downloads/sysin.gbar", archive, 0o644))
	resp = env.call(ctx, admin, "backup/restore", map[string]string{
		"path": "/users/guest/downloads/sysin.gbar", "strategy": "overwrite",
	})
	if !resp.OK {
		t.Fatalf("backup/restore failed: %s", resp.Error)
	}

	hostRaw, err := os.ReadFile(filepath.Join(env.ctx.Root, "config", "system.json"))
	must(t, err)
	if !strings.Contains(string(hostRaw), `"toast_limit":9`) {
		t.Fatalf("restored system.json not in live host store: %s", hostRaw)
	}

	resp = env.call(ctx, admin, "config/get", map[string]any{
		"path": "notifications.toast_limit", "layer": "system",
	})
	if !resp.OK {
		t.Fatalf("config/get failed: %s", resp.Error)
	}
	var gotVal struct {
		Value any `json:"value"`
	}
	must(t, json.Unmarshal(resp.Data, &gotVal))
	if gotVal.Value != float64(9) {
		t.Fatalf("restored system value not live: %v", gotVal.Value)
	}
}
