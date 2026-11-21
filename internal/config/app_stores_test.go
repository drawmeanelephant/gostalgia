package config

import (
	"path/filepath"
	"testing"
)

// GH-8: app-layer values are persisted under <user config>/apps/<id>.json
// but were only loaded inside Set. A freshly bound store must see the
// on-disk values on first read — otherwise app config silently
// disappears after every restart.
func TestAppStoreLoadedOnBind(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "settings.json")
	appOpts := QueryOpts{AppID: "com.gostalgia.notes"}

	ls, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Set(LayerApp, "accessibility.mouse", false, appOpts); err != nil {
		t.Fatal(err)
	}

	// A fresh binding, as after restart, must resolve the persisted value.
	ls2, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	v, layer, ok := ls2.Value("accessibility.mouse", appOpts)
	if !ok || layer != LayerApp || v != false {
		t.Fatalf("app value lost after rebind: v=%v layer=%s ok=%v", v, layer, ok)
	}

	// Rebinding an existing store must pick up the same on-disk values.
	ls3, err := NewLayeredStore("", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ls3.SetUserStore(userPath); err != nil {
		t.Fatal(err)
	}
	v, layer, ok = ls3.Value("accessibility.mouse", appOpts)
	if !ok || layer != LayerApp || v != false {
		t.Fatalf("app value lost after SetUserStore: v=%v layer=%s ok=%v", v, layer, ok)
	}
}

// GH-8: app stores belong to the user profile's config directory. A user
// store switch must not leak the previous profile's app values.
func TestSetUserStoreClearsAppStores(t *testing.T) {
	dir := t.TempDir()
	user1 := filepath.Join(dir, "u1", "settings.json")
	user2 := filepath.Join(dir, "u2", "settings.json")
	appOpts := QueryOpts{AppID: "com.gostalgia.notes"}

	ls, err := NewLayeredStore("", user1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Set(LayerApp, "accessibility.mouse", false, appOpts); err != nil {
		t.Fatal(err)
	}

	if err := ls.SetUserStore(user2); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := ls.Value("accessibility.mouse", appOpts); ok {
		t.Fatal("previous profile's app value leaked across user store switch")
	}
}

// GH-8: Unset reported success while the on-disk app value survived —
// resurrected by the next unrelated Set. Unset must reach the persisted
// store even when it was never loaded in this process.
func TestAppUnsetDeletesPersistedValue(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "settings.json")
	appPath := filepath.Join(dir, "apps", "com.gostalgia.notes.json")
	appOpts := QueryOpts{AppID: "com.gostalgia.notes"}

	ls, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Set(LayerApp, "accessibility.mouse", false, appOpts); err != nil {
		t.Fatal(err)
	}

	// Fresh binding, as after restart.
	ls2, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls2.Unset(LayerApp, "accessibility.mouse", appOpts); err != nil {
		t.Fatal(err)
	}

	disk, err := Load(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Value("accessibility.mouse"); ok {
		t.Fatal("unset reported success but left the value on disk")
	}

	// An unrelated set must not resurrect the deleted value.
	if err := ls2.Set(LayerApp, "accessibility.reduced_motion", true, appOpts); err != nil {
		t.Fatal(err)
	}
	disk, err = Load(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Value("accessibility.mouse"); ok {
		t.Fatal("deleted app value resurrected by an unrelated set")
	}
}

// GH-8: resetting the app layer must clear the persisted file, not just
// whatever happened to be loaded in memory.
func TestAppResetDeletesPersistedValues(t *testing.T) {
	dir := t.TempDir()
	userPath := filepath.Join(dir, "settings.json")
	appPath := filepath.Join(dir, "apps", "com.gostalgia.notes.json")
	appOpts := QueryOpts{AppID: "com.gostalgia.notes"}

	ls, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.Set(LayerApp, "accessibility.mouse", false, appOpts); err != nil {
		t.Fatal(err)
	}

	ls2, err := NewLayeredStore("", userPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls2.Reset(LayerApp, "", appOpts); err != nil {
		t.Fatal(err)
	}

	disk, err := Load(appPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := disk.Value("accessibility.mouse"); ok {
		t.Fatal("reset reported success but left the value on disk")
	}
}
