package services

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
)

// ProcService exposes the process manager over IPC. It owns "proc/*".
type ProcService struct {
	ctx *service.Context
}

func NewProc() *ProcService { return &ProcService{} }

func (s *ProcService) Name() string      { return "process" }
func (s *ProcService) Depends() []string { return nil }

func (s *ProcService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *ProcService) Start(ctx context.Context) error {
	if err := s.ctx.Router.Handle("proc/list", s.list); err != nil {
		return err
	}
	if err := s.ctx.Router.Handle("proc/info", s.info); err != nil {
		return err
	}
	if err := s.ctx.Router.Handle("proc/logs", s.logs); err != nil {
		return err
	}
	if err := s.ctx.Router.Handle("proc/history", s.history); err != nil {
		return err
	}
	if err := s.ctx.Router.Handle("proc/reap", s.reap); err != nil {
		return err
	}
	return s.ctx.Router.Handle("proc/stop", s.stop)
}

func (s *ProcService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("proc/")
	return nil
}

func (s *ProcService) list(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcList); err != nil {
		return nil, err
	}
	return s.ctx.Procs.List(), nil
}

func (s *ProcService) history(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcList); err != nil {
		return nil, err
	}
	return s.ctx.Procs.History(), nil
}

func (s *ProcService) reap(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcStop); err != nil {
		return nil, err
	}
	reaped := s.ctx.Procs.Reap()
	return map[string]any{"reaped": reaped}, nil
}

func (s *ProcService) info(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcList); err != nil {
		return nil, err
	}
	var p struct {
		ID int32 `json:"id"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == 0 {
		return nil, fmt.Errorf("params.id is required")
	}
	proc, ok := s.ctx.Procs.Get(p.ID)
	if ok {
		return proc.Info(), nil
	}
	entry, ok := s.ctx.Procs.HistoryByID(p.ID)
	if ok {
		return entry, nil
	}
	return nil, fmt.Errorf("process: no such process %d", p.ID)
}

func (s *ProcService) logs(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcList); err != nil {
		return nil, err
	}
	var p struct {
		ID     int32  `json:"id"`
		Stream string `json:"stream,omitempty"` // "stdout", "stderr", "combined", or ""
		Tail   int    `json:"tail,omitempty"`   // number of trailing lines
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == 0 {
		return nil, fmt.Errorf("params.id is required")
	}
	logs, ok := s.ctx.Procs.Logs(p.ID)
	if !ok {
		return nil, fmt.Errorf("process: no such process %d", p.ID)
	}

	if p.Tail > 0 {
		logs.Stdout.Content = tailLines(logs.Stdout.Content, p.Tail)
		logs.Stderr.Content = tailLines(logs.Stderr.Content, p.Tail)
		logs.Combined.Content = tailLines(logs.Combined.Content, p.Tail)
	}

	switch strings.ToLower(p.Stream) {
	case "stdout":
		return map[string]any{
			"id":         logs.ID,
			"name":       logs.Name,
			"kind":       logs.Kind,
			"state":      logs.State,
			"exit_code":  logs.ExitCode,
			"started_at": logs.StartedAt,
			"exited_at":  logs.ExitedAt,
			"duration":   logs.Duration,
			"stdout":     logs.Stdout,
		}, nil
	case "stderr":
		return map[string]any{
			"id":         logs.ID,
			"name":       logs.Name,
			"kind":       logs.Kind,
			"state":      logs.State,
			"exit_code":  logs.ExitCode,
			"started_at": logs.StartedAt,
			"exited_at":  logs.ExitedAt,
			"duration":   logs.Duration,
			"stderr":     logs.Stderr,
		}, nil
	default:
		return logs, nil
	}
}

func tailLines(s string, n int) string {
	if n <= 0 || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n") + "\n"
}

// maxProcStopTimeout caps proc/stop's caller-supplied timeout_seconds so a
// caller cannot park a handler goroutine indefinitely on a process that
// never exits. A var so tests can shrink it.
var maxProcStopTimeout = 60 * time.Second

func (s *ProcService) stop(ctx context.Context, req ipc.Request) (any, error) {
	if err := ipc.RequireCap(ctx, security.CapProcStop); err != nil {
		return nil, err
	}
	var p struct {
		ID      int32 `json:"id"`
		Timeout int   `json:"timeout_seconds,omitempty"`
	}
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.ID == 0 {
		return nil, fmt.Errorf("params.id is required")
	}
	timeout := 5 * time.Second
	if p.Timeout > 0 {
		timeout = min(time.Duration(p.Timeout)*time.Second, maxProcStopTimeout)
	}
	if err := s.ctx.Procs.Stop(p.ID, timeout); err != nil {
		return nil, err
	}
	return map[string]any{"id": p.ID, "stopped": true}, nil
}
