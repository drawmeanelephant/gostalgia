// Package filex holds small shared helpers for filesystem operations that
// differ subtly across platforms.
package filex

import "time"

// RenameRetry retries fn when the platform reports a transient sharing
// violation. On Windows, os.Rename / os.Root.Rename cannot replace a
// destination that another handle has open for reading unless that handle
// granted delete sharing — os.ReadFile does not — so a reader racing an
// atomic save makes MoveFileEx fail with ERROR_ACCESS_DENIED or
// ERROR_SHARING_VIOLATION. Readers hold the file for microseconds, so a
// brief bounded retry turns a spurious failure into a delayed success.
// Other platforms never retry (see isRetryableRenameErr) and this is a
// single fn() call.
func RenameRetry(fn func() error) error {
	err := fn()
	for i := 0; i < 100 && err != nil && isRetryableRenameErr(err); i++ {
		time.Sleep(10 * time.Millisecond)
		err = fn()
	}
	return err
}
