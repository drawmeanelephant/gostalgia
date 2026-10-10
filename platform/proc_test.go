package platform

import (
	"math"
	"os"
	"os/exec"
	"testing"
)

func TestSampleProcessResources(t *testing.T) {
	u := SampleProcessResources(-1)
	if u.Supported {
		t.Errorf("expected Supported=false for negative pid, got true")
	}

	u = SampleProcessResources(0)
	if u.Supported {
		t.Errorf("expected Supported=false for zero pid, got true")
	}

	stateUsage := SampleProcessState(nil)
	if stateUsage.Supported {
		t.Errorf("expected Supported=false for nil ProcessState, got true")
	}
}

func TestKillProcessTreeNil(t *testing.T) {
	if err := KillProcessTree(nil); err != nil {
		t.Errorf("KillProcessTree(nil) error: %v", err)
	}
	if err := KillProcessTree(&exec.Cmd{}); err != nil {
		t.Errorf("KillProcessTree(&exec.Cmd{}) error: %v", err)
	}
}

// TestProcessAlive checks the liveness probe against the running test
// process and pids that cannot exist.
func TestProcessAlive(t *testing.T) {
	self := os.Getpid()
	if !ProcessAlive(self) {
		t.Errorf("ProcessAlive(%d) = false for the running test process", self)
	}
	for _, pid := range []int{0, -1, math.MaxInt32} {
		if ProcessAlive(pid) {
			t.Errorf("ProcessAlive(%d) = true, want false", pid)
		}
	}
}

func TestLinuxParseProcStat(t *testing.T) {
	// Fields after comm: state ppid pgrp session tty_nr tpgid flags
	// minflt cminflt majflt cmajflt utime stime cutime cstime priority
	// nice num_threads itrealvalue starttime ...
	stat := "1234 (weird (name) here) S 1233 1234 1234 0 -1 4194304 100 0 0 0 5 2 0 0 20 0 1 0 987654 1000 18446744073709551615 1 1 0 0 0 0"
	ppid, pgid, start, ok := linuxParseProcStat(stat)
	if !ok {
		t.Fatal("linuxParseProcStat failed on a valid stat line")
	}
	if ppid != 1233 || pgid != 1234 || start != 987654 {
		t.Fatalf("linuxParseProcStat = (%d, %d, %d), want (1233, 1234, 987654)", ppid, pgid, start)
	}

	for _, bad := range []string{
		"",                // empty
		"1234 no paren",   // no comm close
		"1234 (comm) S 1", // too few fields
		"1234 (comm) S x y 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0",     // non-numeric ppid/pgrp
		"1234 (comm) S 1 2 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 zzz", // non-numeric starttime
	} {
		if _, _, _, ok := linuxParseProcStat(bad); ok {
			t.Errorf("linuxParseProcStat(%q) = ok, want failure", bad)
		}
	}
}
