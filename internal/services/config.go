package services

import (
	"context"
	"errors"
	"fmt"

	"gostalgia/internal/config"
	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

// ConfigService exposes configuration operations and live change notifications.
// It owns the "config/*" method namespace.
type ConfigService struct {
	ctx   *service.Context
	store *config.LayeredStore
}

func NewConfig() *ConfigService {
	return &ConfigService{}
}

func (s *ConfigService) Name() string      { return "config" }
func (s *ConfigService) Depends() []string { return nil }

func (s *ConfigService) Init(ctx *service.Context) error {
	s.ctx = ctx
	s.store = ctx.Layered
	if s.store == nil {
		// Fallback to standalone layered store if not injected
		var err error
		sysPath := ""
		if ctx.Config != nil {
			sysPath = ctx.Config.Path()
		}
		s.store, err = config.NewLayeredStore(sysPath, "")
		if err != nil {
			return err
		}
	}

	// Wire change events to the environment event bus
	s.store.OnChange(func(ev config.ChangeEvent) {
		if s.ctx.Events != nil {
			s.ctx.Events.Publish("config", ev)
		}
	})

	return nil
}

func (s *ConfigService) Start(ctx context.Context) error {
	routes := map[string]ipc.Handler{
		"config/get":            s.get,
		"config/set":            s.set,
		"config/unset":          s.unset,
		"config/reset":          s.reset,
		"config/list":           s.list,
		"config/snapshot":       s.list,
		"config/preview":        s.preview,
		"config/cancel_preview": s.cancelPreview,
		"config/commit_preview": s.commitPreview,
		"config/validate":       s.validate,
		"config/explain":        s.explain,
	}
	for method, h := range routes {
		if err := s.ctx.Router.Handle(method, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *ConfigService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("config/")
	return nil
}

type getReq struct {
	Path  string       `json:"path"`
	Layer config.Layer `json:"layer,omitempty"`
	AppID string       `json:"app_id,omitempty"`
}

type getResp struct {
	Path      string       `json:"path"`
	Value     any          `json:"value"`
	Layer     config.Layer `json:"layer"`
	Effective bool         `json:"effective"`
	Found     bool         `json:"found"`
}

func (s *ConfigService) get(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigRead); err != nil {
		return nil, err
	}
	var p getReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, errors.New("config/get: path is required")
	}

	principal := ipc.CallerPrincipal(ctx)
	if principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/get: cannot read preferences for another application")
		}
		p.AppID = principal.AppID
	}

	opts := config.QueryOpts{
		AppID: p.AppID,
		Layer: p.Layer,
	}
	val, layer, ok := s.store.Value(p.Path, opts)
	return getResp{
		Path:      p.Path,
		Value:     val,
		Layer:     layer,
		Effective: p.Layer == "",
		Found:     ok,
	}, nil
}

type setReq struct {
	Path  string       `json:"path"`
	Value any          `json:"value"`
	Layer config.Layer `json:"layer,omitempty"`
	AppID string       `json:"app_id,omitempty"`
}

type setResp struct {
	OK    bool         `json:"ok"`
	Path  string       `json:"path"`
	Value any          `json:"value"`
	Layer config.Layer `json:"layer"`
}

func (s *ConfigService) set(ctx context.Context, req ipc.Request) (any, error) {
	var p setReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, errors.New("config/set: path is required")
	}

	targetLayer := p.Layer
	if targetLayer == "" {
		targetLayer = config.LayerUser
	}

	principal := ipc.CallerPrincipal(ctx)

	// Permission checks
	if targetLayer == config.LayerSystem {
		if err := ipc.RequireCap(ctx, security.CapAdmin); err != nil {
			return nil, fmt.Errorf("config/set: system layer requires admin capability: %w", err)
		}
	} else if targetLayer == config.LayerUser {
		if principal.IsApp() {
			if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
				return nil, fmt.Errorf("config/set: user layer requires config.write capability: %w", err)
			}
		} else if err := ipc.RequireCap(ctx, security.CapIPC); err != nil {
			return nil, err
		}
	} else if targetLayer == config.LayerApp {
		if principal.IsApp() && p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/set: cannot modify preferences for another application")
		}
		if p.AppID == "" && principal.IsApp() {
			p.AppID = principal.AppID
		}
	}

	opts := config.QueryOpts{
		AppID: p.AppID,
		Layer: targetLayer,
	}
	if err := s.store.Set(targetLayer, p.Path, p.Value, opts); err != nil {
		return nil, err
	}

	return setResp{
		OK:    true,
		Path:  p.Path,
		Value: p.Value,
		Layer: targetLayer,
	}, nil
}

