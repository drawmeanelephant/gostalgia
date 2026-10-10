package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gostalgia/internal/app"
	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/session"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
)

// SysService exposes runtime status, control, and application endpoints.
// It owns the "sys/*", "app/*", and "session/*" method namespaces.
type SysService struct {
	ctx      *service.Context
	lifetime context.Context
}

func NewSys() *SysService { return &SysService{} }

func (s *SysService) Name() string      { return "sys" }
func (s *SysService) Depends() []string { return nil }

func (s *SysService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *SysService) Start(ctx context.Context) error {
	s.lifetime = ctx
	routes := map[string]ipc.Handler{
		"sys/ping":          s.ping,
		"sys/status":        s.status,
		"sys/shutdown":      s.shutdown,
		"sys/policy":        s.policyGet,
		"sys/policy/update": s.policyUpdate,
		"app/list":          s.appList,
		"app/launch":        s.appLaunch,
		"app/stop":          s.appStop,
		"app/invalidate":    s.appInvalidate,
		"session/whoami":    s.whoami,
	}
	for method, h := range routes {
		if err := s.ctx.Router.Handle(method, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *SysService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("sys/")
	s.ctx.Router.UnhandlePrefix("app/")
	s.ctx.Router.UnhandlePrefix("session/")
	return nil
}

func (s *SysService) ping(ctx context.Context, req ipc.Request) (any, error) {
	return map[string]any{"pong": true, "version": s.ctx.Version}, nil
}

type statusData struct {
	Version       string                            `json:"version"`
	Root          string                            `json:"root"`
	UptimeSeconds float64                           `json:"uptime_seconds"`
	Endpoint      string                            `json:"ipc_endpoint,omitempty"`
	User          string                            `json:"user"`
	Session       string                            `json:"session,omitempty"`
	Services      []service.Status                  `json:"services"`
	Processes     []processInfo                     `json:"processes"`
	Apps          []app.Status                      `json:"apps"`
	Sessions      []sessionInfo                     `json:"sessions"`
	Security      platform.HostSecurityCapabilities `json:"security"`
	Policy        security.OperatorPolicy           `json:"policy"`
}

type processInfo struct {
	ID        int32  `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	Isolation string `json:"isolation,omitempty"`
}

type sessionInfo struct {
	ID   string `json:"id"`
	User string `json:"user"`
}

func (s *SysService) status(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapIPC); err != nil {
		return nil, err
	}
	procs := make([]processInfo, 0)
	for _, p := range s.ctx.Procs.List() {
		procs = append(procs, processInfo{
			ID:        p.ID,
			Name:      p.Name,
			Kind:      string(p.Kind),
			State:     string(p.State),
			Isolation: p.Isolation,
		})
	}
	sessions := make([]sessionInfo, 0)
	for _, sess := range s.ctx.Sessions.Active() {
		sessions = append(sessions, sessionInfo{ID: sess.ID, User: sess.User.Name})
	}
	data := statusData{
		Version:       s.ctx.Version,
		Root:          s.ctx.Root,
		UptimeSeconds: time.Since(s.ctx.BootedAt).Seconds(),
		Endpoint:      s.ctx.Endpoint,
		User:          s.defaultUser().Name,
		Services:      s.ctx.Services.Snapshot(),
		Processes:     procs,
		Apps:          s.ctx.Apps.List(),
		Sessions:      sessions,
		Security:      platform.GetHostSecurityCapabilities(),
		Policy:        security.DefaultOperatorPolicy(),
	}
	if s.ctx.Policy != nil {
		data.Policy = s.ctx.Policy.Get()
	}
	if sess := s.defaultSession(); sess != nil {
		data.Session = sess.ID
	}
	return data, nil
}

func (s *SysService) defaultUser() security.User {
	if s.ctx.Profiles != nil {
		p := s.ctx.Profiles.Active()
		if p.ID != "" {
			return security.User{ID: p.ID, Name: p.ID}
		}
	}
	if sess := s.defaultSession(); sess != nil {
		return sess.User
	}
	return security.User{Name: "guest"}
}

func (s *SysService) defaultSession() *session.Session {
	if s.ctx.Sessions == nil {
		return nil
	}
	if s.ctx.Profiles != nil {
		p := s.ctx.Profiles.Active()
		if p.ID != "" {
			userSessions := s.ctx.Sessions.ForUser(p.ID)
			if len(userSessions) > 0 {
				return userSessions[len(userSessions)-1]
			}
		}
	}
	active := s.ctx.Sessions.Active()
	if len(active) == 0 {
		return nil
	}
	return active[0]
}

func (s *SysService) shutdown(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapShutdown); err != nil {
		return nil, err
	}
	var p struct {
		Reason string `json:"reason"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Reason == "" {
		p.Reason = "requested via IPC"
	}
	if s.ctx.Shutdown == nil {
		return nil, errors.New("shutdown is unavailable")
	}
	// Run in a goroutine: shutdown closes the very connection this
	// response is being written on.
	go s.ctx.Shutdown(p.Reason)
	return map[string]any{"stopping": true, "reason": p.Reason}, nil
}

func (s *SysService) appList(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapAppList); err != nil {
		return nil, err
	}
	return s.ctx.Apps.List(), nil
}

func (s *SysService) appLaunch(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapAppLaunch); err != nil {
		return nil, err
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	proc, err := s.ctx.Apps.Launch(s.lifetime, p.ID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "pid": proc.ID()}, nil
}

func (s *SysService) appStop(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcStop); err != nil {
		return nil, err
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == "" {
		return nil, fmt.Errorf("params.id is required")
	}
	if err := s.ctx.Apps.Stop(p.ID, 6*time.Second); err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "stopped": true}, nil
}

