// Package profile manages personal user profiles, directory isolation,
// and profile switching in Gostalgia.
package profile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gostalgia/internal/events"
	"gostalgia/internal/security"
	"gostalgia/internal/vfs"
)

// DefaultProfilesPath is the persistent VFS path for profile registry state.
const DefaultProfilesPath = "/config/profiles.json"

// DefaultProfileID is the initial guest user profile.
const DefaultProfileID = "guest"

var validIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Profile represents a defined personal workspace profile identity.
type Profile struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Avatar      string         `json:"avatar,omitempty"`
	Preferences map[string]any `json:"preferences,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// User converts a Profile to a security.User identity.
func (p Profile) User() security.User {
	return security.User{
		ID:   "u-" + p.ID,
		Name: p.ID,
	}
}

// ValidateID checks whether a profile identifier is safe and valid.
func ValidateID(id string) error {
	normalized := strings.TrimSpace(strings.ToLower(id))
	if normalized == "" {
		return errors.New("profile id cannot be empty")
	}
	if normalized == "." || normalized == ".." {
		return errors.New("invalid profile id: path segments not allowed")
	}
	if strings.ContainsAny(normalized, "/\\: \t\r\n") {
		return errors.New("invalid profile id: cannot contain slashes or whitespace")
	}
	if !validIDPattern.MatchString(normalized) {
		return errors.New("invalid profile id: must be 1-32 lowercase alphanumeric characters, dashes, or underscores, starting with an alphanumeric")
	}
	return nil
}

// Path helpers for profile isolation under /users/<profile_id>/
func UserDir(id string) string       { return "/users/" + id }
func DocumentsDir(id string) string  { return "/users/" + id + "/documents" }
func DownloadsDir(id string) string  { return "/users/" + id + "/downloads" }
func DesktopDir(id string) string    { return "/users/" + id + "/desktop" }
func ConfigDir(id string) string     { return "/users/" + id + "/config" }
func TrashDir(id string) string      { return "/users/" + id + "/.trash" }
func SettingsPath(id string) string  { return "/users/" + id + "/config/settings.json" }
func WorkspacePath(id string) string { return "/users/" + id + "/config/workspace.json" }
func RecentsPath(id string) string   { return "/users/" + id + "/config/recents.json" }
func FavoritesPath(id string) string { return "/users/" + id + "/config/favorites.json" }

// ProfileEvent is emitted on profile creation or deletion.
type ProfileEvent struct {
	Action  string  `json:"action"` // "created", "deleted", "updated"
	Profile Profile `json:"profile"`
}

func (ProfileEvent) Type() string { return "profile.event" }

// SwitchedEvent is emitted on profile switch.
type SwitchedEvent struct {
	PreviousID string  `json:"previous_id"`
	ActiveID   string  `json:"active_id"`
	Profile    Profile `json:"profile"`
}

func (SwitchedEvent) Type() string { return "profile.switched" }

type serializedState struct {
	Version   int                `json:"version"`
	ActiveID  string             `json:"active_id"`
	Profiles  map[string]Profile `json:"profiles"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// Manager manages profiles, active selection, directory trees, and lifecycle events.
type Manager struct {
	mu        sync.RWMutex
	fsys      vfs.FS
	storePath string
	bus       *events.Bus
	log       *slog.Logger
	profiles  map[string]Profile
	activeID  string
	onSwitch  []func(prev, next Profile)
}

// NewManager creates a profile Manager. If storePath is empty, DefaultProfilesPath is used.
func NewManager(fsys vfs.FS, storePath string, bus *events.Bus, log *slog.Logger) (*Manager, error) {
	if storePath == "" {
		storePath = DefaultProfilesPath
	}
	if log == nil {
		log = slog.Default()
	}

	m := &Manager{
		fsys:      fsys,
		storePath: storePath,
		bus:       bus,
		log:       log,
		profiles:  make(map[string]Profile),
		activeID:  DefaultProfileID,
	}

	if err := m.load(); err != nil {
		log.Warn("profiles: failed to load state, restoring default guest profile", "error", err)
		m.resetDefault()
		_ = m.saveLocked()
	}

	// Ensure directories for all registered profiles exist
	m.mu.RLock()
	for id := range m.profiles {
		_ = m.ensureDirsLocked(id)
	}
	m.mu.RUnlock()

	return m, nil
}

