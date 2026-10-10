package pkg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// state.json is the sole commit point. Versions are immutable directories, so
// readers see the complete old or new version, never a partially moved tree.
type diskState struct {
	FormatVersion int    `json:"format_version"`
	Active        string `json:"active"`
	Previous      string `json:"previous,omitempty"`
}

type installed struct {
	state    diskState
	active   *verified
	previous *verified
}

type Info struct {
	Manifest        sdk.Manifest   `json:"manifest"`
	Provenance      Provenance     `json:"provenance"`
	PreviousVersion string         `json:"previous_version,omitempty"`
	Permissions     PermissionDiff `json:"permission_diff"`
	PathGrants      []vfs.Grant    `json:"active_path_grants,omitempty"`
}

type ConfirmationError struct{ Diff PermissionDiff }

func (e *ConfirmationError) Error() string {
	b, _ := json.Marshal(e.Diff)
	return "package: permission expansion requires explicit confirm_permissions: " + string(b)
}

type Manager struct {
	mu         sync.Mutex
	operations sync.Mutex // mutations serialize without blocking read callbacks during Stop
	root       *os.Root
	apps       *app.Manager
	trust      TrustStore
	installed  map[string]*installed
	// commitHook injects failures after staging, before the atomic commit.
	commitHook func() error
}

