package profile

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"gostalgia/internal/events"
	"gostalgia/internal/vfs"
)

func TestProfileManagerDefaultAndValidation(t *testing.T) {
	memFS := vfs.NewMem()
	bus := events.NewBus()
	mgr, err := NewManager(memFS, DefaultProfilesPath, bus, slog.Default())
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	active := mgr.Active()
	if active.ID != DefaultProfileID {
		t.Fatalf("expected active profile %s, got %s", DefaultProfileID, active.ID)
	}
	if active.Name != "Guest" {
		t.Fatalf("expected active profile name Guest, got %s", active.Name)
	}

	// Validate path helpers
	if UserDir("alice") != "/users/alice" {
		t.Errorf("UserDir = %s, want /users/alice", UserDir("alice"))
	}
	if DocumentsDir("alice") != "/users/alice/documents" {
		t.Errorf("DocumentsDir = %s, want /users/alice/documents", DocumentsDir("alice"))
	}
	if SettingsPath("alice") != "/users/alice/config/settings.json" {
		t.Errorf("SettingsPath = %s, want /users/alice/config/settings.json", SettingsPath("alice"))
	}
	if WorkspacePath("alice") != "/users/alice/config/workspace.json" {
		t.Errorf("WorkspacePath = %s, want /users/alice/config/workspace.json", WorkspacePath("alice"))
	}

	// Validate ID rejection
	badIDs := []string{"", "   ", "a/b", "a\\b", "..", ".", "ALICE!", "a b", strings.Repeat("x", 40)}
	for _, bad := range badIDs {
		if err := ValidateID(bad); err == nil {
			t.Errorf("ValidateID(%q) expected error, got nil", bad)
		}
	}

	goodIDs := []string{"guest", "alice", "alice-123", "dev_user"}
	for _, good := range goodIDs {
		if err := ValidateID(good); err != nil {
			t.Errorf("ValidateID(%q) unexpected error: %v", good, err)
		}
	}
}

func TestProfileManagerLifecycleAndSwitch(t *testing.T) {
	memFS := vfs.NewMem()
	bus := events.NewBus()

	var eventsCaptured []any
	bus.Subscribe("profile", func(env events.Envelope) {
		eventsCaptured = append(eventsCaptured, env.Payload)
	})

	mgr, err := NewManager(memFS, DefaultProfilesPath, bus, slog.Default())
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}

	// Create profile "developer"
	dev, err := mgr.Create("developer", "Dev Lead")
	if err != nil {
		t.Fatalf("Create developer failed: %v", err)
	}
	if dev.ID != "developer" || dev.Name != "Dev Lead" {
		t.Fatalf("unexpected profile: %+v", dev)
	}

	// Verify directory structure created in VFS
	for _, dir := range []string{
		"users/developer/documents",
		"users/developer/downloads",
		"users/developer/desktop",
		"users/developer/config",
		"users/developer/.trash",
	} {
		info, err := memFS.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("expected directory %s to exist, err: %v", dir, err)
		}
	}

	// Switch profile
	switched := false
	mgr.OnSwitch(func(prev, next Profile) {
		if prev.ID == "guest" && next.ID == "developer" {
			switched = true
		}
	})

	active, err := mgr.Switch("developer")
	if err != nil {
		t.Fatalf("Switch failed: %v", err)
	}
	if active.ID != "developer" {
		t.Fatalf("expected active ID developer, got %s", active.ID)
	}
	if !switched {
		t.Fatal("expected OnSwitch callback to be invoked")
	}

	// Try deleting active profile (must fail)
	if err := mgr.Delete("developer"); err == nil {
		t.Fatal("expected error deleting active profile, got nil")
	}

	// Switch back to guest and delete developer
	_, err = mgr.Switch("guest")
	if err != nil {
		t.Fatalf("Switch back failed: %v", err)
	}
	if err := mgr.Delete("developer"); err != nil {
		t.Fatalf("Delete developer failed: %v", err)
	}
	if _, ok := mgr.Get("developer"); ok {
		t.Fatal("expected developer profile to be deleted")
	}
}

// #158: deleting a profile must remove its /users/<id>/ data tree —
// documents, config (including workspace history), downloads, desktop,
// trash — not just the registry entry.
func TestProfileDeleteRemovesUserTree(t *testing.T) {
	memFS := vfs.NewMem()
	mgr, err := NewManager(memFS, DefaultProfilesPath, events.NewBus(), slog.Default())
	if err != nil {
		t.Fatalf("NewManager failed: %v", err)
	}
	if _, err := mgr.Create("alice", "Alice"); err != nil {
		t.Fatalf("Create alice failed: %v", err)
	}
	for path, data := range map[string]string{
		"users/alice/documents/secret.txt":     "TOP SECRET",
		"users/alice/config/workspace.json":    `{"history":["cmd"]}`,
		"users/alice/downloads/payload.bin":    "data",
		"users/alice/.trash/files/x/meta.json": "{}",
	} {
		if err := memFS.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}

	if err := mgr.Delete("alice"); err != nil {
		t.Fatalf("Delete alice failed: %v", err)
	}
	if _, err := memFS.Stat("users/alice"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("users/alice still exists after Delete (stat err %v)", err)
	}
	// Other profiles' trees are untouched.
	if info, err := memFS.Stat("users/guest/documents"); err != nil || !info.IsDir() {
		t.Fatalf("guest tree damaged by alice's delete: %v", err)
	}

	// A missing tree is not an error — the registry entry still goes away.
	if _, err := mgr.Create("bob", "Bob"); err != nil {
		t.Fatalf("Create bob failed: %v", err)
	}
	if err := memFS.RemoveAll("users/bob"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Delete("bob"); err != nil {
		t.Fatalf("Delete with missing tree failed: %v", err)
	}
	if _, ok := mgr.Get("bob"); ok {
		t.Fatal("bob still registered after Delete")
	}
}

func TestProfileManagerCorruptRecovery(t *testing.T) {
	memFS := vfs.NewMem()
	_ = memFS.MkdirAll("config")
	_ = memFS.WriteFile("config/profiles.json", []byte("{invalid json garbage"), 0o644)

	mgr, err := NewManager(memFS, DefaultProfilesPath, nil, slog.Default())
	if err != nil {
		t.Fatalf("expected recovery, got error: %v", err)
	}
	if mgr.ActiveID() != DefaultProfileID {
		t.Fatalf("expected fallback active profile guest, got %s", mgr.ActiveID())
	}
	if len(mgr.List()) != 1 {
		t.Fatalf("expected 1 profile in recovered store, got %d", len(mgr.List()))
	}
}
