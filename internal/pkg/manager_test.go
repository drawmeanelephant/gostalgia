package pkg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/events"
	"gostalgia/internal/ipc"
	"gostalgia/internal/process"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
	"gostalgia/sdk"
)

func bareApps(t *testing.T) (*app.Manager, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	procs := process.NewManager(bus, log)
	apps := app.NewManager(app.NewRegistry(), procs, ipc.NewRouter(), bus, log)
	apps.SetGrantStore(vfs.NewGrantStore())
	t.Cleanup(func() {
		for id := range apps.Running() {
			_ = apps.Stop(id, 2*time.Second)
		}
		procs.Shutdown(time.Second)
	})
	return apps, bus
}

func testManager(t *testing.T) (*Manager, *app.Manager, string) {
	t.Helper()
	apps, _ := bareApps(t)
	root := t.TempDir()
	m, err := NewManager(root, apps, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, apps, root
}

func install(t *testing.T, m *Manager, man sdk.Manifest, binary []byte, update bool) Info {
	t.Helper()
	out, err := m.Install(context.Background(), bytes.NewReader(archive(t, man, binary, nil)), update, true)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInstallUpdateRollbackPersistence(t *testing.T) {
	m, apps, root := testManager(t)
	man := testManifest()
	install(t, m, man, []byte("old executable"), false)
	old, _ := apps.Registry().Manifest(man.ID)
	man.Version = "2.0.0"
	out := install(t, m, man, []byte("new executable"), true)
	if out.PreviousVersion != "1.0.0" {
		t.Fatalf("no previous version: %+v", out)
	}
	newManifest, _ := apps.Registry().Manifest(man.ID)
	if old.Executable == newManifest.Executable {
		t.Fatal("mutable executable path")
	}
	data, err := os.ReadFile(old.Executable)
	if err != nil || string(data) != "old executable" {
		t.Fatal("previous executable lost", err)
	}
	out, err = m.Rollback(context.Background(), man.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Manifest.Version != "1.0.0" || out.PreviousVersion != "2.0.0" {
		t.Fatalf("bad rollback: %+v", out)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	rebootApps, _ := bareApps(t)
	reboot, err := NewManager(root, rebootApps, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reboot.Close()
	out, err = reboot.Inspect(man.ID)
	if err != nil || out.Manifest.Version != "1.0.0" {
		t.Fatal("reboot lost active state", err)
	}
	out, err = reboot.Rollback(context.Background(), man.ID, false)
	if err != nil || out.Manifest.Version != "2.0.0" {
		t.Fatal("reboot lost rollback state", err)
	}
	// Returned manifest slices must not alias persisted permissions.
	out.Manifest.Args = append(out.Manifest.Args, "modified")
	if got, _ := reboot.Inspect(man.ID); len(got.Manifest.Args) != 0 {
		t.Fatal("inspection changed installed manifest")
	}
	if err := reboot.Uninstall(context.Background(), man.ID); err != nil {
		t.Fatal(err)
	}
	if len(reboot.List()) != 0 {
		t.Fatal("uninstall left an installed record")
	}
	if _, ok := rebootApps.Registry().Manifest(man.ID); ok {
		t.Fatal("uninstall left a registry entry")
	}
	if _, err := os.Stat(filepath.Join(root, man.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("uninstall left app directory")
	}
}

func TestFailedTransactionsPreserveOldVersion(t *testing.T) {
	m, apps, root := testManager(t)
	man := testManifest()
	man.PathGrants = []sdk.PathGrant{{Path: "/users/guest/documents", Access: "read"}}
	install(t, m, man, []byte("old"), false)
	old, _ := apps.Registry().Manifest(man.ID)
	statePath := filepath.Join(root, man.ID, "state.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	man.Version = "2.0.0"
	m.commitHook = func() error {
		if _, err := apps.Launch(context.Background(), man.ID); err == nil {
			t.Fatal("launch allowed during maintenance")
		}
		if len(m.List()) != 1 {
			t.Fatal("read callback blocked or lost old package")
		}
		return errors.New("simulated mid-update disk failure")
	}
	_, err = m.Install(context.Background(), bytes.NewReader(archive(t, man, []byte("new"), nil)), true, true)
	if err == nil {
		t.Fatal("simulated failure succeeded")
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed update changed commit point", err)
	}
	current, _ := apps.Registry().Manifest(man.ID)
	if current.Executable != old.Executable || current.Version != "1.0.0" {
		t.Fatal("failed update changed runnable app")
	}
	if err := apps.GrantStore().CheckAccess(man.ID, "/users/guest/documents", vfs.AccessRead); err != nil {
		t.Fatal("failed update revoked old grants", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, man.ID, "versions"))
	if err != nil || len(entries) != 1 {
		t.Fatal("failed update left staged version", err)
	}
	m.commitHook = nil
	// Verification failures happen before any maintenance.
	if _, err := m.Install(context.Background(), bytes.NewReader([]byte("corrupt")), true, false); err == nil {
		t.Fatal("corruption accepted")
	}
	install(t, m, man, []byte("new"), true)
	before, _ = os.ReadFile(statePath)
	m.commitHook = func() error { return errors.New("rollback failure") }
	if _, err := m.Rollback(context.Background(), man.ID, true); err == nil {
		t.Fatal("rollback failure succeeded")
	}
	after, _ = os.ReadFile(statePath)
	if !bytes.Equal(before, after) {
		t.Fatal("failed rollback changed state")
	}
}

func TestConfirmationAndGrantRevocation(t *testing.T) {
	m, apps, _ := testManager(t)
	man := testManifest()
	install(t, m, man, []byte("old"), false)
	man.Version = "2.0.0"
	man.Permissions = []string{sdk.CapFileRead}
	man.PathGrants = []sdk.PathGrant{{Path: "/users/guest/documents", Access: "read-write", Recursive: true}}
	candidate := archive(t, man, []byte("new"), nil)
	out, err := m.InspectArchive(bytes.NewReader(candidate))
	if err != nil || !out.Permissions.Expansion || len(out.Permissions.AddedPathGrants) != 1 {
		t.Fatal("inspection missed expansion", err)
	}
	_, err = m.Install(context.Background(), bytes.NewReader(candidate), true, false)
	var confirmation *ConfirmationError
	if !errors.As(err, &confirmation) {
		t.Fatal("expansion did not require confirmation", err)
	}
	install(t, m, man, []byte("new"), true)
	if err := apps.GrantStore().CheckAccess(man.ID, "/users/guest/documents/file", vfs.AccessReadWrite); err != nil {
		t.Fatal(err)
	}
	grants := apps.GrantStore().List(man.ID)
	if _, err := m.Rollback(context.Background(), man.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		now, _ := apps.GrantStore().Get(g.ID)
		if !now.Revoked {
			t.Fatal("rollback retained expanded grants")
		}
	}
	if _, err := m.Rollback(context.Background(), man.ID, false); !errors.As(err, &confirmation) {
		t.Fatal("rollback expansion bypassed confirmation", err)
	}
	if _, err := m.Rollback(context.Background(), man.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Uninstall(context.Background(), man.ID); err != nil {
		t.Fatal(err)
	}
	if err := apps.GrantStore().CheckAccess(man.ID, "/users/guest/documents", vfs.AccessRead); err == nil {
		t.Fatal("uninstall retained grants")
	}
}

func TestInstalledTamperingRejected(t *testing.T) {
	m, apps, root := testManager(t)
	man := testManifest()
	install(t, m, man, []byte("old"), false)
	old, _ := apps.Registry().Manifest(man.ID)
	man.Version = "2.0.0"
	install(t, m, man, []byte("new"), true)
	if err := os.WriteFile(old.Executable, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rollback(context.Background(), man.ID, false); err == nil {
		t.Fatal("rollback accepted tampered payload")
	}
	_ = m.Close()
	// The tampered payload is a rollback-only artifact, so boot must still
	// succeed with the pointer dropped (#159) — but rollback stays refused.
	rebootApps, _ := bareApps(t)
	reopened, err := NewManager(root, rebootApps, nil)
	if err != nil {
		t.Fatal("tampered previous version blocked boot", err)
	}
	defer reopened.Close()
	if _, err := reopened.Rollback(context.Background(), man.ID, false); err == nil {
		t.Fatal("rollback accepted tampered payload after reboot")
	}
	info, err := reopened.Inspect(man.ID)
	if err != nil || info.Manifest.Version != "2.0.0" {
		t.Fatal("reboot lost the intact active version", err)
	}
	if info.PreviousVersion != "" {
		t.Fatal("tampered previous version still offered for rollback")
	}
}

// #159a: a corrupt or missing previous-version artifact is rollback-only
// state, so it must not brick the environment boot while the active version
// is intact. Load drops the pointer; the package stays usable without a
// rollback target.
func TestCorruptPreviousVersionDoesNotBlockBoot(t *testing.T) {
	damage := map[string]func(t *testing.T, prevDir string){
		"missing": func(t *testing.T, prevDir string) {
			t.Helper()
			if err := os.RemoveAll(prevDir); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt": func(t *testing.T, prevDir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(prevDir, "manifest.json"), []byte("{corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, damageFn := range damage {
		t.Run(name, func(t *testing.T) {
			m, _, root := testManager(t)
			man := testManifest()
			install(t, m, man, []byte("old executable"), false)
			man.Version = "2.0.0"
			install(t, m, man, []byte("new executable"), true)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}

			data, err := os.ReadFile(filepath.Join(root, man.ID, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			var state diskState
			if err := json.Unmarshal(data, &state); err != nil || state.Previous == "" {
				t.Fatalf("expected a previous pointer, got %s (%v)", data, err)
			}
			damageFn(t, filepath.Join(root, man.ID, "versions", state.Previous))

			rebootApps, _ := bareApps(t)
			reboot, err := NewManager(root, rebootApps, nil)
			if err != nil {
				t.Fatal("damaged previous version blocked boot", err)
			}
			defer reboot.Close()
			out, err := reboot.Inspect(man.ID)
			if err != nil || out.Manifest.Version != "2.0.0" {
				t.Fatal("boot lost the intact active version", err)
			}
			if out.PreviousVersion != "" {
				t.Fatal("dropped previous version still advertised")
			}
			if _, err := reboot.Rollback(context.Background(), man.ID, false); err == nil {
				t.Fatal("rollback survived a dropped previous version")
			}
		})
	}
}

// #159b: crash-orphaned install/uninstall staging artifacts (.staging-*,
// .removed-*, .state-*) accumulated under the apps root forever. Load
// collects them; lookalike names without a valid generation are left alone.
func TestStagingArtifactsGarbageCollectedOnLoad(t *testing.T) {
	m, _, root := testManager(t)
	man := testManifest()
	install(t, m, man, []byte("executable"), false)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}

	gen := "0123456789abcdef0123456789abcdef"
	orphans := []string{
		filepath.Join(root, ".staging-"+gen, "payload"),
		filepath.Join(root, ".removed-"+gen, "payload"),
		filepath.Join(root, man.ID, ".state-"+gen),
	}
	for _, orphan := range orphans {
		if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(orphan, []byte("orphan"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(root, ".staging-notours")
	if err := os.MkdirAll(sentinel, 0o700); err != nil {
		t.Fatal(err)
	}

	rebootApps, _ := bareApps(t)
	reboot, err := NewManager(root, rebootApps, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reboot.Close()
	if _, err := reboot.Inspect(man.ID); err != nil {
		t.Fatal("installed package lost to staging GC", err)
	}
	for _, orphan := range []string{
		filepath.Join(root, ".staging-"+gen),
		filepath.Join(root, ".removed-"+gen),
		filepath.Join(root, man.ID, ".state-"+gen),
	} {
		if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphaned staging artifact %q survived load", orphan)
		}
	}
	if _, err := os.Lstat(sentinel); err != nil {
		t.Fatal("GC removed an entry it does not own", err)
	}
}

func TestTargetSymlinkAndCancellation(t *testing.T) {
	m, _, root := testManager(t)
	outside := t.TempDir()
	man := testManifest()
	if err := os.Symlink(outside, filepath.Join(root, man.ID)); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := m.Install(context.Background(), bytes.NewReader(archive(t, man, []byte("binary"), nil)), false, true); err == nil {
		t.Fatal("installation followed existing target symlink")
	}
	files, _ := os.ReadDir(outside)
	if len(files) != 0 {
		t.Fatal("wrote outside package root")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Install(ctx, bytes.NewReader(archive(t, man, []byte("binary"), nil)), false, true); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
}

// This test executable also acts as an external SDK application when launched
// by the package manager. No host package manager or generated source is used.
type packageChild struct{}

func (*packageChild) Init(ctx *sdk.Context) error {
	return ctx.Handle("ping", func(context.Context, json.RawMessage) (any, error) { return "pong", nil })
}
func (*packageChild) Run(ctx context.Context) error { <-ctx.Done(); return nil }
func (*packageChild) Stop(context.Context) error    { return nil }

func TestPackageChild(t *testing.T) {
	if os.Getenv("GOSTALGIA_APP_ID") == "" {
		return
	}
	if err := sdk.Serve(&packageChild{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestLiveProcessUpdateAndUninstall(t *testing.T) {
	if !platform.GetHostSecurityCapabilities().Supported {
		t.Skip("host sandbox unavailable")
	}
	m, apps, _ := testManager(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	man := testManifest()
	man.Permissions = []string{sdk.CapIPC}
	man.Args = []string{"-test.run=^TestPackageChild$"}
	install(t, m, man, binary, false)
	for _, action := range []string{"failed update", "update", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			proc, err := apps.Launch(context.Background(), man.ID)
			if err != nil {
				t.Fatal(err)
			}
			token, ok := apps.AppToken(man.ID)
			if !ok {
				t.Fatal("missing launch token")
			}
			if action == "failed update" {
				// Bad input must leave the live process and credentials alone.
				if _, err := m.Install(context.Background(), bytes.NewReader([]byte("bad archive")), true, true); err == nil {
					t.Fatal("bad update succeeded")
				}
				if !apps.IsRunning(man.ID) {
					t.Fatal("verification failure stopped live app")
				}
				if _, _, err := apps.TokenStore().Authenticate(token); err != nil {
					t.Fatal("verification failure revoked token")
				}
				m.commitHook = func() error { return errors.New("mid-update failure") }
				man.Version = "2.0.0"
				if _, err := m.Install(context.Background(), bytes.NewReader(archive(t, man, binary, nil)), true, true); err == nil {
					t.Fatal("commit failure succeeded")
				}
				m.commitHook = nil
				got, _ := m.Inspect(man.ID)
				if got.Manifest.Version != "1.0.0" {
					t.Fatal("failed commit replaced old version")
				}
			} else if action == "update" {
				install(t, m, man, binary, true)
			} else {
				if err := m.Uninstall(context.Background(), man.ID); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-proc.Done():
			default:
				t.Fatal("operation returned before process termination")
			}
			if apps.IsRunning(man.ID) {
				t.Fatal("operation left live app")
			}
			if _, _, err := apps.TokenStore().Authenticate(token); err == nil {
				t.Fatal("operation retained credentials")
			}
		})
	}
}
