package pkg

import (
	"context"
	"encoding/json"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #276: one corrupt installed package must not brick the environment boot.
// Each on-disk artifact type (state.json, active version directory, active
// version payload) gets damaged in turn; the manager boots, quarantines
// the package, and intact packages keep working.
func TestCorruptPackageQuarantinedAtBoot(t *testing.T) {
	damage := map[string]func(t *testing.T, root, id string){
		"corrupt-state-json": func(t *testing.T, root, id string) {
			mustWrite(t, filepath.Join(root, id, "state.json"), []byte("{corrupt"))
		},
		"oversized-state-json": func(t *testing.T, root, id string) {
			mustWrite(t, filepath.Join(root, id, "state.json"), make([]byte, maxJSONBytes+1))
		},
		"missing-active-version": func(t *testing.T, root, id string) {
			gen := activeGen(t, root, id)
			if err := os.RemoveAll(filepath.Join(root, id, "versions", gen)); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt-active-version": func(t *testing.T, root, id string) {
			gen := activeGen(t, root, id)
			mustWrite(t, filepath.Join(root, id, "versions", gen, "manifest.json"), []byte("{corrupt"))
		},
		"bad-active-pointer": func(t *testing.T, root, id string) {
			mustWrite(t, filepath.Join(root, id, "state.json"),
				[]byte(`{"format_version":1,"active":"zznothex"}`))
		},
	}
	for name, damageFn := range damage {
		t.Run(name, func(t *testing.T) {
			m, _, root := testManager(t)
			man := testManifest()
			install(t, m, man, []byte("executable"), false)
			good := testManifest()
			good.ID = "com.good.app"
			install(t, m, good, []byte("good exec"), false)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}

			damageFn(t, root, man.ID)

			rebootApps, _ := bareApps(t)
			reboot, err := NewManager(root, rebootApps, nil)
			if err != nil {
				t.Fatalf("corrupt package %s blocked boot: %v", name, err)
			}
			defer reboot.Close()

			// The bad package is quarantined and marked unavailable.
			if _, quarantined := reboot.Quarantined(man.ID); !quarantined {
				t.Fatalf("damaged package %s not quarantined", name)
			}
			found := false
			for _, info := range reboot.List() {
				if info.Manifest.ID == man.ID {
					found = true
					if !info.Quarantined || info.QuarantineReason == "" {
						t.Fatalf("quarantined package not marked unavailable: %+v", info)
					}
				}
			}
			if !found {
				t.Fatal("quarantined package missing from List")
			}
			insp, err := reboot.Inspect(man.ID)
			if err != nil || !insp.Quarantined {
				t.Fatalf("Inspect did not report quarantine: %+v, %v", insp, err)
			}

			// Launch refused: the manifest never reaches the registry.
			if _, exists := rebootApps.Registry().Manifest(man.ID); exists {
				t.Fatal("quarantined package registered for launch")
			}
			if _, err := reboot.Rollback(context.Background(), man.ID, true); err == nil || !strings.Contains(err.Error(), "quarantined") {
				t.Fatalf("rollback on quarantined package: %v", err)
			}
			man.Version = "2.0.0"
			if _, err := reboot.Install(context.Background(), bytes.NewReader(archive(t, man, []byte("new"), nil)), true, true); err == nil || !strings.Contains(err.Error(), "quarantined") {
				t.Fatalf("update on quarantined package: %v", err)
			}

			// The intact package is unaffected.
			if _, exists := rebootApps.Registry().Manifest(good.ID); !exists {
				t.Fatal("intact package lost registration during quarantined boot")
			}
			if _, err := reboot.Inspect(good.ID); err != nil {
				t.Fatalf("intact package inspect failed: %v", err)
			}

			// Uninstall is the repair path: it removes the quarantined dir.
			if err := reboot.Uninstall(context.Background(), man.ID); err != nil {
				t.Fatalf("uninstall of quarantined package failed: %v", err)
			}
			if _, quarantined := reboot.Quarantined(man.ID); quarantined {
				t.Fatal("uninstalled package still quarantined")
			}
			if _, err := os.Stat(filepath.Join(root, man.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("uninstall left quarantined app directory")
			}
		})
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func activeGen(t *testing.T, root, id string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, id, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state diskState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	return state.Active
}
