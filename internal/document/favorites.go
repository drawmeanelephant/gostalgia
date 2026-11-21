package document

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"sync"
	"time"

	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// DefaultFavoritesPath is the default location for user favorite documents.
const DefaultFavoritesPath = "/users/guest/config/favorites.json"

// MaxFavorites is the upper bound on retained favorite documents.
const MaxFavorites = 100

// favoritesData is the serialized JSON structure for favorites.json.
type favoritesData struct {
	Version int                    `json:"version"`
	Entries []sdk.FavoriteDocument `json:"entries"`
}

// FavoritesStore manages the user's pinned/favorite documents with persistence,
// bounded capacity, rank ordering, permission filtering, and stale path verification.
type FavoritesStore struct {
	mu         sync.RWMutex
	vfs        vfs.DocumentFS
	grants     *vfs.GrantStore
	storePath  string
	maxEntries int
	entries    []sdk.FavoriteDocument
}

// NewFavoritesStore creates a new favorites store.
func NewFavoritesStore(dfs vfs.DocumentFS, grants *vfs.GrantStore, storePath string) *FavoritesStore {
	if storePath == "" {
		storePath = DefaultFavoritesPath
	}
	return &FavoritesStore{
		vfs:        dfs,
		grants:     grants,
		storePath:  storePath,
		maxEntries: MaxFavorites,
		entries:    make([]sdk.FavoriteDocument, 0),
	}
}

// Add adds or updates a favorite document with an optional custom label.
func (s *FavoritesStore) Add(envPath string, label string) (*sdk.FavoriteDocument, error) {
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

	if label == "" {
		label = path.Base(clean)
	}

	// Check if already exists; if so, update label/modTime
	for i, e := range s.entries {
		if e.Path == clean {
			s.entries[i].Label = label
			s.entries[i].Exists = exists
			s.entries[i].Size = size
			s.entries[i].ModTime = modTime
			_ = s.saveLocked()
			res := s.entries[i]
			return &res, nil
		}
	}

	if len(s.entries) >= s.maxEntries {
		return nil, fmt.Errorf("favorites: maximum capacity (%d) reached", s.maxEntries)
	}

	entry := sdk.FavoriteDocument{
		Path:    clean,
		Label:   label,
		AddedAt: time.Now().UTC(),
		Rank:    len(s.entries),
		Exists:  exists,
		Size:    size,
		ModTime: modTime,
	}

	s.entries = append(s.entries, entry)
	_ = s.saveLocked()
	return &entry, nil
}

// Remove deletes a document path from the favorites list.
func (s *FavoritesStore) Remove(envPath string) error {
	norm, err := vfs.Normalize(envPath)
	if err != nil {
		return err
	}
	clean := "/" + norm

	s.mu.Lock()
	defer s.mu.Unlock()

	filtered := make([]sdk.FavoriteDocument, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Path != clean {
			filtered = append(filtered, e)
		}
	}
	// Re-assign ranks
	for i := range filtered {
		filtered[i].Rank = i
	}
	s.entries = filtered
	return s.saveLocked()
}

// Reorder rearranges favorites according to the provided path list.
func (s *FavoritesStore) Reorder(orderedPaths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	byPath := make(map[string]sdk.FavoriteDocument)
	for _, e := range s.entries {
		byPath[e.Path] = e
	}

	var reordered []sdk.FavoriteDocument
	seen := make(map[string]bool)

	for _, p := range orderedPaths {
		norm, err := vfs.Normalize(p)
		if err != nil {
			continue
		}
		clean := "/" + norm
		if entry, ok := byPath[clean]; ok && !seen[clean] {
			entry.Rank = len(reordered)
			reordered = append(reordered, entry)
			seen[clean] = true
		}
	}

	// Append any favorites that weren't in orderedPaths
	for _, e := range s.entries {
		if !seen[e.Path] {
			e.Rank = len(reordered)
			reordered = append(reordered, e)
		}
	}

	s.entries = reordered
	return s.saveLocked()
}

// Clear removes all entries from the favorites list.
func (s *FavoritesStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make([]sdk.FavoriteDocument, 0)
	return s.saveLocked()
}

// List returns favorite documents sorted by rank, filtered by caller permissions.
func (s *FavoritesStore) List(callerAppID string, verifyExists bool) []sdk.FavoriteDocument {
	s.mu.RLock()
	raw := make([]sdk.FavoriteDocument, len(s.entries))
	copy(raw, s.entries)
	s.mu.RUnlock()

	sort.Slice(raw, func(i, j int) bool {
		return raw[i].Rank < raw[j].Rank
	})

	var out []sdk.FavoriteDocument
	for _, e := range raw {
		if callerAppID != "" && s.grants != nil {
			if err := s.grants.CheckAccess(callerAppID, e.Path, vfs.AccessRead); err != nil {
				continue // Caller not authorized
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

// Load restores favorites from persistent state in VFS.
func (s *FavoritesStore) Load() error {
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

	var d favoritesData
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

// saveLocked persists favorites to VFS using SaveAtomic.
func (s *FavoritesStore) saveLocked() error {
	if s.vfs == nil {
		return nil
	}
	d := favoritesData{
		Version: 1,
		Entries: s.entries,
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	dir := path.Dir(s.storePath)
	if err := s.vfs.MkdirAll(dir); err != nil {
		return err
	}
	return s.vfs.SaveAtomic(s.storePath, data, 0o644)
}
