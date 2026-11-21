package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gostalgia/internal/ipc"
	"gostalgia/internal/security"
	"gostalgia/internal/service"
	"gostalgia/internal/vfs"
	"gostalgia/platform"
)

// NetService manages outbound network egress for applications and services,
// enforcing strict capability checks and operator policy gates.
// It owns the "net/*" IPC method namespace.
type NetService struct {
	ctx *service.Context
}

func NewNet() *NetService {
	return &NetService{}
}

func (s *NetService) Name() string      { return "net" }
func (s *NetService) Depends() []string { return nil }

func (s *NetService) Init(ctx *service.Context) error {
	s.ctx = ctx
	return nil
}

func (s *NetService) Start(ctx context.Context) error {
	return s.ctx.Router.HandleRoutes(map[string]ipc.Route{
		"net/fetch":  {Handler: s.fetch, Policy: ipc.Cap(security.CapNetEgress)},
		"net/status": {Handler: s.status, Policy: ipc.Baseline},
	})
}

func (s *NetService) Stop(ctx context.Context) error {
	s.ctx.Router.UnhandlePrefix("net/")
	return nil
}

type fetchParams struct {
	URL            string          `json:"url"`
	Method         string          `json:"method,omitempty"`
	Headers        json.RawMessage `json:"headers,omitempty"` // map[string]string or map[string][]string
	BodyBase64     string          `json:"body_base64,omitempty"`
	TimeoutSeconds int             `json:"timeout_seconds,omitempty"`
}

func (s *NetService) fetch(ctx context.Context, req ipc.Request) (any, error) {
	var p fetchParams
	if err := ipc.DecodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.URL) == "" {
		return nil, fmt.Errorf("params.url is required")
	}

	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method == "" {
		method = "GET"
	}

	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	if err := policy.CheckNetwork(p.URL); err != nil {
		return nil, err
	}

	timeout := 10 * time.Second
	if p.TimeoutSeconds > 0 {
		if p.TimeoutSeconds > 60 {
			timeout = 60 * time.Second
		} else {
			timeout = time.Duration(p.TimeoutSeconds) * time.Second
		}
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var bodyReader io.Reader
	if p.BodyBase64 != "" {
		bodyBytes, err := base64.StdEncoding.DecodeString(p.BodyBase64)
		if err != nil {
			return nil, fmt.Errorf("params.body_base64: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	httpReq, err := http.NewRequestWithContext(reqCtx, method, p.URL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("net: create request: %w", err)
	}
	// Re-validate every redirect hop: the check above covers only the
	// initial URL, so a 302 could otherwise bounce egress to a blocked or
	// unlisted host or downgrade HTTPS to plaintext.
	httpReq = httpReq.WithContext(platform.ContextWithRedirectCheck(httpReq.Context(), func(u *url.URL) error {
		return policy.CheckNetwork(u.String())
	}))

	// Parse headers flexibly: either map[string]string or map[string][]string
	if len(p.Headers) > 0 {
		var singleHeaders map[string]string
		if err := json.Unmarshal(p.Headers, &singleHeaders); err == nil {
			for k, v := range singleHeaders {
				httpReq.Header.Set(k, v)
			}
		} else {
			var multiHeaders map[string][]string
			if err := json.Unmarshal(p.Headers, &multiHeaders); err == nil {
				for k, vals := range multiHeaders {
					for _, v := range vals {
						httpReq.Header.Add(k, v)
					}
				}
			}
		}
	}

	adapter := platform.GetNetworkAdapter()
	resp, err := adapter.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("net: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Bound response body read size
	limitReader := io.LimitReader(resp.Body, int64(vfs.MaxIPCReadLimit)+1)
	respBytes, err := io.ReadAll(limitReader)
	if err != nil {
		return nil, fmt.Errorf("net: read response: %w", err)
	}
	if len(respBytes) > vfs.MaxIPCReadLimit {
		return nil, fmt.Errorf("net: response size exceeds maximum limit of %d bytes", vfs.MaxIPCReadLimit)
	}

	headerMap := make(map[string][]string)
	for k, v := range resp.Header {
		headerMap[k] = v
	}

	return map[string]any{
		"status":      resp.StatusCode,
		"status_text": resp.Status,
		"headers":     headerMap,
		"data_base64": base64.StdEncoding.EncodeToString(respBytes),
		"size":        len(respBytes),
	}, nil
}

func (s *NetService) status(ctx context.Context, req ipc.Request) (any, error) {
	policy := security.DefaultOperatorPolicy()
	if s.ctx != nil && s.ctx.Policy != nil {
		policy = s.ctx.Policy.Get()
	}

	return map[string]any{
		"policy_enabled": policy.Network.Enabled,
		"allowed_hosts":  policy.Network.AllowedHosts,
		"blocked_hosts":  policy.Network.BlockedHosts,
		"allowed_ports":  policy.Network.AllowedPorts,
		"allow_insecure": policy.Network.AllowInsecure,
	}, nil
}