func (m *Manager) resetDefault() {
	now := time.Now().UTC()
	m.profiles = map[string]Profile{
		DefaultProfileID: {
			ID:        DefaultProfileID,
			Name:      "Guest",
			CreatedAt: now,
			UpdatedAt: now,
		},
	}
	m.activeID = DefaultProfileID
}

func fsPath(p string) string {
	cleaned := path.Clean(p)
	return strings.TrimPrefix(cleaned, "/")
}

func (m *Manager) load() error {
	if m.fsys == nil {
		m.resetDefault()
		return nil
	}

	rel := fsPath(m.storePath)
	data, err := m.fsys.ReadFile(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			m.resetDefault()
			return m.saveLocked()
		}
		return fmt.Errorf("read profiles: %w", err)
	}

	var state serializedState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("parse profiles json: %w", err)
	}

	if len(state.Profiles) == 0 {
		m.resetDefault()
		return nil
	}

	m.profiles = state.Profiles
	if _, ok := m.profiles[state.ActiveID]; ok {
		m.activeID = state.ActiveID
	} else if _, ok := m.profiles[DefaultProfileID]; ok {
		m.activeID = DefaultProfileID
	} else {
		for id := range m.profiles {
			m.activeID = id
			break
		}
	}

	return nil
}

func (m *Manager) saveLocked() error {
	if m.fsys == nil {
		return nil
	}

	state := serializedState{
		Version:   1,
		ActiveID:  m.activeID,
		Profiles:  m.profiles,
		UpdatedAt: time.Now().UTC(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profiles: %w", err)
	}

	dir := path.Dir(m.storePath)
	if dir != "." && dir != "/" {
		_ = m.fsys.MkdirAll(fsPath(dir))
	}

	if err := m.fsys.SaveAtomic(fsPath(m.storePath), data, 0o644); err != nil {
		return fmt.Errorf("save profiles atomic: %w", err)
	}
	return nil
}

func (m *Manager) ensureDirsLocked(id string) error {
	if m.fsys == nil {
		return nil
	}
	dirs := []string{
		UserDir(id),
		DocumentsDir(id),
		DownloadsDir(id),
		DesktopDir(id),
		ConfigDir(id),
		TrashDir(id),
	}
	for _, d := range dirs {
		if err := m.fsys.MkdirAll(fsPath(d)); err != nil {
			return err
		}
	}
	return nil
}

// Reload re-reads profile definitions and active profile from VFS.
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

// EnsureDirectories ensures that the user directory structure for profile id exists.
func (m *Manager) EnsureDirectories(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ensureDirsLocked(id)
}

// List returns all registered profiles sorted by ID.
func (m *Manager) List() []Profile {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]Profile, 0, len(m.profiles))
	for _, p := range m.profiles {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns the profile with the given ID.
func (m *Manager) Get(id string) (Profile, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.profiles[strings.ToLower(id)]
	return p, ok
}

// Active returns the currently active profile.
func (m *Manager) Active() Profile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p, ok := m.profiles[m.activeID]; ok {
		return p
	}
	return Profile{ID: DefaultProfileID, Name: "Guest"}
}

// ActiveID returns the ID of the currently active profile.
func (m *Manager) ActiveID() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.activeID
}

// OnSwitch registers a callback for profile switch events.
func (m *Manager) OnSwitch(fn func(prev, next Profile)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSwitch = append(m.onSwitch, fn)
}

// Create registers a new profile and initializes its directory structure.
func (m *Manager) Create(id, name string) (Profile, error) {
	return m.CreateDetailed(id, name, "", "", nil)
}

