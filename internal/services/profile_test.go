package services

import (
	"context"
	"encoding/json"
	"testing"

	"gostalgia/internal/profile"
	"gostalgia/internal/security"
)

func TestProfileService_LifecycleAndSwitch(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	caps := security.AdminCapabilities()

	// 1. Check initial active profile (guest)
	resp := env.call(ctx, caps, "profile/active", nil)
	if !resp.OK {
		t.Fatalf("profile/active failed: %s", resp.Error)
	}
	var activeProf profile.Profile
	if err := json.Unmarshal(resp.Data, &activeProf); err != nil {
		t.Fatal(err)
	}
	if activeProf.ID != "guest" {
		t.Fatalf("expected active profile guest, got %q", activeProf.ID)
	}

	// 2. List profiles
	resp = env.call(ctx, caps, "profile/list", nil)
	if !resp.OK {
		t.Fatalf("profile/list failed: %s", resp.Error)
	}
	var listResp ProfileListResponse
	if err := json.Unmarshal(resp.Data, &listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Profiles) != 1 || listResp.Active != "guest" {
		t.Fatalf("unexpected profile list: %+v", listResp)
	}

	// 3. Create new profile 'developer'
	resp = env.call(ctx, caps, "profile/create", CreateProfileRequest{
		ID:          "developer",
		Name:        "Dev User",
		Description: "Software development workspace",
		Preferences: map[string]any{"editor": "vim", "theme": "monochrome"},
	})
	if !resp.OK {
		t.Fatalf("profile/create failed: %s", resp.Error)
	}
	var created profile.Profile
	if err := json.Unmarshal(resp.Data, &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "developer" || created.Name != "Dev User" {
		t.Fatalf("unexpected created profile: %+v", created)
	}

	// 4. Update profile
	resp = env.call(ctx, caps, "profile/update", UpdateProfileRequest{
		ID:          "developer",
		Name:        "Lead Developer",
		Description: "Senior software engineer workspace",
	})
	if !resp.OK {
		t.Fatalf("profile/update failed: %s", resp.Error)
	}
	var updated profile.Profile
	if err := json.Unmarshal(resp.Data, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Lead Developer" || updated.Description != "Senior software engineer workspace" {
		t.Fatalf("unexpected updated profile: %+v", updated)
	}

	// 5. Switch to 'developer'
	resp = env.call(ctx, caps, "profile/switch", SwitchProfileRequest{ID: "developer"})
	if !resp.OK {
		t.Fatalf("profile/switch failed: %s", resp.Error)
	}
	var switched profile.Profile
	if err := json.Unmarshal(resp.Data, &switched); err != nil {
		t.Fatal(err)
	}
	if switched.ID != "developer" {
		t.Fatalf("expected switched profile developer, got %q", switched.ID)
	}

	// Verify active profile is now developer
	resp = env.call(ctx, caps, "profile/active", nil)
	if !resp.OK {
		t.Fatalf("profile/active failed: %s", resp.Error)
	}
	_ = json.Unmarshal(resp.Data, &activeProf)
	if activeProf.ID != "developer" {
		t.Fatalf("expected active profile developer, got %q", activeProf.ID)
	}

	// 6. Delete active profile must be rejected
	resp = env.call(ctx, caps, "profile/delete", DeleteProfileRequest{ID: "developer"})
	if resp.OK {
		t.Fatalf("expected deletion of active profile to fail")
	}

	// 7. Switch back to guest
	resp = env.call(ctx, caps, "profile/switch", SwitchProfileRequest{ID: "guest"})
	if !resp.OK {
		t.Fatalf("profile/switch to guest failed: %s", resp.Error)
	}

	// 8. Delete developer profile should now succeed
	resp = env.call(ctx, caps, "profile/delete", DeleteProfileRequest{ID: "developer"})
	if !resp.OK {
		t.Fatalf("profile/delete failed: %s", resp.Error)
	}

	// 9. Verify developer profile is deleted
	resp = env.call(ctx, caps, "profile/get", map[string]any{"id": "developer"})
	if resp.OK {
		t.Fatalf("expected profile/get for deleted profile to fail")
	}
}

func TestProfileService_CapabilityChecks(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Caller without read cap
	readOnlyCaps := security.NewCapabilities(security.CapProfileRead)
	writeOnlyCaps := security.NewCapabilities(security.CapProfileWrite)
	noCaps := security.NewCapabilities()

	// profile/list requires profile.read
	resp := env.call(ctx, noCaps, "profile/list", nil)
	if resp.OK {
		t.Fatalf("expected profile/list to fail without read capability")
	}
	resp = env.call(ctx, readOnlyCaps, "profile/list", nil)
	if !resp.OK {
		t.Fatalf("profile/list failed with read capability: %s", resp.Error)
	}

	// profile/create requires profile.write
	resp = env.call(ctx, readOnlyCaps, "profile/create", CreateProfileRequest{
		ID:   "tester",
		Name: "Tester",
	})
	if resp.OK {
		t.Fatalf("expected profile/create to fail with only read capability")
	}

	resp = env.call(ctx, writeOnlyCaps, "profile/create", CreateProfileRequest{
		ID:   "tester",
		Name: "Tester",
	})
	if !resp.OK {
		t.Fatalf("profile/create failed with write capability: %s", resp.Error)
	}

	// profile/switch requires profile.write
	resp = env.call(ctx, readOnlyCaps, "profile/switch", SwitchProfileRequest{ID: "tester"})
	if resp.OK {
		t.Fatalf("expected profile/switch to fail with only read capability")
	}

	resp = env.call(ctx, writeOnlyCaps, "profile/switch", SwitchProfileRequest{ID: "tester"})
	if !resp.OK {
		t.Fatalf("profile/switch failed with write capability: %s", resp.Error)
	}
}

// #158: profile/delete must remove the profile's /users/<id>/ data tree,
// not just its registry entry — operator deletion is expected to erase the
// departed profile's private data.
func TestProfileService_DeleteRemovesUserTree(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	caps := security.AdminCapabilities()

	resp := env.call(ctx, caps, "profile/create", CreateProfileRequest{ID: "departed", Name: "Departed"})
	if !resp.OK {
		t.Fatalf("profile/create failed: %s", resp.Error)
	}
	resp = env.call(ctx, caps, "fs/write", map[string]string{
		"path": "/users/departed/documents/private.txt", "data_base64": "c2VjcmV0",
	})
	if !resp.OK {
		t.Fatalf("fs/write failed: %s", resp.Error)
	}

	resp = env.call(ctx, caps, "profile/delete", DeleteProfileRequest{ID: "departed"})
	if !resp.OK {
		t.Fatalf("profile/delete failed: %s", resp.Error)
	}
	if _, err := env.ctx.VFS.Stat("/users/departed"); err == nil {
		t.Fatal("/users/departed still exists after profile/delete")
	}
}

// #74: profile/* must not accept the baseline ipc cap — an app with only
// 'ipc' can no longer enumerate, create, switch, update, or delete profiles.
// (PoC: internal/services/audit_findings_test.go on the audit branch.)
func TestAuditProfileSwitchByApp(t *testing.T) {
	env := newTestEnv(t)
	evilApp := security.AppPrincipal("com.test.evil", 7, "", security.User{Name: "guest"})
	ipcOnly := security.NewCapabilities(security.CapIPC)
	before := env.ctx.Profiles.ActiveID()
	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		{"profile/list", nil},
		{"profile/active", nil},
		{"profile/create", map[string]any{"id": "pwned", "name": "Pwned"}},
		{"profile/update", map[string]any{"id": before, "name": "Pwned"}},
		{"profile/switch", map[string]any{"id": "pwned"}},
		{"profile/delete", map[string]any{"id": before}},
	} {
		resp := env.callAs(context.Background(), evilApp, ipcOnly, tc.method, tc.params)
		if resp.OK {
			t.Fatalf("%s accepted an app with only 'ipc' cap", tc.method)
		}
	}
	if got := env.ctx.Profiles.ActiveID(); got != before {
		t.Fatalf("active profile = %q, want %q", got, before)
	}
}
