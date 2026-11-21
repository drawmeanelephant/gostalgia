package vfs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Trash metadata is a plain JSON document reachable through the ordinary
// filesystem: any writer able to place info/<id>.json controls the blob.
// Restore must derive the payload location from the trash ID and the
// files/<id>/ tree — never from the forgeable trash_path field.
func TestRestoreIgnoresForgedTrashPath(t *testing.T) {
	v, _ := setupTestVFS(t)

	// Victim app-private data on the unscoped filesystem.
	must(t, v.MkdirAll("/apps/data/com.test.victim"))
	must(t, v.WriteFile("/apps/data/com.test.victim/secret.txt", []byte("victim secret"), 0o644))

	// Forge a metadata blob pointing trash_path at the victim directory.
	forged := TrashEntry{
		ID:           "forged-1",
		OriginalPath: "/users/guest/documents/dropped",
		TrashPath:    "/apps/data/com.test.victim",
		Name:         "com.test.victim",
		IsDir:        true,
		Size:         13,
		TrashedAt:    time.Now().UTC(),
	}
	data, err := json.Marshal(forged)
	must(t, err)
	must(t, v.MkdirAll("/users/guest/.trash/info"))
	must(t, v.SaveAtomic("/users/guest/.trash/info/forged-1.json", data, 0o644))

	if _, err := v.Restore("forged-1", "/users/guest/documents/dropped", true); err == nil {
		t.Fatal("restore of forged metadata succeeded")
	}

	// The victim directory must be untouched.
	content, err := v.ReadFile("/apps/data/com.test.victim/secret.txt")
	if err != nil || string(content) != "victim secret" {
		t.Fatalf("forged restore moved victim data: content=%q err=%v", content, err)
	}
}

// The metadata blob is advisory: even when its trash_path is corrupted the
// payload recorded under files/<id>/ must still restore correctly.
func TestRestoreDerivesPayloadFromTrashTree(t *testing.T) {
	v, _ := setupTestVFS(t)

	must(t, v.MkdirAll("/users/guest/documents"))
	must(t, v.WriteFile("/users/guest/documents/report.txt", []byte("quarterly"), 0o644))
	entry, err := v.Trash("/users/guest/documents/report.txt")
	must(t, err)

	// Tamper with the recorded payload location.
	tampered := *entry
	tampered.TrashPath = "/apps/data"
	data, err := json.Marshal(tampered)
	must(t, err)
	must(t, v.SaveAtomic(v.TrashDir()+"/info/"+entry.ID+".json", data, 0o644))

	dst, err := v.Restore(entry.ID, "", false)
	must(t, err)
	if dst != "/users/guest/documents/report.txt" {
		t.Fatalf("restored destination = %q, want original path", dst)
	}
	content, err := v.ReadFile(dst)
	if err != nil || string(content) != "quarterly" {
		t.Fatalf("restored content = %q err=%v", content, err)
	}
	if _, err := v.Stat("/apps/data"); err != nil {
		t.Fatalf("tampered restore disturbed /apps/data: %v", err)
	}
}

// A metadata file whose files/<id>/ payload tree is missing or empty is not
// a restorable entry — the payload is what defines the trash item.
func TestRestoreRefusesMissingPayload(t *testing.T) {
	v, _ := setupTestVFS(t)

	entry := TrashEntry{
		ID:           "ghost-1",
		OriginalPath: "/users/guest/documents/ghost.txt",
		TrashPath:    "/users/guest/.trash/files/ghost-1/ghost.txt",
		Name:         "ghost.txt",
		TrashedAt:    time.Now().UTC(),
	}
	data, err := json.Marshal(entry)
	must(t, err)
	must(t, v.MkdirAll("/users/guest/.trash/info"))
	must(t, v.SaveAtomic("/users/guest/.trash/info/ghost-1.json", data, 0o644))

	if _, err := v.Restore("ghost-1", "", false); err == nil {
		t.Fatal("restore of payload-less metadata succeeded")
	}
	var vErr *Error
	if _, err := v.Restore("ghost-1", "", false); !errors.As(err, &vErr) || vErr.Code != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// Profile trash trees are system-managed: grants must not reach them, so an
// app cannot author metadata or payloads through the ordinary filesystem.
func TestProfileTrashNotGrantable(t *testing.T) {
	v, _ := setupTestVFS(t)
	appA := v.ForApp("appA")

	// A recursive grant over the whole home dir must not cover .trash.
	_, err := v.Grants().Issue("appA", "/users/guest", AccessReadWrite, true)
	must(t, err)

	if err := appA.WriteFile("/users/guest/.trash/info/evil.json", []byte("{}"), 0o644); err == nil {
		t.Fatal("app wrote into the profile trash tree")
	}
	if _, err := appA.ReadDir("/users/guest/.trash"); err == nil {
		t.Fatal("app listed the profile trash tree")
	}
	if err := appA.RemoveAll("/users/guest/.trash"); err == nil {
		t.Fatal("app removed the profile trash tree")
	}

	// A direct grant on the trash dir is refused at issue time.
	if _, err := v.Grants().Issue("appA", "/users/guest/.trash", AccessReadWrite, true); err == nil {
		t.Fatal("grant issued on profile trash")
	}
	if _, err := v.Grants().Issue("appA", "/users/guest/.trash/info", AccessRead, true); err == nil {
		t.Fatal("grant issued on profile trash subtree")
	}
	// And a granted app still cannot reach a neighboring profile's trash.
	if _, err := appA.ReadDir("/users/alice/.trash"); err == nil {
		t.Fatal("app listed another profile's trash tree")
	}
}

// Trashing and restoring inside a non-default profile trash dir keeps the
// payload inside that tree end to end.
func TestTrashFollowsConfiguredDir(t *testing.T) {
	v, _ := setupTestVFS(t)
	v.SetTrashDir("/users/alice/.trash")

	must(t, v.MkdirAll("/users/alice/documents"))
	must(t, v.WriteFile("/users/alice/documents/journal.txt", []byte("dear diary"), 0o644))

	entry, err := v.Trash("/users/alice/documents/journal.txt")
	must(t, err)
	if !strings.HasPrefix(entry.TrashPath, "/users/alice/.trash/files/") {
		t.Fatalf("payload landed at %q, outside alice's trash", entry.TrashPath)
	}
	if _, err := v.Stat("/users/guest/.trash"); err == nil {
		t.Fatal("trash operation created the guest trash tree")
	}
}
