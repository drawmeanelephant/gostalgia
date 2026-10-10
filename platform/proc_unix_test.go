//go:build unix

package platform

import (
	"os/exec"
	"testing"
)

// TestSampleProcessStateReportsMemory verifies that an exited child's peak
// RSS is reported from rusage instead of being invented as zero (issue
// #154): every exec'd process has a non-zero ru_maxrss.
func TestSampleProcessStateReportsMemory(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run child: %v", err)
	}
	u := SampleProcessState(cmd.ProcessState)
	if !u.Supported {
		t.Fatal("SampleProcessState: Supported=false for a reaped child")
	}
	if u.MemoryBytes == 0 {
		t.Fatal("SampleProcessState: MemoryBytes=0 for a process that ran and exited")
	}
}

func TestLinuxTicksToMs(t *testing.T) {
	for _, tc := range []struct {
		ticks, hz, want int64
	}{
		{5, 100, 50}, // CLK_TCK=100: the historical *10
		{5, 250, 20}, // HZ=250 kernels
		{5, 1000, 5}, // HZ=1000 kernels
		{7, 60, 116}, // odd rate still converts proportionally
		{5, 0, 0},    // degenerate hz never divides
		{5, -3, 0},
	} {
		if got := linuxTicksToMs(tc.ticks, tc.hz); got != tc.want {
			t.Errorf("linuxTicksToMs(%d, %d) = %d, want %d", tc.ticks, tc.hz, got, tc.want)
		}
	}
	if got := linuxClockTicks(); got <= 0 {
		t.Errorf("linuxClockTicks() = %d, want positive (getconf CLK_TCK or the 100 fallback)", got)
	}
}
