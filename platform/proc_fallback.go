//go:build !unix && !windows

package platform

import "os"

// processStateMemory is unavailable on platforms without rusage behind
// ProcessState.SysUsage; memory stays unreported.
func processStateMemory(ps *os.ProcessState) (uint64, bool) {
	return 0, false
}
