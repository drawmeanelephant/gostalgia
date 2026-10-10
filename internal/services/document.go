package services

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	"gostalgia/internal/document"
	"gostalgia/internal/ipc"
	"gostalgia/internal/profile"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/vfs"
	"gostalgia/sdk"
)

// DocumentService exposes document search, recents, favorites, associations,
// and open-with handoffs over IPC ("doc/*").
type DocumentService struct {
	ctx      *service.Context
	store    *document.Store
	grants   *vfs.GrantStore
	lifetime context.Context
}

func NewDocument() *DocumentService {
	return &DocumentService{}
}

func (s *DocumentService) Name() string {
	return "doc"
}

func (s *DocumentService) Depends() []string {
	return []string{"fs"}
}

func (s *DocumentService) Store() *document.Store {
	return s.store
}

func (s *DocumentService) Init(ctx *service.Context) error {
	s.ctx = ctx

	var dfs vfs.DocumentFS
	if v, ok := ctx.VFS.(vfs.DocumentFS); ok {
		dfs = v
	}

	var grants *vfs.GrantStore
	if v, ok := ctx.VFS.(*vfs.VFS); ok {
		grants = v.Grants()
	}
	s.grants = grants

	s.store = document.NewStore(dfs, grants, ctx.Apps, ctx.Router)

	// Register document associations from installed application manifests
	if ctx.Apps != nil {
		for _, st := range ctx.Apps.List() {
			s.store.Associations().RegisterFromManifest(st.Manifest)
		}
	}

	if ctx.Profiles != nil {
		act := ctx.Profiles.Active()
		_ = s.store.SwitchProfile(act.ID)
		ctx.Profiles.OnSwitch(func(prev, next profile.Profile) {
			_ = s.store.SwitchProfile(next.ID)
		})
	}

	// Initialize persistent stores and index
	return s.store.Init(context.Background())
}

