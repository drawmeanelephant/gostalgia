package services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/session"
)

// SessionService exposes session management and workspace state over IPC.
// It owns the "session/*" method namespace.
type SessionService struct {
	ctx *service.Context
}

// NewSession creates a new SessionService.
func NewSession() *SessionService {
	return &SessionService{}
}

func (s *SessionService) Name() string { return "session" }

func (s *SessionService) Depends() []string {
	return []string{"fs"}
}

func (s *SessionService) Init(ctx *service.Context) error {
	s.ctx = ctx
	if s.ctx.Sessions != nil && s.ctx.VFS != nil {
		s.ctx.Sessions.SetVFS(s.ctx.VFS)
	}
	return nil
}

func (s *SessionService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"session/list":            {Handler: s.list, Policy: ipc.OperatorOr(security.CapSessionRead, security.CapAdmin)},
		"session/get":             {Handler: s.get, Policy: ipc.OperatorOr(security.CapSessionRead, security.CapAdmin)},
		"session/create":          {Handler: s.create, Policy: ipc.OperatorOr(security.CapSessionWrite, security.CapAdmin)},
		"session/close":           {Handler: s.close, Policy: ipc.OperatorOr(security.CapSessionWrite, security.CapAdmin)},
		"session/attach":          {Handler: s.attach, Policy: ipc.OperatorOr(security.CapSessionRead, security.CapAdmin)},
		"session/detach":          {Handler: s.detach, Policy: ipc.OperatorOr(security.CapSessionRead, security.CapAdmin)},
		"session/workspace/get":   {Handler: s.workspaceGet, Policy: ipc.OperatorOr(security.CapSessionRead, security.CapAdmin)},
		"session/workspace/set":   {Handler: s.workspaceSet, Policy: ipc.OperatorOr(security.CapSessionWrite, security.CapAdmin)},
		"session/workspace/clear": {Handler: s.workspaceClear, Policy: ipc.OperatorOr(security.CapSessionWrite, security.CapAdmin)},
	})
}

func (s *SessionService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("session/")
	return nil
}

// SessionDetail provides rich detail for a session including attachments.
type SessionDetail struct {
	ID          string               `json:"id"`
	User        security.User        `json:"user"`
	StartedAt   time.Time            `json:"started_at"`
	StoppedAt   time.Time            `json:"stopped_at,omitempty"`
	Active      bool                 `json:"active"`
	Attachments []session.Attachment `json:"attachments"`
}

func (s *SessionService) resolveSession(ctx context.Context, sessionID string) (*session.Session, error) {
	if s.ctx.Sessions == nil {
		return nil, fmt.Errorf("session: manager unavailable")
	}
	principal := ipc.CallerPrincipal(ctx)
	if principal.IsApp() {
		// Apps may only address the session they were launched into.
		if sessionID != "" && sessionID != principal.SessionID {
			return nil, fmt.Errorf("session: apps may only access their own session")
		}
		sessionID = principal.SessionID
	}
	if sessionID != "" {
		sess, ok := s.ctx.Sessions.Get(sessionID)
		if !ok {
			return nil, fmt.Errorf("session: session %q not found", sessionID)
		}
		return sess, nil
	}
	if principal.IsApp() {
		return nil, fmt.Errorf("session: no bound session for calling app")
	}
	if s.ctx.Profiles != nil {
		p := s.ctx.Profiles.Active()
		if p.ID != "" {
			userSessions := s.ctx.Sessions.ForUser(p.ID)
			if len(userSessions) > 0 {
				return userSessions[len(userSessions)-1], nil
			}
		}
	}
	active := s.ctx.Sessions.Active()
	if len(active) == 0 {
		return nil, fmt.Errorf("session: no active sessions")
	}
	return active[0], nil
}

func (s *SessionService) list(ctx context.Context, req ipc.Request) (any, error) {
	if s.ctx.Sessions == nil {
		return []SessionDetail{}, nil
	}
	principal := ipc.CallerPrincipal(ctx)
	active := s.ctx.Sessions.Active()
	out := make([]SessionDetail, 0, len(active))
	for _, sess := range active {
		if principal.IsApp() && sess.ID != principal.SessionID {
			continue
		}
		out = append(out, SessionDetail{
			ID:          sess.ID,
			User:        sess.User,
			StartedAt:   sess.StartedAt,
			StoppedAt:   sess.StoppedAt,
			Active:      sess.Active(),
			Attachments: sess.Attachments(),
		})
	}
	return out, nil
}

func (s *SessionService) get(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	sess, err := s.resolveSession(ctx, params.ID)
	if err != nil {
		return nil, err
	}
	return SessionDetail{
		ID:          sess.ID,
		User:        sess.User,
		StartedAt:   sess.StartedAt,
		StoppedAt:   sess.StoppedAt,
		Active:      sess.Active(),
		Attachments: sess.Attachments(),
	}, nil
}