// NewManager opens the host /apps backing directory. Trust is supplied by the
// operator, never by a package. Installed payloads are reverified on every boot.
func NewManager(appsDir string, apps *app.Manager, trust TrustStore) (*Manager, error) {
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(appsDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{root: root, apps: apps, trust: make(TrustStore), installed: make(map[string]*installed)}
	for id, key := range trust {
		m.trust[id] = append([]byte(nil), key...)
	}
	if err := m.load(); err != nil {
		root.Close()
		return nil, err
	}
	return m, nil
}

func (m *Manager) Close() error {
	m.operations.Lock()
	defer m.operations.Unlock()
	return m.root.Close()
}

func generation() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func validGeneration(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == s
}

func (m *Manager) versionManifest(id, gen string, v *verified) sdk.Manifest {
	man := v.Manifest
	man.Executable = filepath.Join(m.root.Name(), id, "versions", gen, filepath.FromSlash(man.Executable))
	return man
}

func (m *Manager) loadVersion(id, gen string) (*verified, error) {
	if !validGeneration(gen) {
		return nil, fmt.Errorf("package: invalid version pointer")
	}
	root, err := m.root.OpenRoot(path.Join(id, "versions", gen))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	v := &verified{files: make(map[string][]byte)}
	var total int64
	count := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		count++
		if count > MaxFiles || !portablePath(name) {
			return fmt.Errorf("package: invalid installed entry")
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxExtractBytes-total {
			return fmt.Errorf("package: invalid installed file")
		}
		if (name == "manifest.json" || name == "package.json") && info.Size() > maxJSONBytes {
			return fmt.Errorf("package: installed metadata exceeds budget")
		}
		total += info.Size()
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		b, readErr := io.ReadAll(io.LimitReader(f, info.Size()+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || int64(len(b)) != info.Size() {
			return fmt.Errorf("package: corrupt installed file")
		}
		v.files[name] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	v, err = validate(v, m.trust)
	if err != nil {
		return nil, err
	}
	if v.Manifest.ID != id {
		return nil, fmt.Errorf("package: installed identity mismatch")
	}
	return v, nil
}

func (m *Manager) load() error {
	entries, err := fs.ReadDir(m.root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		// Garbage-collect install/uninstall staging directories orphaned by a
		// crash between staging and the atomic rename.
		if gen, ok := strings.CutPrefix(name, ".staging-"); ok && validGeneration(gen) {
			_ = m.root.RemoveAll(name)
			continue
		}
		if gen, ok := strings.CutPrefix(name, ".removed-"); ok && validGeneration(gen) {
			_ = m.root.RemoveAll(name)
			continue
		}
		if !entry.IsDir() || !sdk.ValidAppID(name) {
			continue
		}
		id := name
		// A crash between staging and the rename can orphan the staged
		// state.json file; it is never a commit point.
		if appEntries, err := fs.ReadDir(m.root.FS(), id); err == nil {
			for _, appEntry := range appEntries {
				if gen, ok := strings.CutPrefix(appEntry.Name(), ".state-"); ok && validGeneration(gen) {
					_ = m.root.Remove(path.Join(id, appEntry.Name()))
				}
			}
		}
		stateFile := path.Join(id, "state.json")
		info, err := m.root.Lstat(stateFile)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxJSONBytes {
			return fmt.Errorf("package: invalid state file")
		}
		data, err := m.root.ReadFile(stateFile)
		if err != nil {
			return err
		}
		var state diskState
		if err := decodeJSON(data, &state); err != nil {
			return err
		}
		if state.FormatVersion != FormatVersion {
			return fmt.Errorf("package: unsupported installed state")
		}
		if state.Previous != "" && state.Previous == state.Active {
			return fmt.Errorf("package: active and previous pointers must differ")
		}
		active, err := m.loadVersion(id, state.Active)
		if err != nil {
			return fmt.Errorf("package: load %s: %w", id, err)
		}
		rec := &installed{state: state, active: active}
		if state.Previous != "" {
			previous, err := m.loadVersion(id, state.Previous)
			if err != nil {
				// The previous pointer exists only for rollback; a corrupt or
				// missing rollback artifact must not brick the environment
				// boot while the active version is intact.
				slog.Warn("package: dropping unusable previous version; rollback unavailable",
					"id", id, "generation", state.Previous, "error", err)
			} else {
				previous.files = nil
				rec.previous = previous
			}
		}
		active.files = nil
		if err := m.apps.Registry().RegisterExternal(m.versionManifest(id, state.Active, active)); err != nil {
			return err
		}
		m.installed[id] = rec
		finish, err := m.prepareGrants(id, active.Manifest)
		if err != nil {
			return err
		}
		finish(true)
	}
	return nil
}

// Grants are prepared while launches are blocked and old credentials revoked.
// A failed transaction revokes only the new grants, leaving old grants intact.
func (m *Manager) prepareGrants(id string, man sdk.Manifest) (func(bool), error) {
	if gs := m.apps.GrantStore(); gs != nil {
		old := gs.List(id)
		var created []string
		for _, grant := range man.PathGrants {
			g, err := gs.Issue(id, grant.Path, vfs.AccessMode(grant.Access), grant.Recursive)
			if err != nil {
				for _, id := range created {
					_ = gs.Revoke(id)
				}
				return nil, err
			}
			created = append(created, g.ID)
		}
		return func(commit bool) {
			if commit {
				for _, g := range old {
					_ = gs.Revoke(g.ID)
				}
			} else {
				for _, id := range created {
					_ = gs.Revoke(id)
				}
			}
		}, nil
	}
	if len(man.PathGrants) > 0 {
		return nil, fmt.Errorf("package: path grant store is unavailable")
	}
	return func(bool) {}, nil
}

func (m *Manager) info(rec *installed, candidate *verified) Info {
	v := rec.active
	var diff PermissionDiff
	if candidate != nil {
		v = candidate
		diff = DiffPermissions(rec.active.Manifest, v.Manifest)
	}
	out := Info{Manifest: v.Manifest, Provenance: v.Provenance, Permissions: diff}
	if rec.previous != nil {
		out.PreviousVersion = rec.previous.Manifest.Version
	}
	if gs := m.apps.GrantStore(); gs != nil {
		for _, g := range gs.List(v.Manifest.ID) {
			if !g.Revoked {
				out.PathGrants = append(out.PathGrants, g)
			}
		}
	}
	// Return detached documents, not mutable aliases into the installed state.
	b, _ := json.Marshal(out)
	var detached Info
	_ = json.Unmarshal(b, &detached)
	return detached
}

func (m *Manager) List() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.installed))
	for _, rec := range m.installed {
		out = append(out, m.info(rec, nil))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.ID < out[j].Manifest.ID })
	return out
}

