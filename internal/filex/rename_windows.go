//go:build windows

package filex

import (
	"errors"
	"syscall"
)

const (
	errAccessDenied     = syscall.Errno(5)  // ERROR_ACCESS_DENIED
	errSharingViolation = syscall.Errno(32) // ERROR_SHARING_VIOLATION
)

func isRetryableRenameErr(err error) bool {
	return errors.Is(err, errAccessDenied) || errors.Is(err, errSharingViolation)
}
