// Package ipc is the environment's inter-process communication layer: one
// router with namespaced methods, served both in-process (direct
// Dispatch) and over a local socket (NDJSON request/response, token
// handshake). The wire format is one JSON value per line — debuggable with
// plain shell tools, and extensible to streaming without re-framing.
package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"gostalgia/internal/security"
)

// Request is one method call. Method is namespaced: "sys/status",
// "proc/list", "fs/read", "app/<app-id>/<method>".
type Request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the result of one method call. On failure, Error carries the
// human-readable message and Code the machine-readable classification; peers
// speaking an older protocol see the same string in Error and ignore Code.
type Response struct {
	ID    int64           `json:"id"`
	OK    bool            `json:"ok"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
}

// Error codes reported on the wire. The filesystem taxonomy matches
// vfs.ErrorCode; the rest classify transport and handler failures. Every
// error response carries a code; "unknown" means no finer classification
// was available.
const (
	CodeNotFound           = "not_found"
	CodeAlreadyExists      = "already_exists"
	CodeIsDir              = "is_directory"
	CodeNotDir             = "not_directory"
	CodeCrossMount         = "cross_mount"
	CodePathEscape         = "path_escape"
	CodePermissionDenied   = "permission_denied"
	CodeReadOnly           = "read_only"
	CodeTooLarge           = "too_large"
	CodeInvalid            = "invalid"
	CodeIO                 = "io"
	CodeUnauthorized       = "unauthorized"
	CodeUnknownMethod      = "unknown_method"
	CodeCanceled           = "canceled"
	CodeDeadlineExceeded   = "deadline_exceeded"
	CodePolicyDenied       = "policy_denied"
	CodeUnsupportedVersion = "unsupported_version"
	CodeUnavailable        = "unavailable"
	CodeInternal           = "internal"
	CodeUnknown            = "unknown"
)

// Error is an IPC failure carrying a machine-readable Code alongside the
// human-readable Message. It is produced by RequireCap and reconstructed on
// the client side from the wire's code field.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string { return e.Message }

func (e *Error) Unwrap() error {
	if e.Err != nil {
		return e.Err
	}
	return sentinelForCode(e.Code)
}

// ErrorCode exposes the wire code to the router's classification without
// requiring callers to know the concrete type.
func (e *Error) ErrorCode() string { return e.Code }

// sentinelForCode maps a wire code to the sentinel error it stands for, so
// errors.Is works the same on both sides of the transport.
func sentinelForCode(code string) error {
	switch code {
	case CodeNotFound:
		return fs.ErrNotExist
	case CodeAlreadyExists:
		return fs.ErrExist
	case CodePermissionDenied, CodeReadOnly:
		return fs.ErrPermission
	case CodeInvalid, CodeIsDir, CodeNotDir, CodeCrossMount, CodePathEscape:
		return fs.ErrInvalid
	case CodeCanceled:
		return context.Canceled
	case CodeDeadlineExceeded:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// ErrorCode classifies err for the wire. An error may declare its own code
// by implementing ErrorCode() string (as *vfs.Error and *Error do); the
// fs and context sentinels cover the rest, and anything unclassified is
// reported as "unknown".
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var c interface{ ErrorCode() string }
	if errors.As(err, &c) {
		if code := c.ErrorCode(); code != "" {
			return code
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return CodeNotFound
	case errors.Is(err, fs.ErrExist):
		return CodeAlreadyExists
	case errors.Is(err, fs.ErrPermission):
		return CodePermissionDenied
	case errors.Is(err, fs.ErrInvalid):
		return CodeInvalid
	case errors.Is(err, context.Canceled):
		return CodeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return CodeDeadlineExceeded
	default:
		return CodeUnknown
	}
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
		return &Error{
			Code:    CodePermissionDenied,
			Message: fmt.Sprintf("permission denied: missing capability %q", capability),
		}
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