// CreateDetailed registers a new profile with rich metadata and initializes its directory structure.
func (m *Manager) CreateDetailed(id, name, description, avatar string, preferences map[string]any) (Profile, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	if err := ValidateID(id); err != nil {
		return Profile{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.Title(id)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.profiles[id]; exists {
		return Profile{}, fmt.Errorf("profile %q already exists", id)
	}

	now := time.Now().UTC()
	var prefsCopy map[string]any
	if preferences != nil {
		prefsCopy = make(map[string]any, len(preferences))
		for k, v := range preferences {
			prefsCopy[k] = v
		}
	}

	p := Profile{
		ID:          id,
		Name:        name,
		Description: strings.TrimSpace(description),
		Avatar:      strings.TrimSpace(avatar),
		Preferences: prefsCopy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	m.profiles[id] = p
	if err := m.ensureDirsLocked(id); err != nil {
		delete(m.profiles, id)
		return Profile{}, fmt.Errorf("initialize profile directories: %w", err)
	}

	if err := m.saveLocked(); err != nil {
		delete(m.profiles, id)
		return Profile{}, fmt.Errorf("persist profile: %w", err)
	}

	if m.bus != nil {
		m.bus.Publish("profile", ProfileEvent{
			Action:  "created",
			Profile: p,
		})
	}
	m.log.Info("profile created", "id", p.ID, "name", p.Name)
	return p, nil
}

// Switch switches the active profile to targetID.
func (m *Manager) Switch(targetID string) (Profile, error) {
	targetID = strings.TrimSpace(strings.ToLower(targetID))
	m.mu.Lock()
	prevProfile := m.profiles[m.activeID]
	nextProfile, ok := m.profiles[targetID]
	if !ok {
		m.mu.Unlock()
		return Profile{}, fmt.Errorf("profile %q not found", targetID)
	}

	if m.activeID == targetID {
		m.mu.Unlock()
		return nextProfile, nil
	}

	prevID := m.activeID
	m.activeID = targetID
	_ = m.ensureDirsLocked(targetID)

	if err := m.saveLocked(); err != nil {
		m.activeID = prevID
		m.mu.Unlock()
		return Profile{}, fmt.Errorf("persist active profile: %w", err)
	}

	callbacks := make([]func(prev, next Profile), len(m.onSwitch))
	copy(callbacks, m.onSwitch)
	m.mu.Unlock()

	for _, cb := range callbacks {
		cb(prevProfile, nextProfile)
	}

	if m.bus != nil {
		m.bus.Publish("profile", SwitchedEvent{
			PreviousID: prevID,
			ActiveID:   nextProfile.ID,
			Profile:    nextProfile,
		})
	}
	m.log.Info("profile switched", "previous", prevID, "active", nextProfile.ID)
	return nextProfile, nil
}

// Update modifies an existing profile's metadata.
func (m *Manager) Update(id string, name, description, avatar string, preferences map[string]any) (Profile, error) {
	id = strings.TrimSpace(strings.ToLower(id))
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.profiles[id]
	if !ok {
		return Profile{}, fmt.Errorf("profile %q not found", id)
	}

	if name != "" {
		p.Name = strings.TrimSpace(name)
	}
	if description != "" {
		p.Description = strings.TrimSpace(description)
	}
	if avatar != "" {
		p.Avatar = strings.TrimSpace(avatar)
	}
	if preferences != nil {
		if p.Preferences == nil {
			p.Preferences = make(map[string]any)
		}
		for k, v := range preferences {
			p.Preferences[k] = v
		}
	}
	p.UpdatedAt = time.Now().UTC()

	m.profiles[id] = p
	if err := m.saveLocked(); err != nil {
		return Profile{}, fmt.Errorf("save updated profile: %w", err)
	}

	if m.bus != nil {
		m.bus.Publish("profile", ProfileEvent{
			Action:  "updated",
			Profile: p,
		})
	}
	m.log.Info("profile updated", "id", id)
	return p, nil
}

// Delete removes a profile and its data tree. The active profile and the
// last remaining profile cannot be deleted. Deleting a profile also removes
// its entire /users/<id>/ tree — documents, config (including workspace
// history), and trash — so private data does not outlive the profile. A
// missing tree is not an error; a removal failure is reported after the
// registry change has already committed.
func (m *Manager) Delete(id string) error {
	id = strings.TrimSpace(strings.ToLower(id))
	m.mu.Lock()
	defer m.mu.Unlock()

	p, ok := m.profiles[id]
	if !ok {
		return fmt.Errorf("profile %q not found", id)
	}
	if id == m.activeID {
		return fmt.Errorf("cannot delete currently active profile %q; switch profiles first", id)
	}
	if len(m.profiles) <= 1 {
		return errors.New("cannot delete the only remaining profile")
	}

	delete(m.profiles, id)
	if err := m.saveLocked(); err != nil {
		m.profiles[id] = p
		return fmt.Errorf("save after delete: %w", err)
	}

	if m.fsys != nil {
		if err := m.fsys.RemoveAll(fsPath(UserDir(id))); err != nil {
			return fmt.Errorf("profile %q deleted but removing data tree %s failed: %w", id, UserDir(id), err)
		}
	}

	if m.bus != nil {
		m.bus.Publish("profile", ProfileEvent{
			Action:  "deleted",
			Profile: p,
		})
	}
	m.log.Info("profile deleted", "id", id)
	return nil
}
