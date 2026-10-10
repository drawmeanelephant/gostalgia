//go:build !windows

package filex

func isRetryableRenameErr(error) bool { return false }
