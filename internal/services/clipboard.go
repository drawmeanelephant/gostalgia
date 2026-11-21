package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/platform"
)

// ClipboardService manages the environment clipboard, coordinating between
// internal in-memory clipboard storage and optional host system clipboard integration.
// It owns the "clipboard/*" IPC method namespace.
type ClipboardService struct {
	ctx          *service.Context
	mu           sync.RWMutex
	internalText string
	updatedAt    time.Time
}

func NewClipboard() *ClipboardService {
	return &ClipboardService{}
}

func (s *ClipboardService) Name() string      { return "clipboard" }
func (s *ClipboardService) Depends() []string { return nil }

func (s *ClipboardService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *ClipboardService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"clipboard/read":   {Handler: s.read, Policy: ipc.Cap(security.CapClipboardRead)},
		"clipboard/write":  {Handler: s.write, Policy: ipc.Cap(security.CapClipboardWrite)},
		"clipboard/status": {Handler: s.status, Policy: ipc.Baseline},
		"clipboard/clear":  {Handler: s.clear, Policy: ipc.Cap(security.CapClipboardWrite)},
	})
}

func (s *ClipboardService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("clipboard/")
	return nil
}

type clipboardReadParams struct {
	Target string `json:"target,omitempty"` // "auto", "host", "internal"
}

func (s *ClipboardService) read(ctx context.Context, req ipc.Request) (any, error) {
	var p clipboardReadParams
	_ = ipc.DecodeParams(req.Params, &p)
	if p.Target == "" {
		p.Target = "auto"
	}

	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	switch p.Target {
	case "host":
		if err := policy.CheckClipboard(false); err != nil {
			return nil, err
		}
		if !platform.HostClipboardAvailable() {
			return nil, platform.ErrClipboardUnsupported
		}
		text, err := platform.ReadHostClipboard(ctx)
		if err != nil {
			return nil, err
		}
		cleanText := platform.SanitizeClipboard(text)
		s.mu.Lock()
		s.internalText = cleanText
		s.updatedAt = time.Now()
		s.mu.Unlock()
		return map[string]any{
			"text":   cleanText,
			"source": "host",
		}, nil

	case "internal":
		s.mu.RLock()
		txt := s.internalText
		s.mu.RUnlock()
		return map[string]any{
			"text":   txt,
			"source": "internal",
		}, nil

	case "auto":
		if policy.CheckClipboard(false) == nil && platform.HostClipboardAvailable() {
			if text, err := platform.ReadHostClipboard(ctx); err == nil {
				cleanText := platform.SanitizeClipboard(text)
				s.mu.Lock()
				s.internalText = cleanText
				s.updatedAt = time.Now()
				s.mu.Unlock()
				return map[string]any{
					"text":   cleanText,
					"source": "host",
				}, nil
			}
		}

		s.mu.RLock()
		txt := s.internalText
		s.mu.RUnlock()
		return map[string]any{
			"text":   txt,
			"source": "internal",
		}, nil

	default:
		return nil, fmt.Errorf("clipboard: unknown target %q (valid: auto, host, internal)", p.Target)
	}
}

type clipboardWriteParams struct {
	Text   string `json:"text"`
	Target string `json:"target,omitempty"` // "auto", "host", "internal"
}

func (s *ClipboardService) write(ctx context.Context, req ipc.Request) (any, error) {
	var p clipboardWriteParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Target == "" {
		p.Target = "auto"
	}

	// Always sanitize input: strip OSC and escape sequences from untrusted inputs
	cleanText := platform.SanitizeClipboard(p.Text)

	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	// Validate the target and complete any external writes before mutating
	// internal state: a failed write must leave the clipboard unchanged.
	commit := func() {
		s.mu.Lock()
		s.internalText = cleanText
		s.updatedAt = time.Now()
		s.mu.Unlock()
	}

	switch p.Target {
	case "host":
		if err := policy.CheckClipboard(true); err != nil {
			return nil, err
		}
		if !platform.HostClipboardAvailable() {
			return nil, platform.ErrClipboardUnsupported
		}
		if err := platform.WriteHostClipboard(ctx, cleanText); err != nil {
			return nil, err
		}
		commit()
		return map[string]any{
			"written": true,
			"bytes":   len(cleanText),
			"source":  "host",
		}, nil

	case "internal":
		commit()
		return map[string]any{
			"written": true,
			"bytes":   len(cleanText),
			"source":  "internal",
		}, nil

	case "auto":
		if policy.CheckClipboard(true) == nil && platform.HostClipboardAvailable() {
			if err := platform.WriteHostClipboard(ctx, cleanText); err == nil {
				commit()
				return map[string]any{
					"written": true,
					"bytes":   len(cleanText),
					"source":  "host",
				}, nil
			}
		}
		commit()
		return map[string]any{
			"written": true,
			"bytes":   len(cleanText),
			"source":  "internal",
		}, nil

	default:
		return nil, fmt.Errorf("clipboard: unknown target %q (valid: auto, host, internal)", p.Target)
	}
}

func (s *ClipboardService) status(ctx context.Context, req ipc.Request) (any, error) {
	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	s.mu.RLock()
	hasContent := len(s.internalText) > 0
	contentBytes := len(s.internalText)
	s.mu.RUnlock()

	return map[string]any{
		"host_available":   platform.HostClipboardAvailable(),
		"policy_enabled":   policy.Clipboard.Enabled,
		"allow_host_read":  policy.Clipboard.AllowHostRead,
		"allow_host_write": policy.Clipboard.AllowHostWrite,
		"has_content":      hasContent,
		"content_bytes":    contentBytes,
	}, nil
}

func (s *ClipboardService) clear(ctx context.Context, req ipc.Request) (any, error) {
	s.mu.Lock()
	s.internalText = ""
	s.updatedAt = time.Now()
	s.mu.Unlock()

	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	if policy.CheckClipboard(true) == nil && platform.HostClipboardAvailable() {
		_ = platform.WriteHostClipboard(ctx, "")
	}

	return map[string]any{"cleared": true}, nil
}