func (s *SessionService) create(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		UserID   string `json:"user_id"`
		UserName string `json:"user_name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("session/create: invalid parameters: %w", err)
	}
	user := security.User{ID: params.UserID, Name: params.UserName}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() {
		// Apps cannot mint sessions for a different user identity.
		if params.UserID != "" && params.UserID != principal.User.ID {
			return nil, fmt.Errorf("session/create: apps may only create sessions for their own user")
		}
		user = principal.User
	}
	if user.ID == "" {
		return nil, fmt.Errorf("session/create: user_id is required")
	}
	if user.Name == "" {
		user.Name = user.ID
	}
	sess, err := s.ctx.Sessions.Create(user)
	if err != nil {
		return nil, err
	}
	return SessionDetail{
		ID:          sess.ID,
		User:        sess.User,
		StartedAt:   sess.StartedAt,
		Active:      sess.Active(),
		Attachments: sess.Attachments(),
	}, nil
}

func (s *SessionService) close(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.ID == "" {
		return nil, fmt.Errorf("session/close: id is required")
	}
	if principal := ipc.CallerPrincipal(ctx); principal.IsApp() && params.ID != principal.SessionID {
		return nil, fmt.Errorf("session/close: apps may only close their own session")
	}
	if err := s.ctx.Sessions.Close(params.ID); err != nil {
		return nil, err
	}
	return map[string]any{"closed": true, "id": params.ID}, nil
}

// SessionAttachResponse is returned when a client attaches to a session.
type SessionAttachResponse struct {
	SessionID    string                 `json:"session_id"`
	AttachmentID string                 `json:"attachment_id"`
	ClientID     string                 `json:"client_id"`
	ClientType   string                 `json:"client_type"`
	ActiveCount  int                    `json:"active_count"`
	Workspace    session.WorkspaceState `json:"workspace"`
	User         security.User          `json:"user"`
}

func (s *SessionService) attach(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		SessionID  string `json:"session_id"`
		ClientType string `json:"client_type"`
		ClientID   string `json:"client_id"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	sess, err := s.resolveSession(ctx, params.SessionID)
	if err != nil {
		return nil, err
	}

	att, err := s.ctx.Sessions.Attach(sess.ID, params.ClientType, params.ClientID)
	if err != nil {
		return nil, err
	}

	// The attachment belongs to the connection that made it: a client that
	// crashes or drops without session/detach must not leak its slot for
	// the life of the runtime. In-process callers have no connection, so
	// this is a no-op for them.
	ipc.OnConnClose(ctx, func() {
		_ = s.ctx.Sessions.Detach(sess.ID, att.ID)
	})

	ws := s.ctx.Sessions.Workspace(sess.ID)
	if ws == nil {
		return nil, fmt.Errorf("session: session %q not found", sess.ID)
	}
	wsState := ws.Get()

	return SessionAttachResponse{
		SessionID:    sess.ID,
		AttachmentID: att.ID,
		ClientID:     att.ClientID,
		ClientType:   att.ClientType,
		ActiveCount:  sess.AttachmentCount(),
		Workspace:    wsState,
		User:         sess.User,
	}, nil
}

func (s *SessionService) detach(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		SessionID    string `json:"session_id"`
		AttachmentID string `json:"attachment_id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil || params.AttachmentID == "" {
		return nil, fmt.Errorf("session/detach: attachment_id is required")
	}
	sess, err := s.resolveSession(ctx, params.SessionID)
	if err != nil {
		return nil, err
	}

	if err := s.ctx.Sessions.Detach(sess.ID, params.AttachmentID); err != nil {
		return nil, err
	}

	return map[string]any{
		"detached":      true,
		"session_id":    sess.ID,
		"attachment_id": params.AttachmentID,
		"active_count":  sess.AttachmentCount(),
	}, nil
}

func (s *SessionService) workspaceGet(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		SessionID string `json:"session_id"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	sess, err := s.resolveSession(ctx, params.SessionID)
	if err != nil {
		return nil, err
	}
	ws := s.ctx.Sessions.Workspace(sess.ID)
	if ws == nil {
		return nil, fmt.Errorf("session: session %q not found", sess.ID)
	}
	return ws.Get(), nil
}

func (s *SessionService) workspaceSet(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		SessionID  string `json:"session_id"`
		CWD        string `json:"cwd"`
		ActiveView string `json:"active_view"`
		Command    string `json:"cmd"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return nil, fmt.Errorf("session/workspace/set: invalid parameters: %w", err)
	}
	sess, err := s.resolveSession(ctx, params.SessionID)
	if err != nil {
		return nil, err
	}
	ws := s.ctx.Sessions.Workspace(sess.ID)
	if ws == nil {
		return nil, fmt.Errorf("session: session %q not found", sess.ID)
	}

	if params.CWD != "" {
		if err := ws.SetCurrentDir(params.CWD); err != nil {
			return nil, err
		}
	}
	if params.ActiveView != "" {
		if err := ws.SetActiveView(params.ActiveView); err != nil {
			return nil, err
		}
	}
	if params.Command != "" {
		if err := ws.AppendHistory(params.Command); err != nil {
			return nil, err
		}
	}

	return ws.Get(), nil
}

func (s *SessionService) workspaceClear(ctx context.Context, req ipc.Request) (any, error) {
	var params struct {
		SessionID    string `json:"session_id"`
		ClearHistory bool   `json:"clear_history"`
	}
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	sess, err := s.resolveSession(ctx, params.SessionID)
	if err != nil {
		return nil, err
	}
	ws := s.ctx.Sessions.Workspace(sess.ID)
	if ws == nil {
		return nil, fmt.Errorf("session: session %q not found", sess.ID)
	}
	if params.ClearHistory {
		if err := ws.ClearHistory(); err != nil {
			return nil, err
		}
	}
	return ws.Get(), nil
}