type unsetReq struct {
	Path  string       `json:"path"`
	Layer config.Layer `json:"layer,omitempty"`
	AppID string       `json:"app_id,omitempty"`
}

func (s *ConfigService) unset(ctx context.Context, req ipc.Request) (any, error) {
	var p unsetReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, errors.New("config/unset: path is required")
	}

	targetLayer := p.Layer
	if targetLayer == "" {
		targetLayer = config.LayerUser
	}

	principal := ipc.CallerPrincipal(ctx)

	if targetLayer == config.LayerSystem {
		if err := ipc.RequireCap(ctx, security.CapAdmin); err != nil {
			return nil, err
		}
	} else if principal.IsApp() {
		if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
			return nil, err
		}
	}

	// App principals may only address their own app layer, matching
	// config/set and the read routes.
	if principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/unset: cannot modify preferences for another application")
		}
		p.AppID = principal.AppID
	}

	opts := config.QueryOpts{
		AppID: p.AppID,
		Layer: targetLayer,
	}
	if err := s.store.Unset(targetLayer, p.Path, opts); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "path": p.Path, "layer": targetLayer}, nil
}

type resetReq struct {
	Layer config.Layer `json:"layer,omitempty"`
	Path  string       `json:"path,omitempty"`
	AppID string       `json:"app_id,omitempty"`
}

func (s *ConfigService) reset(ctx context.Context, req ipc.Request) (any, error) {
	var p resetReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	targetLayer := p.Layer
	if targetLayer == "" {
		targetLayer = config.LayerUser
	}

	principal := ipc.CallerPrincipal(ctx)

	if targetLayer == config.LayerSystem {
		if err := ipc.RequireCap(ctx, security.CapAdmin); err != nil {
			return nil, err
		}
	} else if principal.IsApp() {
		if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
			return nil, err
		}
	}

	// App principals may only address their own app layer, matching
	// config/set and the read routes.
	if principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/reset: cannot modify preferences for another application")
		}
		p.AppID = principal.AppID
	}

	opts := config.QueryOpts{
		AppID: p.AppID,
		Layer: targetLayer,
	}
	if err := s.store.Reset(targetLayer, p.Path, opts); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "layer": targetLayer, "path": p.Path}, nil
}

type listReq struct {
	AppID string `json:"app_id,omitempty"`
}

type listResp struct {
	Effective map[string]any          `json:"effective"`
	Layers    map[config.Layer]any    `json:"layers"`
	Corrupt   map[config.Layer]string `json:"corrupt,omitempty"`
}

func (s *ConfigService) list(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigRead); err != nil {
		return nil, err
	}
	var p listReq
	_ = ipc.DecodeParams(req.Params, &p)

	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/list: cannot read preferences for another application")
		}
		p.AppID = principal.AppID
	}

	opts := config.QueryOpts{AppID: p.AppID}
	effective := s.store.Snapshot(opts)
	layers := map[config.Layer]any{
		config.LayerDefault: s.store.LayerSnapshot(config.LayerDefault, opts),
		config.LayerSystem:  s.store.LayerSnapshot(config.LayerSystem, opts),
		config.LayerUser:    s.store.LayerSnapshot(config.LayerUser, opts),
		config.LayerApp:     s.store.LayerSnapshot(config.LayerApp, opts),
		config.LayerPreview: s.store.LayerSnapshot(config.LayerPreview, opts),
	}

	return listResp{
		Effective: effective,
		Layers:    layers,
		Corrupt:   s.store.CorruptLayers(),
	}, nil
}

