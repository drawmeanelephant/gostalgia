package services

// Regression tests for #137: every backup/* handler ran against the
// unscoped root VFS, so an app holding only backup.read/backup.write could
// exfiltrate the whole environment (export), probe arbitrary paths
// (inspect), read live hashes of ungranted files (preview), and write
// anywhere a backup manifest points (restore). Backup operations must run
// through the caller's grant-scoped VFS view.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gostalgia/internal/recovery"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

// backup/export must only gather paths covered by the caller's grants and
// only write the archive where the caller may write.
func TestAuditBackupExportExfiltratesEntireVFS(t *testing.T) {
	env := newTestEnv(t)
	must(t, env.ctx.VFS.MkdirAll("/users/other/documents"))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/secret.txt", []byte("hidden"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/granted.txt", []byte("ok"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/users/other/documents/private.txt", []byte("not yours"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/config/system.json", []byte(`{"system":true}`), 0o644))

	backupWrite := security.NewCapabilities(security.CapIPC, security.CapBackupWrite)
	gs := env.ctx.VFS.(*vfs.VFS).Grants()

	// The app may stage the archive inside its own private tree, but the
	// archive must contain nothing outside its grants.
	resp := env.callAs(context.Background(), evilApp, backupWrite, "backup/export", map[string]any{
		"path": "/apps/data/com.test.evil/exfil.gbar",
	})
	if resp.OK {
		var res recovery.ExportResult
		must(t, json.Unmarshal(resp.Data, &res))
		data, err := env.ctx.VFS.ReadFile(res.ArchivePath)
		must(t, err)
		man, err := recovery.Inspect(bytes.NewReader(data))
		must(t, err)
		for _, f := range man.Files {
			if err := gs.CheckAccess(evilApp.AppID, f.VFSPath, vfs.AccessRead); err != nil {
				t.Fatalf("BUG: backup/export copied ungranted path %s into the caller-readable archive", f.VFSPath)
			}
		}
	}

	// An ungranted destination must be refused outright.
	resp = env.callAs(context.Background(), evilApp, backupWrite, "backup/export", map[string]any{
		"path": "/users/other/downloads/steal.gbar",
	})
	if resp.OK {
		t.Fatal("BUG: backup/export wrote an archive to an ungranted destination")
	}

	// With a real grant the export covers exactly the granted scope.
	_, err := gs.Issue(evilApp.AppID, "/users/guest/documents", vfs.AccessRead, true)
	must(t, err)
	resp = env.callAs(context.Background(), evilApp, backupWrite, "backup/export", map[string]any{
		"path":       "/apps/data/com.test.evil/scoped.gbar",
		"profile_id": "guest",
	})
	if !resp.OK {
		t.Fatalf("backup/export with a read grant failed: %s", resp.Error)
	}
	var res recovery.ExportResult
	must(t, json.Unmarshal(resp.Data, &res))
	data, err := env.ctx.VFS.ReadFile(res.ArchivePath)
	must(t, err)
	man, err := recovery.Inspect(bytes.NewReader(data))
	must(t, err)
	if len(man.Files) == 0 {
		t.Fatal("export with a grant produced an empty archive")
	}
	for _, f := range man.Files {
		if err := gs.CheckAccess(evilApp.AppID, f.VFSPath, vfs.AccessRead); err != nil {
			t.Fatalf("BUG: backup/export included ungranted path %s despite scoped caller", f.VFSPath)
		}
	}
}

// backup/inspect must not read archives outside the caller's grants.
func TestAuditBackupInspectExistenceOracle(t *testing.T) {
	env := newTestEnv(t)
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/note.txt", []byte("note"), 0o644))

	resp := env.call(context.Background(), adminCaps, "backup/export", map[string]any{
		"path": "/users/guest/downloads/operator.gbar",
	})
	if !resp.OK {
		t.Fatalf("operator export failed: %s", resp.Error)
	}

	backupRead := security.NewCapabilities(security.CapIPC, security.CapBackupRead)
	resp = env.callAs(context.Background(), evilApp, backupRead, "backup/inspect",
		map[string]any{"path": "/users/guest/downloads/operator.gbar"})
	if resp.OK {
		t.Fatal("BUG: backup/inspect read an archive outside the caller's grants")
	}

	// An archive inside the app's own private tree remains inspectable.
	must(t, env.ctx.VFS.WriteFile(
		"/apps/data/com.test.evil/own.gbar",
		buildArchive(t, "/users/guest/documents/note.txt", "note"),
		0o644))
	resp = env.callAs(context.Background(), evilApp, backupRead, "backup/inspect",
		map[string]any{"path": "/apps/data/com.test.evil/own.gbar"})
	if !resp.OK {
		t.Fatalf("backup/inspect of an app-owned archive failed: %s", resp.Error)
	}
}

