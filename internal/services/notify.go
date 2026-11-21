package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

const (
	maxNotifyTitle = 140
	maxNotifyBody  = 1024
)

// NotifyService exposes application-facing user notifications.
// It owns the "notify/*" method namespace.
type NotifyService struct {
	ctx *service.Context
}

// NewNotify creates a new NotifyService.
func NewNotify() *NotifyService {
	return &NotifyService{}
}

func (s *NotifyService) Name() string      { return "notify" }
func (s *NotifyService) Depends() []string { return nil }

func (s *NotifyService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *NotifyService) Start(ctx context.Context) error {
	return s.ctx.Router.HandlePolicy("notify/post", s.post,
		ipc.AppOnly(security.CapNotify))
}

func (s *NotifyService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("notify/")
	return nil
}

type postParams struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Body     string `json:"body"`
}

func (s *NotifyService) post(ctx context.Context, req ipc.Request) (any, error) {
	// The route policy admits only application principals holding the
	// notify capability; this repeats the check so the handler cannot be
	// invoked off-router with weaker guarantees.
	principal := ipc.CallerPrincipal(ctx)
	if !principal.IsApp() || principal.AppID == "" {
		return nil, fmt.Errorf("notify/post: only applications can post notifications")
	}
	if err := ipc.RequireCap(ctx, security.CapNotify); err != nil {
		return nil, err
	}

	var p postParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return nil, fmt.Errorf("notify/post: invalid parameters: %w", err)
	}
	p.Severity = strings.TrimSpace(p.Severity)
	p.Title = strings.TrimSpace(p.Title)
	p.Body = strings.TrimSpace(p.Body)

	if p.Title == "" {
		return nil, fmt.Errorf("notify/post: title is required")
	}
	if p.Body == "" {
		return nil, fmt.Errorf("notify/post: body is required")
	}
	if len(p.Title) > maxNotifyTitle {
		return nil, fmt.Errorf("notify/post: title exceeds %d characters", maxNotifyTitle)
	}
	if len(p.Body) > maxNotifyBody {
		return nil, fmt.Errorf("notify/post: body exceeds %d characters", maxNotifyBody)
	}

	level := strings.ToLower(p.Severity)
	switch level {
	case "info", "warning", "error":
	default:
		return nil, fmt.Errorf("notify/post: severity must be info, warning, or error")
	}

	if s.ctx.Notifications == nil {
		return nil, fmt.Errorf("notify/post: notification pipeline unavailable")
	}

	s.ctx.Notifications.Record(service.NotificationRecord{
		Title:     p.Title,
		Message:   p.Body,
		Level:     level,
		Source:    principal.AppID,
		PID:       principal.ProcessID,
		Timestamp: time.Now(),
	})

	return map[string]any{"posted": true}, nil
}