func (m *Manager) Inspect(id string) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.installed[id]
	if !ok {
		return Info{}, fmt.Errorf("package: %q is not installed", id)
	}
	return m.info(rec, nil), nil
}

func (m *Manager) InspectArchive(r io.Reader) (Info, error) {
	v, err := verify(r, m.trust)
	if err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.installed[v.Manifest.ID]
	if rec == nil {
		rec = &installed{active: &verified{}}
	}
	return m.info(rec, v), nil
}

func (m *Manager) writeFile(root *os.Root, name string, data []byte, mode fs.FileMode) error {
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func (m *Manager) commit(id string, state diskState) error {
	if m.commitHook != nil {
		if err := m.commitHook(); err != nil {
			return err
		}
	}
	gen, err := generation()
	if err != nil {
		return err
	}
	tmp := path.Join(id, ".state-"+gen)
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := m.writeFile(m.root, tmp, b, 0o600); err != nil {
		return err
	}
	defer m.root.Remove(tmp)
	return m.root.Rename(tmp, path.Join(id, "state.json"))
}

func (m *Manager) Install(ctx context.Context, r io.Reader, update, confirm bool) (Info, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	v, err := verify(r, m.trust)
	if err != nil {
		return Info{}, err
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	id := v.Manifest.ID
	old := m.record(id)
	if update && old == nil {
		return Info{}, fmt.Errorf("package: %s is not installed", id)
	}
	if !update {
		if _, exists := m.apps.Registry().Manifest(id); exists || old != nil {
			return Info{}, fmt.Errorf("package: %s already exists; use update", id)
		}
		if _, err := m.root.Lstat(id); !errors.Is(err, fs.ErrNotExist) {
			return Info{}, fmt.Errorf("package: target directory already exists")
		}
	}
	var oldManifest sdk.Manifest
	if old != nil {
		oldManifest = old.active.Manifest
	}
	diff := DiffPermissions(oldManifest, v.Manifest)
	if diff.Expansion && !confirm {
		return Info{}, &ConfirmationError{Diff: diff}
	}
	gen, err := generation()
	if err != nil {
		return Info{}, err
	}
	stage := ".staging-" + gen
	if err := m.root.Mkdir(stage, 0o700); err != nil {
		return Info{}, err
	}
	defer m.root.RemoveAll(stage)
	staging, err := m.root.OpenRoot(stage)
	if err != nil {
		return Info{}, err
	}
	names := make([]string, 0, len(v.files))
	for name := range v.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err = ctx.Err(); err != nil {
			break
		}
		if err = staging.MkdirAll(path.Dir(name), 0o700); err != nil {
			break
		}
		mode := fs.FileMode(0o600)
		if name == v.Manifest.Executable {
			mode = 0o700
		}
		if err = m.writeFile(staging, name, v.files[name], mode); err != nil {
			break
		}
	}
	err = errors.Join(err, staging.Close())
	if err != nil {
		return Info{}, err
	}
	// Do not stop a live app until every input is verified and fully staged.
	release, err := m.apps.BeginMaintenance(id, 5*time.Second)
	if err != nil {
		return Info{}, err
	}
	defer release()
	finishGrants, err := m.prepareGrants(id, v.Manifest)
	if err != nil {
		return Info{}, err
	}
	grantsCommitted := false
	defer func() {
		if !grantsCommitted {
			finishGrants(false)
		}
	}()
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	committed := false
	if old == nil {
		// An exclusive directory creation closes the preflight/extraction race.
		if err := m.root.Mkdir(id, 0o700); err != nil {
			return Info{}, err
		}
		defer func() {
			if !committed {
				_ = m.root.RemoveAll(id)
			}
		}()
	}
	if err := m.root.MkdirAll(path.Join(id, "versions"), 0o700); err != nil {
		return Info{}, err
	}
	target := path.Join(id, "versions", gen)
	if err := m.root.Rename(stage, target); err != nil {
		return Info{}, err
	}
	defer func() {
		if !committed {
			_ = m.root.RemoveAll(target)
		}
	}()
	state := diskState{FormatVersion: FormatVersion, Active: gen}
	if old != nil {
		state.Previous = old.state.Active
	}
	if err := m.commit(id, state); err != nil {
		return Info{}, err
	}
	man := m.versionManifest(id, gen, v)
	if old == nil {
		err = m.apps.Registry().RegisterExternal(man)
	} else {
		err = m.apps.Registry().ReplaceExternal(man)
	}
	if err != nil {
		// No launch can observe the new pointer while maintenance holds.
		if old != nil {
			if restoreErr := m.commit(id, old.state); restoreErr != nil {
				committed = true // retain both complete versions for recovery
				return Info{}, errors.Join(err, restoreErr)
			}
		}
		return Info{}, err
	}
	committed = true
	v.files = nil // retain permission/provenance metadata, not entire binaries
	rec := &installed{state: state, active: v}
	if old != nil {
		rec.previous = old.active
	}
	m.mu.Lock()
	m.installed[id] = rec
	m.mu.Unlock()
	grantsCommitted = true
	finishGrants(true)
	if old != nil && old.state.Previous != "" {
		_ = m.root.RemoveAll(path.Join(id, "versions", old.state.Previous))
	}
	out := m.info(rec, nil)
	out.Permissions = diff
	return out, nil
}

func (m *Manager) Rollback(ctx context.Context, id string, confirm bool) (Info, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	rec := m.record(id)
	if rec == nil || rec.previous == nil {
		return Info{}, fmt.Errorf("package: no previous version for %q", id)
	}
	// Reverify the on-disk previous version before stopping a live app.
	previous, err := m.loadVersion(id, rec.state.Previous)
	if err != nil {
		return Info{}, err
	}
	diff := DiffPermissions(rec.active.Manifest, previous.Manifest)
	if diff.Expansion && !confirm {
		return Info{}, &ConfirmationError{Diff: diff}
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	release, err := m.apps.BeginMaintenance(id, 5*time.Second)
	if err != nil {
		return Info{}, err
	}
	defer release()
	finishGrants, err := m.prepareGrants(id, previous.Manifest)
	if err != nil {
		return Info{}, err
	}
	grantsCommitted := false
	defer func() {
		if !grantsCommitted {
			finishGrants(false)
		}
	}()
	state := diskState{FormatVersion: FormatVersion, Active: rec.state.Previous, Previous: rec.state.Active}
	if err := m.commit(id, state); err != nil {
		return Info{}, err
	}
	if err := m.apps.Registry().ReplaceExternal(m.versionManifest(id, state.Active, previous)); err != nil {
		return Info{}, errors.Join(err, m.commit(id, rec.state))
	}
	previous.files = nil
	next := &installed{state: state, active: previous, previous: rec.active}
	m.mu.Lock()
	m.installed[id] = next
	m.mu.Unlock()
	grantsCommitted = true
	finishGrants(true)
	out := m.info(next, nil)
	out.Permissions = diff
	return out, nil
}

func (m *Manager) Uninstall(ctx context.Context, id string) error {
	m.operations.Lock()
	defer m.operations.Unlock()
	if m.record(id) == nil {
		return fmt.Errorf("package: %q is not installed", id)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := m.apps.BeginMaintenance(id, 5*time.Second)
	if err != nil {
		return err
	}
	defer release()
	gen, err := generation()
	if err != nil {
		return err
	}
	removed := ".removed-" + gen
	if err := m.root.Rename(id, removed); err != nil {
		return err
	}
	m.apps.Registry().RemoveExternal(id)
	m.mu.Lock()
	delete(m.installed, id)
	m.mu.Unlock()
	if gs := m.apps.GrantStore(); gs != nil {
		gs.RevokeApp(id)
	}
	// The rename is the uninstall commit; cleanup failure cannot resurrect it.
	_ = m.root.RemoveAll(removed)
	return nil
}

func (m *Manager) record(id string) *installed {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.installed[id]
}