// backup/preview must not disclose live file state (hashes, sizes,
// existence) for paths outside the caller's grants.
func TestAuditBackupPreviewHashOracle(t *testing.T) {
	env := newTestEnv(t)
	secret := "/users/guest/documents/secret.txt"
	live := []byte("live-secret-content")
	must(t, env.ctx.VFS.WriteFile(secret, live, 0o644))
	liveHash := recovery.ComputeSHA256(live)

	// The archive claims different content for the same path, so a leak lands
	// in Conflicts carrying LiveSHA256/LiveSize.
	must(t, env.ctx.VFS.WriteFile(
		"/apps/data/com.test.evil/probe.gbar",
		buildArchive(t, secret, "attacker-known-different"),
		0o644))

	backupRead := security.NewCapabilities(security.CapIPC, security.CapBackupRead)
	resp := env.callAs(context.Background(), evilApp, backupRead, "backup/preview",
		map[string]any{"path": "/apps/data/com.test.evil/probe.gbar"})
	if resp.OK {
		var report recovery.PreviewReport
		must(t, json.Unmarshal(resp.Data, &report))
		for _, c := range report.Conflicts {
			if c.LiveSHA256 == liveHash || c.LiveSize == int64(len(live)) {
				t.Fatalf("BUG: backup/preview disclosed live sha256=%s size=%d of ungranted path %s",
					c.LiveSHA256, c.LiveSize, c.VFSPath)
			}
		}
		for _, p := range append(report.Create, report.Identical...) {
			if p == secret {
				t.Fatalf("BUG: backup/preview classified ungranted path %s", p)
			}
		}
	} else if strings.Contains(resp.Error, liveHash) {
		t.Fatal("BUG: backup/preview error leaked the live content hash")
	}
}

// backup/restore must not create or overwrite paths outside the caller's
// grants — holding backup.write alone is not an fs grant.
func TestAuditBackupRestoreWritesWithoutFsGrant(t *testing.T) {
	env := newTestEnv(t)
	must(t, env.ctx.VFS.WriteFile("/config/system.json", []byte(`{"original":true}`), 0o644))
	must(t, env.ctx.VFS.WriteFile(
		"/apps/data/com.test.evil/restore.gbar",
		buildArchiveMulti(t, map[string]string{
			"/users/guest/documents/pwned.txt": "pwned",
			"/config/system.json":              `{"attacker":true}`,
		}),
		0o644))

	backupWrite := security.NewCapabilities(security.CapIPC, security.CapBackupWrite)
	resp := env.callAs(context.Background(), evilApp, backupWrite, "backup/restore", map[string]any{
		"path":     "/apps/data/com.test.evil/restore.gbar",
		"strategy": string(recovery.ConflictOverwrite),
	})
	if resp.OK {
		t.Fatal("BUG: backup/restore applied archive entries outside the caller's grants")
	}
	if _, err := env.ctx.VFS.Stat("/users/guest/documents/pwned.txt"); err == nil {
		t.Fatal("BUG: backup/restore created an ungranted path")
	}
	got, err := env.ctx.VFS.ReadFile("/config/system.json")
	must(t, err)
	if strings.Contains(string(got), "attacker") {
		t.Fatal("BUG: backup/restore overwrote /config/system.json")
	}

	// A read-write grant on the target tree permits a scoped restore.
	gs := env.ctx.VFS.(*vfs.VFS).Grants()
	_, err = gs.Issue(evilApp.AppID, "/users/guest/documents", vfs.AccessReadWrite, true)
	must(t, err)
	must(t, env.ctx.VFS.WriteFile(
		"/apps/data/com.test.evil/ok.gbar",
		buildArchive(t, "/users/guest/documents/ok.txt", "restored"),
		0o644))
	resp = env.callAs(context.Background(), evilApp, backupWrite, "backup/restore", map[string]any{
		"path":     "/apps/data/com.test.evil/ok.gbar",
		"strategy": string(recovery.ConflictOverwrite),
	})
	if !resp.OK {
		t.Fatalf("backup/restore within granted scope failed: %s", resp.Error)
	}
	got, err = env.ctx.VFS.ReadFile("/users/guest/documents/ok.txt")
	must(t, err)
	if string(got) != "restored" {
		t.Fatalf("granted restore wrote %q, want restored", got)
	}
}