type previewReq struct {
	Path     string         `json:"path,omitempty"`
	Value    any            `json:"value,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

func (s *ConfigService) preview(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
		return nil, err
	}
	var p previewReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	// Preview overrides are owned by the staging caller: applications stage
	// under their own app ID, operators and in-process callers under the
	// shared scope. Only the owner can commit or cancel them.
	opts := config.QueryOpts{}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		opts.AppID = principal.AppID
	}
	if p.Settings != nil {
		if err := s.store.PreviewBatch(p.Settings, opts); err != nil {
			return nil, err
		}
	} else if p.Path != "" {
		if err := s.store.Preview(p.Path, p.Value, opts); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New("config/preview: path or settings required")
	}

	return map[string]any{
		"ok":      true,
		"preview": s.store.LayerSnapshot(config.LayerPreview, opts),
	}, nil
}

func (s *ConfigService) cancelPreview(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
		return nil, err
	}
	opts := config.QueryOpts{}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		opts.AppID = principal.AppID
	}
	s.store.CancelPreview(opts)
	return map[string]any{"ok": true}, nil
}

type commitPreviewReq struct {
	Layer config.Layer `json:"layer,omitempty"`
}

func (s *ConfigService) commitPreview(ctx context.Context, req ipc.Request) (any, error) {
	var p commitPreviewReq
	_ = ipc.DecodeParams(req.Params, &p)

	targetLayer := p.Layer
	if targetLayer == "" {
		targetLayer = config.LayerUser
	}

	principal := ipc.CallerPrincipal(ctx)

	if targetLayer == config.LayerSystem {
		if err := ipc.RequireCap(ctx, security.CapAdmin); err != nil {
			return nil, err
		}
	} else if err := ipc.RequireCap(ctx, security.CapConfigWrite); err != nil {
		return nil, err
	}

	// Only the caller's own staged overrides are committed.
	opts := config.QueryOpts{}
	if principal.IsApp() {
		opts.AppID = principal.AppID
	}
	if err := s.store.CommitPreview(targetLayer, opts); err != nil {
		return nil, err
	}

	return map[string]any{"ok": true, "layer": targetLayer}, nil
}

type validateReq struct {
	Path     string         `json:"path,omitempty"`
	Value    any            `json:"value,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

type validateResp struct {
	Valid     bool                        `json:"valid"`
	Conflicts []config.KeybindingConflict `json:"conflicts,omitempty"`
	Error     string                      `json:"error,omitempty"`
}

func (s *ConfigService) validate(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigRead); err != nil {
		return nil, err
	}
	var p validateReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	if p.Settings != nil {
		if err := config.ValidateBatch(p.Settings); err != nil {
			var conflictErr *config.KeybindingConflictError
			if errors.As(err, &conflictErr) {
				return validateResp{
					Valid:     false,
					Conflicts: conflictErr.Conflicts,
					Error:     conflictErr.Error(),
				}, nil
			}
			return validateResp{
				Valid: false,
				Error: err.Error(),
			}, nil
		}
		return validateResp{Valid: true}, nil
	}

	if p.Path != "" {
		if err := config.Validate(p.Path, p.Value); err != nil {
			return validateResp{
				Valid: false,
				Error: err.Error(),
			}, nil
		}
		return validateResp{Valid: true}, nil
	}

	return nil, errors.New("config/validate: path or settings required")
}

type explainReq struct {
	Path  string `json:"path"`
	AppID string `json:"app_id,omitempty"`
}

func (s *ConfigService) explain(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapConfigRead); err != nil {
		return nil, err
	}
	var p explainReq
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Path == "" {
		return nil, errors.New("config/explain: path is required")
	}

	principal := ipc.CallerPrincipal(ctx)
	if principal.IsApp() {
		if p.AppID != "" && p.AppID != principal.AppID {
			return nil, errors.New("config/explain: cannot read preferences for another application")
		}
		p.AppID = principal.AppID
	}

	exp := s.store.Explain(p.Path, config.QueryOpts{AppID: p.AppID})
	return exp, nil
}
