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

	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		// Search and lookup are permission-aware inside: applications
		// are scoped by their grants; everyone else needs fs.read.
		"doc/search":                {Handler: s.search, Policy: ipc.AppScoped(security.CapFileRead)},
		"doc/lookup":                {Handler: s.lookup, Policy: ipc.AppScoped(security.CapFileRead)},
		"doc/recents":               {Handler: s.recentsList, Policy: ipc.Baseline},
		"doc/recents/add":           {Handler: s.recentsAdd, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/recents/remove":        {Handler: s.recentsRemove, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/recents/clear":         {Handler: s.recentsClear, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/favorites":             {Handler: s.favoritesList, Policy: ipc.Baseline},
		"doc/favorites/add":         {Handler: s.favoritesAdd, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/favorites/remove":      {Handler: s.favoritesRemove, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/favorites/reorder":     {Handler: s.favoritesReorder, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/favorites/clear":       {Handler: s.favoritesClear, Policy: ipc.Cap(security.CapFileWrite)},
		"doc/associations":          {Handler: s.associationsList, Policy: ipc.Baseline},
		"doc/associations/resolve":  {Handler: s.associationsResolve, Policy: ipc.Baseline},
		"doc/associations/register": {Handler: s.associationsRegister, Policy: ipc.Cap(security.CapAdmin)},
		// Handoff's caller checks live in the store: app callers need a
		// grant covering the document, operators hand off anything.
		"doc/handoff": {Handler: s.handoff, Policy: ipc.Baseline},
	})
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
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		// Scoped removal: an app may only drop entries it can see in a
		// filtered recents list — granted paths, or entries tagged with
		// its own id. An unmatched entry is a silent no-op so the
		// response stays indistinguishable from an absent one.
		if err := s.store.Recents().RemoveFor(p.Path, principal.AppID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if err := s.store.Recents().Remove(p.Path); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// recentsClear removes all recents entries for an operator, or only the
// caller-visible entries for an app.
func (s *DocumentService) recentsClear(ctx context.Context, req ipc.Request) (any, error) {
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		if err := s.store.Recents().ClearFor(principal.AppID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
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
	var p struct {
		Path string `json:"path"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		// Same scoped-removal rule as recents: only entries the app can
		// see in a filtered favorites list may be dropped.
		if err := s.store.Favorites().RemoveFor(p.Path, principal.AppID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if err := s.store.Favorites().Remove(p.Path); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// favoritesReorder updates the order of favorites. App callers reorder only
// the caller-visible subset; entries outside their grants keep their ranks.
func (s *DocumentService) favoritesReorder(ctx context.Context, req ipc.Request) (any, error) {
	var p struct {
		Paths []string `json:"paths"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		if err := s.store.Favorites().ReorderFor(p.Paths, principal.AppID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	if err := s.store.Favorites().Reorder(p.Paths); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

// favoritesClear removes all favorites for an operator, or only the
// caller-visible entries for an app.
func (s *DocumentService) favoritesClear(ctx context.Context, req ipc.Request) (any, error) {
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		if err := s.store.Favorites().ClearFor(principal.AppID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
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
