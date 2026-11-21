package document

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"sync"
	"time"

	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// DefaultRecentsPath is the default location for user recent documents.
const DefaultRecentsPath = "/users/guest/config/recents.json"

// MaxRecents is the upper bound on retained recent documents.
const MaxRecents = 100

// recentsData is the serialized JSON structure for recents.json.
type recentsData struct {
	Version int                  `json:"version"`
	Entries []sdk.RecentDocument `json:"entries"`
}

// RecentsStore manages the user's recently accessed documents with persistence,
// bounded capacity, permission filtering, and stale path verification.
type RecentsStore struct {
	mu         sync.RWMutex
	vfs        vfs.DocumentFS
	grants     *vfs.GrantStore
	storePath  string
	maxEntries int
	entries    []sdk.RecentDocument
}

// NewRecentsStore creates a new recents store.
func NewRecentsStore(dfs vfs.DocumentFS, grants *vfs.GrantStore, storePath string) *RecentsStore {
	if storePath == "" {
		storePath = DefaultRecentsPath
	}
	return &RecentsStore{
		vfs:        dfs,
		grants:     grants,
		storePath:  storePath,
		maxEntries: MaxRecents,
		entries:    make([]sdk.RecentDocument, 0),
	}
}

// Add inserts or updates an entry at the top of the recents list, updating metadata.
func (s *RecentsStore) Add(envPath string, appID string) (*sdk.RecentDocument, error) {
	norm, err := vfs.Normalize(envPath)
	if err != nil {
		return nil, err
	}
	clean := "/" + norm

	s.mu.Lock()
	defer s.mu.Unlock()

	// Stat file to obtain current metadata if VFS is available
	var size int64
	var modTime string
	exists := true
	if s.vfs != nil {
		if fi, err := s.vfs.Stat(clean); err == nil {
			size = fi.Size()
			modTime = fi.ModTime().UTC().Format(time.RFC3339)
			exists = true
		} else if errors.Is(err, fs.ErrNotExist) {
			exists = false
		}
	}

	entry := sdk.RecentDocument{
		Path:       clean,
		AppID:      appID,
		AccessedAt: time.Now().UTC(),
		Exists:     exists,
		Size:       size,
		ModTime:    modTime,
	}

	// Remove any existing entry for this path
	var updated []sdk.RecentDocument
	updated = append(updated, entry)
	for _, e := range s.entries {
		if e.Path != clean {
			updated = append(updated, e)
		}
	}

	// Bound to maxEntries
	if len(updated) > s.maxEntries {
		updated = updated[:s.maxEntries]
	}
	s.entries = updated

	_ = s.saveLocked()
	return &entry, nil
}

// Remove deletes a document path from the recents list.
func (s *RecentsStore) Remove(envPath string) error {
	norm, err := vfs.Normalize(envPath)
	if err != nil {
		return err
	}
	clean := "/" + norm

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]sdk.RecentDocument, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Path != clean {
			filtered = append(filtered, e)
		}
	}
	s.entries = filtered
	return s.saveLocked()
}

// Clear removes all entries from the recents list.
func (s *RecentsStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make([]sdk.RecentDocument, 0)
	return s.saveLocked()
}

// List returns recent documents, filtered by caller permissions and optionally checking existence.
func (s *RecentsStore) List(callerAppID string, verifyExists bool) []sdk.RecentDocument {
	s.mu.RLock()
	raw := make([]sdk.RecentDocument, len(s.entries))
	copy(raw, s.entries)
	s.mu.RUnlock()

	var out []sdk.RecentDocument
	for _, e := range raw {
		// Permission check: if caller is an app, verify read access
		if callerAppID != "" && s.grants != nil {
			if err := s.grants.CheckAccess(callerAppID, e.Path, vfs.AccessRead); err != nil {
				continue // Caller not authorized to see this document
			}
		}

		if verifyExists && s.vfs != nil {
			if fi, err := s.vfs.Stat(e.Path); err == nil {
				e.Exists = true
				e.Size = fi.Size()
				e.ModTime = fi.ModTime().UTC().Format(time.RFC3339)
			} else {
				e.Exists = false
			}
		}

		out = append(out, e)
	}
	return out
}

// PruneMissing removes all entries whose files no longer exist in the VFS.
func (s *RecentsStore) PruneMissing() (int, error) {
	if s.vfs == nil {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var pruned int
	filtered := make([]sdk.RecentDocument, 0, len(s.entries))
	for _, e := range s.entries {
		if _, err := s.vfs.Stat(e.Path); err == nil {
			filtered = append(filtered, e)
		} else {
			pruned++
		}
	}
	s.entries = filtered
	if pruned > 0 {
		_ = s.saveLocked()
	}
	return pruned, nil
}

// Load restores the recents store from persistent state in VFS.
func (s *RecentsStore) Load() error {
	if s.vfs == nil {
		return nil
	}
	data, err := s.vfs.ReadFile(s.storePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Missing file means empty persisted state; the in-memory
			// snapshot must reflect it so a reload does not resurrect
			// entries the document no longer contains.
			s.mu.Lock()
			s.entries = nil
			s.mu.Unlock()
			return nil
		}
		return err
	}

	var d recentsData
	if err := json.Unmarshal(data, &d); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = d.Entries
	if len(s.entries) > s.maxEntries {
		s.entries = s.entries[:s.maxEntries]
	}
	return nil
}

// saveLocked persists the current entries to VFS using SaveAtomic.
func (s *RecentsStore) saveLocked() error {
	if s.vfs == nil {
		return nil
	}
	d := recentsData{
		Version: 1,
		Entries: s.entries,
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	// Ensure parent directory exists
	dir := path.Dir(s.storePath)
	if err := s.vfs.MkdirAll(dir); err != nil {
		return err
	}
	return s.vfs.SaveAtomic(s.storePath, data, 0o644)
}
