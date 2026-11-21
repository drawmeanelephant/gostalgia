package sdk

import (
	"context"
	"errors"
	"io/fs"
)

// Wire error codes carried alongside every IPC error message. The
// filesystem taxonomy matches the environment's vfs.ErrorCode; the rest
// classify transport and handler failures. "unknown" means no finer
// classification was available. See docs/ipc.md for the error contract.
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

// Error is a failed Call carrying the wire's machine-readable Code next to
// the human-readable Message. Code-aware peers fill both; an older peer
// leaves Code empty. Unwrap maps the code back to the matching fs/context
// sentinel, so errors.Is(err, fs.ErrNotExist) or errors.Is(err,
// context.Canceled) work the same in-process and over the wire.
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

// ErrorCode exposes the wire code to classification by errors.As without
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

// ErrorCode returns the machine-readable code carried by err, or "" when
// the error declares none. An error may declare its code by implementing
// ErrorCode() string; the fs and context sentinels map to the equivalent
// codes so wrapped filesystem failures classify like wire errors.
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
		return ""
	}
}
