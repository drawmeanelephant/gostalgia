package services

// Regression tests for backup/restore integrity findings (issues #87, #88, #89).

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strings"
	"testing"

	"gostalgia/internal/recovery"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

// unreadablePathFS wraps an FS so ReadFile of one path fails with a permission
// error — a portable way to simulate an unreadable live file (os.Chmod 0o000
// does not block reads on Windows).
type unreadablePathFS struct {
	vfs.FS
	target string
}

func (u *unreadablePathFS) ReadFile(name string) ([]byte, error) {
	if path.Clean("/"+name) == u.target {
		return nil, &vfs.Error{Op: "read", Path: u.target, Code: vfs.ErrPermission, Message: "permission denied"}
	}
	return u.FS.ReadFile(name)
}

// #87: export silently dropped files — user *.zip documents, unreadable files,
// and /apps/data never made the archive while the export reported success.
// Fixed: ordinary user files are exported and every omission is enumerated in
// the export result.
func TestAuditExportSilentlyDropsUserFiles(t *testing.T) {
	env := newTestEnv(t)
	must(t, env.ctx.VFS.MkdirAll("/users/guest/documents"))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/keep.txt", []byte("keep me"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/archive.zip", []byte("PK fake zip"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/photo.gbar", []byte("not a backup"), 0o644))
	must(t, env.ctx.VFS.WriteFile("/users/guest/documents/locked.txt", []byte("cannot read"), 0o644))
	// App-private data lives outside the portable backup scope by policy.
	must(t, env.ctx.VFS.MkdirAll("/apps/data/com.test.app"))
	must(t, env.ctx.VFS.WriteFile("/apps/data/com.test.app/state.json", []byte("{}"), 0o644))

	lockedVFS := &unreadablePathFS{FS: env.ctx.VFS, target: "/users/guest/documents/locked.txt"}

	var buf bytes.Buffer
	res, err := recovery.Export(context.Background(), lockedVFS, &buf, recovery.ExportOptions{})
	must(t, err)

	names := strings.Join(resArchiveFiles(t, buf.Bytes()), ",")
	if !strings.Contains(names, "keep.txt") {
		t.Error("keep.txt missing from export")
	}
	if !strings.Contains(names, "archive.zip") {
		t.Error("archive.zip is ordinary user data and must be exported")
	}
	if strings.Contains(names, "photo.gbar") {
		t.Error("backup-format archive files must not be re-exported")
	}

	// Every omission must be enumerated — no silent loss.
	skipReasons := map[string]string{}
	for _, s := range res.Skipped {
		skipReasons[s.Path] = s.Reason
	}
	for _, want := range []string{
		"/users/guest/documents/photo.gbar",
		"/users/guest/documents/locked.txt",
		"/apps/data",
	} {
		if _, ok := skipReasons[want]; !ok {
			t.Errorf("omission of %q not reported; skipped=%+v", want, res.Skipped)
		}
	}
}

// #146: two backup exports within the same second shared the default
// backup-<unix>.gbar destination and the second silently overwrote the
// first. Fixed: same-second exports land on distinct -N suffixed paths and
// both archives survive.
func TestAuditBackupExportSameSecondCollision(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	caps := security.AdminCapabilities()

	export := func(description string) recovery.ExportResult {
		t.Helper()
		resp := env.call(ctx, caps, "backup/export", map[string]any{"description": description})
		if !resp.OK {
			t.Fatalf("backup/export %q failed: %s", description, resp.Error)
		}
		var res recovery.ExportResult
		must(t, json.Unmarshal(resp.Data, &res))
		return res
	}

	first := export("first-backup")
	second := export("second-backup")
	if first.ArchivePath == "" || second.ArchivePath == "" {
		t.Fatalf("exports missing archive_path: %q / %q", first.ArchivePath, second.ArchivePath)
	}
	if first.ArchivePath == second.ArchivePath {
		t.Fatalf("BUG: same-second exports collided at %s and the FIRST backup was silently overwritten", first.ArchivePath)
	}
	for _, tc := range []struct {
		path        string
		description string
	}{
		{first.ArchivePath, "first-backup"},
		{second.ArchivePath, "second-backup"},
	} {
		data, err := env.ctx.VFS.ReadFile(tc.path)
		must(t, err)
		m, err := recovery.Inspect(bytes.NewReader(data))
		must(t, err)
		if m.Description != tc.description {
			t.Fatalf("%s description = %q, want %q", tc.path, m.Description, tc.description)
		}
	}
}

func resArchiveFiles(t *testing.T, data []byte) []string {
	t.Helper()
	m, err := recovery.Inspect(bytes.NewReader(data))
	must(t, err)
	var names []string
	for _, f := range m.Files {
		names = append(names, f.VFSPath)
	}
	return names
}

// #88: restore treated any live ReadFile error as "file does not exist", so an
// unreadable existing file was planned as a create — overwritten with no
// conflict check and no rollback journal entry for its content. Fixed:
// non-NotExist read errors hard-fail under every conflict strategy.
func TestAuditRestoreOverwritesUnreadableFile(t *testing.T) {
	env := newTestEnv(t)
	target := "/users/guest/documents/locked.txt"
	must(t, env.ctx.VFS.MkdirAll("/users/guest/documents"))
	must(t, env.ctx.VFS.WriteFile(target, []byte("ORIGINAL-SECRET"), 0o644))
	lockedVFS := &unreadablePathFS{FS: env.ctx.VFS, target: target}

	archive := buildArchive(t, target, "REPLACED")

	// Preview cannot classify the live file either — it must fail loudly.
	if _, err := recovery.Preview(bytes.NewReader(archive), lockedVFS); err == nil {
		t.Error("preview classified an unreadable live file as a create")
	}

	for _, strategy := range []recovery.ConflictStrategy{
		recovery.ConflictAbort, recovery.ConflictSkip, recovery.ConflictOverwrite,
	} {
		if _, err := recovery.Restore(context.Background(), bytes.NewReader(archive), lockedVFS,
			recovery.RestoreOptions{Strategy: strategy}); err == nil {
			t.Errorf("strategy %s: restore succeeded over an unreadable live file", strategy)
		}
	}

	got, err := env.ctx.VFS.ReadFile(target)
	must(t, err)
	if string(got) != "ORIGINAL-SECRET" {
		t.Fatalf("content = %q, want ORIGINAL-SECRET preserved", got)
	}
}

// #89: restore's profile filter only filtered /users/* paths — /config/* files
// were written regardless, so a "profile-scoped" restore overwrote global
// state. Fixed: with ProfileFilter set, every entry outside /users/<profile>/
// is skipped and counted.
func TestAuditRestoreProfileFilterBypass(t *testing.T) {
	env := newTestEnv(t)
	must(t, env.ctx.VFS.WriteFile("/config/system.json", []byte(`{"original":true}`), 0o644))
	archive := buildArchiveMulti(t, map[string]string{
		"/config/system.json":                `{"attacker":true}`,
		"/users/guest/documents/note.txt":    "hello",
		"/users/other/documents/private.txt": "not yours",
	})
	rep, err := recovery.Restore(context.Background(), bytes.NewReader(archive), env.ctx.VFS,
		recovery.RestoreOptions{Strategy: recovery.ConflictOverwrite, ProfileFilter: "guest"})
	must(t, err)
	if rep.RestoredCount != 1 {
		t.Errorf("RestoredCount = %d, want 1 (only the guest file)", rep.RestoredCount)
	}
	if rep.SkippedCount != 2 {
		t.Errorf("SkippedCount = %d, want 2 (config + other profile filtered out)", rep.SkippedCount)
	}
	got, err := env.ctx.VFS.ReadFile("/config/system.json")
	must(t, err)
	if strings.Contains(string(got), "attacker") {
		t.Fatal("profile-filtered restore overwrote /config/*")
	}
	if _, err := env.ctx.VFS.Stat("/users/other/documents/private.txt"); err == nil {
		t.Fatal("profile-filtered restore wrote another profile's files")
	}
	if got, err := env.ctx.VFS.ReadFile("/users/guest/documents/note.txt"); err != nil || string(got) != "hello" {
		t.Fatalf("profile's own file not restored: %q, %v", got, err)
	}
}

// buildArchive produces a minimal valid .gbar with one manifest entry.
func buildArchive(t *testing.T, vfsPath, content string) []byte {
	t.Helper()
	return buildArchiveMulti(t, map[string]string{vfsPath: content})
}

// buildArchiveMulti produces a minimal valid .gbar from a vfsPath -> content map.
func buildArchiveMulti(t *testing.T, files map[string]string) []byte {
	t.Helper()
	man := recovery.Manifest{FormatVersion: recovery.FormatVersion}
	payloads := map[string][]byte{}
	for vfsPath, content := range files {
		arcName := recovery.DataDirPrefix + strings.TrimPrefix(vfsPath, "/")
		man.Files = append(man.Files, recovery.FileEntry{
			VFSPath: vfsPath, ArcName: arcName,
			Size: int64(len(content)), SHA256: recovery.ComputeSHA256([]byte(content)),
		})
		payloads[arcName] = []byte(content)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	manifestBytes, err := json.Marshal(man)
	must(t, err)
	payloads[recovery.ManifestFilename] = manifestBytes
	for name, body := range payloads {
		w, err := zw.Create(name)
		must(t, err)
		_, err = w.Write(body)
		must(t, err)
	}
	must(t, zw.Close())
	return buf.Bytes()
}
