package document

import (
	"context"
	"fmt"
	"sync"

	"gostalgia/internal/vfs"
)

// Store coordinates document management, recents, favorites, associations,
// permission-aware search, and open-with handoffs.
type Store struct {
	mu           sync.RWMutex
	vfs          vfs.DocumentFS
	grants       *vfs.GrantStore
	recents      *RecentsStore
	favorites    *FavoritesStore
	associations *AssociationTable
	searcher     *Searcher
	handoff      *HandoffManager
}

// NewStore initializes a document management store with all subsystems.
func NewStore(
	dfs vfs.DocumentFS,
	grants *vfs.GrantStore,
	launcher AppLauncher,
	dispatcher IPCDispatcher,
) *Store {
	assoc := NewAssociationTable()
	recents := NewRecentsStore(dfs, grants, DefaultRecentsPath)
	favorites := NewFavoritesStore(dfs, grants, DefaultFavoritesPath)
	searcher := NewSearcher(dfs, grants, recents, favorites)
	handoff := NewHandoffManager(dfs, grants, assoc, recents, launcher, dispatcher)

	return &Store{
		vfs:          dfs,
		grants:       grants,
		recents:      recents,
		favorites:    favorites,
		associations: assoc,
		searcher:     searcher,
		handoff:      handoff,
	}
}

// Init loads persistent recents and favorites from VFS and indexes initial documents.
func (s *Store) Init(ctx context.Context) error {
	_ = s.recents.Load()
	_ = s.favorites.Load()
	_ = s.searcher.RebuildIndex(ctx)
	return nil
}

// SetLifetimeContext propagates the runtime lifetime context to subsystems such as HandoffManager.
func (s *Store) SetLifetimeContext(ctx context.Context) {
	if s.handoff != nil {
		s.handoff.SetLifetimeContext(ctx)
	}
}

// Recents returns the recents store.
func (s *Store) Recents() *RecentsStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.recents
}

// Favorites returns the favorites store.
func (s *Store) Favorites() *FavoritesStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.favorites
}

// Associations returns the document type association table.
func (s *Store) Associations() *AssociationTable {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.associations
}

// Searcher returns the permission-aware searcher and index.
func (s *Store) Searcher() *Searcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.searcher
}

// Handoff returns the open-with handoff manager.
func (s *Store) Handoff() *HandoffManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.handoff
}

// Reload re-reads recents, favorites, and the search index from the VFS
// so in-memory state reflects external changes (for example a backup
// restore).
func (s *Store) Reload(ctx context.Context) error {
	s.mu.RLock()
	recents, favorites, searcher := s.recents, s.favorites, s.searcher
	s.mu.RUnlock()
	_ = recents.Load()
	_ = favorites.Load()
	_ = searcher.RebuildIndex(ctx)
	return nil
}

// SwitchProfile updates the persistent recents, favorites, and search paths for the new profile.
func (s *Store) SwitchProfile(profileID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	recentsPath := fmt.Sprintf("/users/%s/config/recents.json", profileID)
	favoritesPath := fmt.Sprintf("/users/%s/config/favorites.json", profileID)

	s.recents = NewRecentsStore(s.vfs, s.grants, recentsPath)
	s.favorites = NewFavoritesStore(s.vfs, s.grants, favoritesPath)
	_ = s.recents.Load()
	_ = s.favorites.Load()

	s.searcher = NewSearcher(s.vfs, s.grants, s.recents, s.favorites)
	s.searcher.SetProfile(profileID)
	_ = s.searcher.RebuildIndex(context.Background())

	if s.handoff != nil {
		s.handoff.recents = s.recents
	}
	return nil
}
