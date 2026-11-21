package platform

import "errors"

// ErrLocked reports that another process holds the environment root lock.
// Callers should treat it as "environment already running on this root".
var ErrLocked = errors.New("platform: environment root is locked")

// rootLockPath places the boot lock beside the root (root + ".lock"), so
// a refused boot fails before creating or mutating the root itself.
func rootLockPath(root string) string { return root + ".lock" }
