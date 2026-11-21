// Package ipc is the environment's inter-process communication layer: one
// router with namespaced methods, served both in-process (direct
// Dispatch) and over a local socket (NDJSON request/response, token
// handshake). The wire format is one JSON value per line — debuggable with
// plain shell tools, and extensible to streaming without re-framing.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"

	"gostalgia/internal/security"
)

// Request is one method call. Method is namespaced: "sys/status",
// "proc/list", "fs/read", "app/<app-id>/<method>".
type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
	kind   string          // control-frame kind parsed off the wire; "" for calls
}

// kindCancel identifies the fire-and-forget cancel frame: a client sends
// {"kind":"cancel","id":N} when it abandons call N; the server releases the
// call's in-flight slot and cancels its handler context. Peers that never
// send it are unaffected.
const kindCancel = "cancel"

// cancelFrame is the wire form of a call cancellation.
type cancelFrame struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

// Response is the result of one method call.
type Response struct {
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Handler serves one method. Returned errors become response errors;
// returned values are JSON-encoded into the response data.
type Handler func(ctx context.Context, req Request) (any, error)

// Encode marshals v for transport.
func Encode(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("ipc: encode: %w", err)
	}
	return b, nil
}

// DecodeParams unmarshals request params into out. Empty params are not
// an error (out is left untouched).
func DecodeParams(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ipc: bad params: %w", err)
	}
	return nil
}

type capsKey struct{}

// WithCapabilities returns a context carrying the caller's capabilities.
// The socket server attaches the authenticated client's set; in-process
// callers attach their own.
func WithCapabilities(ctx context.Context, caps *security.Capabilities) context.Context {
	return context.WithValue(ctx, capsKey{}, caps)
}

// Capabilities returns the caller's capability set, or nil.
func Capabilities(ctx context.Context) *security.Capabilities {
	caps, _ := ctx.Value(capsKey{}).(*security.Capabilities)
	return caps
}

// RequireCap returns an error if the call context lacks capability.
func RequireCap(ctx context.Context, capability string) error {
	caps := Capabilities(ctx)
	if caps == nil || !caps.Has(capability) {
		return fmt.Errorf("permission denied: missing capability %q", capability)
	}
	return nil
}

type principalKey struct{}

// WithPrincipal returns a context carrying the caller's authenticated principal.
func WithPrincipal(ctx context.Context, p security.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// CallerPrincipal returns the caller's authenticated principal, or a zero Principal.
func CallerPrincipal(ctx context.Context) security.Principal {
	p, _ := ctx.Value(principalKey{}).(security.Principal)
	return p
}
