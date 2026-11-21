package recovery_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gostalgia/internal/recovery"
	"gostalgia/internal/vfs"
)

// #272: export previously reported success for archives its own parser
// refused — a highly compressible file tripped the zip-bomb ratio guard.
// Export must emit the payload in a form ParseArchive accepts.
func TestExportCompressiblePayloadStaysValid(t *testing.T) {
	v := newTestVFS(t)
	// ~256 KiB of one repeated line compresses far beyond the 100x ratio
	// ParseArchive enforces for deflated entries.
	content := bytes.Repeat([]byte("the same line of text\n"), 12*1024)
	if err := v.WriteFile("/users/guest/documents/compressible.txt", content, 0o644); err != nil {
		t.Fatal(err)
	}

	buf := new(bytes.Buffer)
	res, err := recovery.Export(context.Background(), v, buf, recovery.ExportOptions{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.TotalFiles != 1 {
		t.Fatalf("expected 1 exported file, got %d", res.TotalFiles)
	}
	if _, err := recovery.ParseArchive(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("exported archive rejected by own parser: %v", err)
	}
	if _, err := recovery.Inspect(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("exported archive rejected by inspect: %v", err)
	}
}

// #272: export must budget the manifest descriptor; with enough files the
// serialized manifest exceeds MaxManifestBytes and the produced archive is
// unusable. Export must refuse instead of reporting success.
func TestExportRefusesOversizedManifest(t *testing.T) {
	v := newTestVFS(t)
	// ~3200 files with long names pushes backup.json past the 1 MiB budget
	// while staying under the 5000-file ceiling.
	for i := 0; i < 3201; i++ {
		name := strings.Repeat("x", 190) + "-" + strconv.Itoa(i) + ".txt"
		if err := v.WriteFile("/users/guest/documents/"+name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	buf := new(bytes.Buffer)
	if _, err := recovery.Export(context.Background(), v, buf, recovery.ExportOptions{}); err == nil {
		t.Fatal("export reported success for a backup whose manifest exceeds the budget")
	} else if !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("expected manifest budget error, got: %v", err)
	}
}

// #272: filenames that collide under case-insensitive comparison produce
// archives ParseArchive refuses. Export must skip and report the collision.
func TestExportCaseCollidingNamesSkipped(t *testing.T) {
	v := newTestVFS(t)
	if err := v.WriteFile("/users/guest/documents/README.md", []byte("upper"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/users/guest/documents/readme.md", []byte("lower"), 0o644); err != nil {
		t.Fatal(err)
	}

	buf := new(bytes.Buffer)
	res, err := recovery.Export(context.Background(), v, buf, recovery.ExportOptions{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if res.TotalFiles != 1 {
		t.Fatalf("expected exactly one of the colliding files exported, got %d", res.TotalFiles)
	}
	found := false
	for _, s := range res.Skipped {
		if strings.Contains(strings.ToLower(s.Path), "readme.md") && strings.Contains(s.Reason, "collide") {
			found = true
		}
	}
	if !found {
		t.Fatalf("case collision not reported in skipped: %+v", res.Skipped)
	}
	if _, err := recovery.ParseArchive(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("exported archive rejected by own parser: %v", err)
	}
}

// #274: recovery ceilings (100 MiB archives, 50 MiB files) exceed the
// interactive 16 MiB document cap; archives and restored payloads that fit
// the recovery budgets must save and restore through the VFS.
func TestExportAndRestoreAboveDocumentCap(t *testing.T) {
	v := newTestVFS(t)
	// Incompressible ~17 MiB payload keeps the archive above the 16 MiB
	// document cap but well inside the recovery budgets.
	payload := make([]byte, 17<<20)
	rng := rand.New(rand.NewSource(42))
	if _, err := rng.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/users/guest/documents/big.bin", payload, 0o644); err != nil {
		t.Fatal(err)
	}

	dest := "/users/guest/downloads/big.gbar"
	res, err := recovery.ExportToVFS(context.Background(), v, dest, recovery.ExportOptions{})
	if err != nil {
		t.Fatalf("export of >16 MiB archive failed: %v", err)
	}
	if res.ArchivePath != dest {
		t.Fatalf("archive path = %q, want %q", res.ArchivePath, dest)
	}
	data, err := v.ReadFile(dest)
	if err != nil {
		t.Fatalf("read exported archive: %v", err)
	}
	if _, err := recovery.ParseArchive(bytes.NewReader(data)); err != nil {
		t.Fatalf("exported archive rejected by own parser: %v", err)
	}

	dst := vfs.New(vfs.NewMem())
	rep, err := recovery.Restore(context.Background(), bytes.NewReader(data), dst, recovery.RestoreOptions{
		Strategy: recovery.ConflictAbort,
	})
	if err != nil {
		t.Fatalf("restore of >16 MiB payload failed: %v", err)
	}
	if rep.RestoredCount != 1 {
		t.Fatalf("expected 1 restored file, got %d", rep.RestoredCount)
	}
	restored, err := dst.ReadFile("/users/guest/documents/big.bin")
	if err != nil {
		t.Fatalf("read restored payload: %v", err)
	}
	if sha256.Sum256(restored) != sha256.Sum256(payload) {
		t.Fatal("restored payload differs from source")
	}
}

// #275: the live system preferences store and user-installed themes must
// land in the backup; expected-but-absent items are reported as skipped.
func TestExportIncludesSystemStoreAndThemes(t *testing.T) {
	v := newTestVFS(t)
	if err := v.WriteFile("/config/themes/neon.json", []byte(`{"name":"neon","colors":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteFile("/config/profiles.json", []byte(`{"active":"guest","profiles":[{"id":"guest","name":"Guest"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	systemDoc := []byte(`{"theme":"neon","accessibility":{"high_contrast":true}}`)

	buf := new(bytes.Buffer)
	res, err := recovery.Export(context.Background(), v, buf, recovery.ExportOptions{
		IncludeSystem: true,
		SystemConfig:  systemDoc,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	va, err := recovery.ParseArchive(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := va.Files["data/config/system.json"]; !bytes.Equal(got, systemDoc) {
		t.Fatalf("system preferences not exported from the live store: %q", got)
	}
	if got := va.Files["data/config/themes/neon.json"]; !bytes.Equal(got, []byte(`{"name":"neon","colors":{}}`)) {
		t.Fatalf("theme not exported: %q", got)
	}
	_ = res

	// Expected-but-absent items appear in Skipped.
	v2 := newTestVFS(t)
	if err := v2.WriteFile("/config/profiles.json", []byte(`{"active":"guest","profiles":[{"id":"guest","name":"Guest"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res2, err := recovery.Export(context.Background(), v2, new(bytes.Buffer), recovery.ExportOptions{IncludeSystem: true})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	found := false
	for _, s := range res2.Skipped {
		if s.Path == "/config/system.json" {
			found = true
		}
	}
	if !found {
		t.Fatalf("absent system store not reported: %+v", res2.Skipped)
	}
}

// #275: a restored /config/system.json must reach the live host system
// store, not a VFS location nothing reads.
func TestRestoreRoutesSystemConfigToHostStore(t *testing.T) {
	systemDoc := `{"theme":"midnight"}`
	man := recovery.Manifest{FormatVersion: recovery.FormatVersion}
	content := []byte(systemDoc)
	man.Files = append(man.Files, recovery.FileEntry{
		VFSPath: "/config/system.json",
		ArcName: "data/config/system.json",
		Size:    int64(len(content)),
		SHA256:  recovery.ComputeSHA256(content),
	})
	mBytes, _ := json.Marshal(man)
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)
	for name, body := range map[string][]byte{
		recovery.ManifestFilename: mBytes,
		"data/config/system.json": content,
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()

	hostPath := filepath.Join(t.TempDir(), "config", "system.json")
	dst := newTestVFS(t)
	rep, err := recovery.Restore(context.Background(), bytes.NewReader(archive), dst, recovery.RestoreOptions{
		Strategy:         recovery.ConflictAbort,
		SystemConfigPath: hostPath,
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if rep.RestoredCount != 1 {
		t.Fatalf("expected 1 restored file, got %d", rep.RestoredCount)
	}
	got, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatalf("system store not written to host path: %v", err)
	}
	if string(got) != systemDoc {
		t.Fatalf("host system store = %q, want %q", got, systemDoc)
	}
	if _, err := dst.ReadFile("/config/system.json"); err == nil {
		t.Fatal("restored system.json leaked into the dead VFS location")
	}
}