func (s *DocumentService) Start(ctx context.Context) error {
	s.lifetime = ctx
	if s.store != nil {
		s.store.SetLifetimeContext(ctx)
	}

	routes := map[string]ipc.Handler{
		"doc/search":                s.search,
		"doc/lookup":                s.lookup,
		"doc/recents":               s.recentsList,
		"doc/recents/add":           s.recentsAdd,
		"doc/recents/remove":        s.recentsRemove,
		"doc/recents/clear":         s.recentsClear,
		"doc/favorites":             s.favoritesList,
		"doc/favorites/add":         s.favoritesAdd,
		"doc/favorites/remove":      s.favoritesRemove,
		"doc/favorites/reorder":     s.favoritesReorder,
		"doc/favorites/clear":       s.favoritesClear,
		"doc/associations":          s.associationsList,
		"doc/associations/resolve":  s.associationsResolve,
		"doc/associations/register": s.associationsRegister,
		"doc/handoff":               s.handoff,
	}

	for method, h := range routes {
		if err := s.ctx.Router.Handle(method, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *DocumentService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("doc/")
	return nil
}

// search handles bounded permission-aware VFS search.
func (s *DocumentService) search(ctx context.Context, req ipc.Request) (any, error) {
	var q sdk.DocumentSearchQuery
	if err := ipc.DecodeParams(req.Params, &q); err != nil {
		return nil, err
	}

	principal := ipc.CallerPrincipal(ctx)
	if !principal.IsApp() {
		if err := ipc.RequireCap(ctx, security.CapFileRead); err != nil {
			return nil, err
		}
	}

	return s.store.Searcher().Search(ctx, principal, q)
}

// lookup handles indexed document queries.
func (s *DocumentService) lookup(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Query     string `json:"query"`
		Extension string `json:"extension"`
		Limit     int    `json:"limit"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	principal := ipc.CallerPrincipal(ctx)
	if !principal.IsApp() {
		if err := ipc.RequireCap(ctx, security.CapFileRead); err != nil {
			return nil, err
		}
	}

	return s.store.Searcher().Lookup(ctx, principal, p.Query, p.Extension, p.Limit)
}

// recentsList returns recents filtered by caller permissions.
func (s *DocumentService) recentsList(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		VerifyExists bool `json:"verify_exists"`
	}
	_ = ipc.DecodeParams(req.Params, &p)

	principal := ipc.CallerPrincipal(ctx)
	callerApp := ""
	if principal.IsApp() {
		callerApp = principal.AppID
	}

	list := s.store.Recents().List(callerApp, p.VerifyExists)
	return map[string]any{
		"entries": list,
		"total":   len(list),
	}, nil
}

// appCanRead reports whether appID holds a VFS grant covering envPath.
func (s *DocumentService) appCanRead(appID, envPath string) bool {
	return s.grants != nil && s.grants.CheckAccess(appID, envPath, vfs.AccessRead) == nil
}

// recentsAdd appends or updates an entry in recents.
func (s *DocumentService) recentsAdd(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	var p struct {
		Path  string `json:"path"`
		AppID string `json:"app_id"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Path) == "" {
		return nil, fmt.Errorf("doc: path is required")
	}

	principal := ipc.CallerPrincipal(ctx)
	appID := p.AppID
	if principal.IsApp() {
		appID = principal.AppID
	}

	norm, err := vfs.Normalize(p.Path)
	if err != nil {
		return nil, err
	}
	clean := "/" + norm

	if principal.IsApp() && !s.appCanRead(appID, clean) {
		// Grant check before any stat/persist/index: an app may only record
		// documents it can read. The add is ignored and answered with the
		// redacted shape #82 established, indistinguishable from a miss.
		return &sdk.RecentDocument{
			Path:       clean,
			AppID:      appID,
			AccessedAt: time.Now().UTC(),
		}, nil
	}

	entry, err := s.store.Recents().Add(p.Path, appID)
	if err != nil {
		return nil, err
	}
	_ = s.store.Searcher().IndexDocument(p.Path)
	return entry, nil
}

// recentsRemove deletes an entry from recents.
func (s *DocumentService) recentsRemove(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := s.store.Recents().Remove(p.Path); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// recentsClear removes all recents entries.
func (s *DocumentService) recentsClear(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	if err := s.store.Recents().Clear(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// favoritesList returns favorites filtered by caller permissions.
func (s *DocumentService) favoritesList(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		VerifyExists bool `json:"verify_exists"`
	}
	_ = ipc.DecodeParams(req.Params, &p)

	principal := ipc.CallerPrincipal(ctx)
	callerApp := ""
	if principal.IsApp() {
		callerApp = principal.AppID
	}

	list := s.store.Favorites().List(callerApp, p.VerifyExists)
	return map[string]any{
		"entries": list,
		"total":   len(list),
	}, nil
}

// favoritesAdd adds a document to favorites.
func (s *DocumentService) favoritesAdd(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	var p struct {
		Path  string `json:"path"`
		Label string `json:"label"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Path) == "" {
		return nil, fmt.Errorf("doc: path is required")
	}

	norm, err := vfs.Normalize(p.Path)
	if err != nil {
		return nil, err
	}
	clean := "/" + norm

	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() && !s.appCanRead(principal.AppID, clean) {
		// Same ordering rule as recentsAdd: never stat, persist, or index a
		// path the calling app cannot read.
		label := p.Label
		if label == "" {
			label = path.Base(clean)
		}
		return &sdk.FavoriteDocument{
			Path:    clean,
			Label:   label,
			AddedAt: time.Now().UTC(),
		}, nil
	}

	entry, err := s.store.Favorites().Add(p.Path, p.Label)
	if err != nil {
		return nil, err
	}
	_ = s.store.Searcher().IndexDocument(p.Path)
	return entry, nil
}

// favoritesRemove deletes a favorite.
func (s *DocumentService) favoritesRemove(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := s.store.Favorites().Remove(p.Path); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// favoritesReorder updates the order of favorites.
func (s *DocumentService) favoritesReorder(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	var p struct {
		Paths []string `json:"paths"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if err := s.store.Favorites().Reorder(p.Paths); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// favoritesClear removes all favorites.
func (s *DocumentService) favoritesClear(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapFileWrite); err != nil {
		return nil, err
	}
	if err := s.store.Favorites().Clear(); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// associationsList returns all registered associations.
func (s *DocumentService) associationsList(ctx context.Context, req ipc.Request) (any, error) {
	list := s.store.Associations().List()
	return map[string]any{
		"associations": list,
		"total":        len(list),
	}, nil
}

// associationsResolve finds handlers for a file path or extension.
func (s *DocumentService) associationsResolve(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	defApp, handlers, found := s.store.Associations().Resolve(p.Path)
	return map[string]any{
		"path":        p.Path,
		"default_app": defApp,
		"handlers":    handlers,
		"found":       found,
	}, nil
}

// associationsRegister adds a new association.
// Registration mutates the shared association table, so it requires the
// admin capability. Claiming the default handler for a type is
// additionally operator-only: a hijacked default receives a scoped grant
// for every matching document the operator opens via handoff.
func (s *DocumentService) associationsRegister(ctx context.Context, req ipc.Request) (any, error) {
	var assoc sdk.DocumentTypeAssociation
	if err := ipc.DecodeParams(req.Params, &assoc); err != nil {
		return nil, err
	}

	if err := ipc.RequireCap(ctx, security.CapAdmin); err != nil {
		return nil, err
	}
	principal := ipc.CallerPrincipal(ctx)
	if principal.IsApp() && assoc.Default {
		return nil, fmt.Errorf("doc: only an operator may claim a default association")
	}
	if principal.IsApp() && assoc.AppID != "" && assoc.AppID != principal.AppID {
		return nil, fmt.Errorf("doc: apps cannot register associations on behalf of other apps")
	}
	if principal.IsApp() && assoc.AppID == "" {
		assoc.AppID = principal.AppID
	}

	if err := s.store.Associations().Register(assoc); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// handoff executes versioned open-with document handoff.
func (s *DocumentService) handoff(ctx context.Context, req ipc.Request) (any, error) {
	var hReq sdk.HandoffRequest
	if err := ipc.DecodeParams(req.Params, &hReq); err != nil {
		return nil, err
	}

	principal := ipc.CallerPrincipal(ctx)
	return s.store.Handoff().Handoff(ctx, principal, hReq)
}