// appInvalidate is the presentation contract's version-3 push channel: the
// calling app reports its own snapshot stale and subscribers refetch it.
// The app id comes from the caller principal — an app cannot invalidate a
// view it does not own, and no view data rides the event.
func (s *SysService) appInvalidate(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapIPC); err != nil {
		return nil, err
	}
	principal := ipc.CallerPrincipal(ctx)
	if !principal.IsApp() || principal.AppID == "" {
		return nil, fmt.Errorf("app/invalidate: only applications can invalidate their view")
	}
	if s.ctx.Events == nil {
		return nil, fmt.Errorf("app/invalidate: event bus unavailable")
	}
	s.ctx.Events.Publish("app/"+principal.AppID, app.ViewEvent{
		ID:  principal.AppID,
		PID: principal.ProcessID,
	})
	return map[string]any{"published": true}, nil
}

func (s *SysService) whoami(ctx context.Context, req ipc.Request) (any, error) {
	caps := ipc.Capabilities(ctx)
	names := []string{}
	if caps != nil {
		names = caps.List()
	}
	data := map[string]any{"capabilities": names}
	p := ipc.CallerPrincipal(ctx)
	if p.Kind != "" {
		data["principal"] = string(p.Kind)
		if p.IsApp() {
			data["app_id"] = p.AppID
			data["process_id"] = p.ProcessID
			if p.SessionID != "" {
				data["session"] = p.SessionID
			}
			if p.User.Name != "" {
				data["user"] = p.User.Name
				data["user_id"] = p.User.ID
			}
		} else if p.IsOperator() {
			if p.User.Name != "" {
				data["user"] = p.User.Name
				data["user_id"] = p.User.ID
			}
		}
	} else if sess := s.defaultSession(); sess != nil {
		data["user"] = sess.User.Name
		data["user_id"] = sess.User.ID
		data["session"] = sess.ID
	}
	return data, nil
}

func (s *SysService) policyGet(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapIPC); err != nil {
		return nil, err
	}
	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}
	return policy, nil
}

func (s *SysService) policyUpdate(ctx context.Context, req ipc.Request) (any, error) {
	principal := ipc.CallerPrincipal(ctx)
	caps := ipc.Capabilities(ctx)
	if !principal.IsOperator() && (caps == nil || !caps.Has(security.CapAdmin)) {
		return nil, &vfs.Error{Op: "policy/update", Code: vfs.ErrPermission, Message: "permission denied: operator required"}
	}
	var newPolicy security.OperatorPolicy
	if err := ipc.DecodeParams(req.Params, &newPolicy); err != nil {
		return nil, err
	}
	if s.ctx != nil && s.ctx.Policy != nil {
		s.ctx.Policy.Set(newPolicy)
	}
	return map[string]any{"updated": true, "policy": newPolicy}, nil
}
