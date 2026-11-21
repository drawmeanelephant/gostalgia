package services

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

// Items trashed from shared host mounts carry the hostfs policy and the
// hostfs.* capabilities with them: an app holding only fs.read/fs.write must
// not be able to list, restore, or purge them.
func TestTrashOpsEnforceHostFSCapability(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	tempHostDir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(tempHostDir, "secret.txt"), []byte("host secret"), 0o644))

	operator := security.OperatorPrincipal(security.User{Name: "admin"})
	capsAdmin := security.AdminCapabilities()
	appPrinc := security.AppPrincipal("com.test.app", 10, "sess-1", security.User{Name: "guest"})
	fsCaps := security.NewCapabilities(security.CapFileRead, security.CapFileWrite)
	hostCaps := security.NewCapabilities(security.CapFileRead, security.CapFileWrite,
		security.CapHostFSRead, security.CapHostFSWrite)

	policy := env.ctx.Policy.Get()
	policy.HostFS.Enabled = true
	policy.HostFS.AllowRead = true
	policy.HostFS.AllowWrite = true
	policy.HostFS.AllowedPaths = []string{tempHostDir}
	env.ctx.Policy.Set(policy)

	res := env.callAs(ctx, operator, capsAdmin, "hostfs/mount", map[string]any{
		"host_path":  tempHostDir,
		"mount_path": "/mnt/shared",
		"read_only":  false,
	})
	if !res.OK {
		t.Fatalf("hostfs/mount failed: %s", res.Error)
	}
	t.Cleanup(func() {
		_ = env.callAs(ctx, operator, capsAdmin, "hostfs/unmount", map[string]string{
			"mount_path": "/mnt/shared",
		})
	})

	// The operator trashes a file off the shared host mount.
	res = env.callAs(ctx, operator, capsAdmin, "fs/trash", map[string]string{
		"path": "/mnt/shared/secret.txt",
	})
	if !res.OK {
		t.Fatalf("fs/trash of shared-mount file failed: %s", res.Error)
	}
	var trashEntry vfs.TrashEntry
	must(t, json.Unmarshal(res.Data, &trashEntry))

	// An app with only fs.* must not see the entry in fs/trash/list.
	res = env.callAs(ctx, appPrinc, fsCaps, "fs/trash/list", nil)
	if !res.OK {
		t.Fatalf("fs/trash/list failed: %s", res.Error)
	}
	var listOut struct {
		Entries []vfs.TrashEntry `json:"entries"`
	}
	must(t, json.Unmarshal(res.Data, &listOut))
	for _, e := range listOut.Entries {
		if e.ID == trashEntry.ID {
			t.Fatal("fs/trash/list revealed a shared-host item to an app without hostfs.read")
		}
	}

	// Nor restore it into its own private storage.
	res = env.callAs(ctx, appPrinc, fsCaps, "fs/restore", map[string]any{
		"id":        trashEntry.ID,
		"dst":       "/apps/data/com.test.app/stolen.txt",
		"overwrite": true,
	})
	if res.OK {
		t.Fatal("fs/restore moved a shared-host item for an app without hostfs.write")
	}
	if _, err := env.ctx.VFS.Stat("/apps/data/com.test.app/stolen.txt"); err == nil {
		t.Fatal("shared-host payload landed in app-private storage")
	}

	// Nor purge it.
	res = env.callAs(ctx, appPrinc, fsCaps, "fs/trash/empty", nil)
	if !res.OK {
		t.Fatalf("fs/trash/empty failed: %s", res.Error)
	}
	var emptyOut struct {
		Count int `json:"count"`
	}
	must(t, json.Unmarshal(res.Data, &emptyOut))
	if emptyOut.Count != 0 {
		t.Fatalf("fs/trash/empty purged %d items for an app without hostfs.write", emptyOut.Count)
	}

	// An app holding hostfs.* alongside fs.* keeps full access.
	res = env.callAs(ctx, appPrinc, hostCaps, "fs/trash/list", nil)
	if !res.OK {
		t.Fatalf("fs/trash/list with hostfs caps failed: %s", res.Error)
	}
	must(t, json.Unmarshal(res.Data, &listOut))
	found := false
	for _, e := range listOut.Entries {
		if e.ID == trashEntry.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("fs/trash/list hid a shared-host item from a hostfs-capable app")
	}
	res = env.callAs(ctx, appPrinc, hostCaps, "fs/restore", map[string]any{
		"id":        trashEntry.ID,
		"dst":       "/mnt/shared/secret.txt",
		"overwrite": true,
	})
	if !res.OK {
		t.Fatalf("fs/restore with hostfs caps failed: %s", res.Error)
	}
	hostBytes, err := os.ReadFile(filepath.Join(tempHostDir, "secret.txt"))
	if err != nil || string(hostBytes) != "host secret" {
		t.Fatalf("restored host file = %q err=%v", hostBytes, err)
	}
}

// The trash directory follows the active profile: items trashed under one
// profile are stored, listed, and die inside that profile's own tree.
func TestTrashFollowsActiveProfile(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	admin := security.AdminCapabilities()

	res := env.call(ctx, admin, "profile/create", map[string]string{
		"id":   "alice",
		"name": "Alice",
	})
	if !res.OK {
		t.Fatalf("profile/create failed: %s", res.Error)
	}
	res = env.call(ctx, admin, "profile/switch", map[string]string{"id": "alice"})
	if !res.OK {
		t.Fatalf("profile/switch failed: %s", res.Error)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("alice only"))
	res = env.call(ctx, admin, "fs/write", map[string]string{
		"path":        "/users/alice/documents/journal.txt",
		"data_base64": payload,
	})
	if !res.OK {
		t.Fatalf("fs/write failed: %s", res.Error)
	}
	res = env.call(ctx, admin, "fs/trash", map[string]string{
		"path": "/users/alice/documents/journal.txt",
	})
	if !res.OK {
		t.Fatalf("fs/trash failed: %s", res.Error)
	}
	var entry vfs.TrashEntry
	must(t, json.Unmarshal(res.Data, &entry))
	if !strings.HasPrefix(entry.TrashPath, "/users/alice/.trash/files/") {
		t.Fatalf("alice's file trashed to %q, outside her profile trash", entry.TrashPath)
	}

	// Switch back to guest: alice's trashed item must not appear.
	res = env.call(ctx, admin, "profile/switch", map[string]string{"id": "guest"})
	if !res.OK {
		t.Fatalf("profile/switch back failed: %s", res.Error)
	}
	res = env.call(ctx, admin, "fs/trash/list", nil)
	if !res.OK {
		t.Fatalf("fs/trash/list failed: %s", res.Error)
	}
	var listOut struct {
		Entries []vfs.TrashEntry `json:"entries"`
	}
	must(t, json.Unmarshal(res.Data, &listOut))
	for _, e := range listOut.Entries {
		if e.ID == entry.ID {
			t.Fatal("alice's trashed item is visible in guest's trash")
		}
	}

	// Deleting alice removes her tree; nothing may linger in guest's bin.
	res = env.call(ctx, admin, "profile/delete", map[string]string{"id": "alice"})
	if !res.OK {
		t.Fatalf("profile/delete failed: %s", res.Error)
	}
	res = env.call(ctx, admin, "fs/trash/list", nil)
	if !res.OK {
		t.Fatalf("fs/trash/list after delete failed: %s", res.Error)
	}
	must(t, json.Unmarshal(res.Data, &listOut))
	if len(listOut.Entries) != 0 {
		t.Fatalf("deleted profile's trash survived in guest's bin: %+v", listOut.Entries)
	}
}
