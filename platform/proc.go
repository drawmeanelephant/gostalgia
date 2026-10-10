package platform

import (
	"os"
	"strconv"
	"strings"
)

// ResourceUsage describes memory and CPU consumption for an OS process.
// If metrics cannot be obtained safely or portably, Supported is false
// and callers must report the metrics as unavailable rather than inventing zeroes.
type ResourceUsage struct {
	Supported   bool   `json:"supported"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	CPUUserMs   int64  `json:"cpu_user_ms,omitempty"`
	CPUSysMs    int64  `json:"cpu_sys_ms,omitempty"`
}

// SampleProcessState extracts CPU and peak-memory usage metrics from an
// exited process state. Memory is populated from the reaped child's rusage
// where the platform exposes it; it stays zero when unavailable.
func SampleProcessState(ps *os.ProcessState) ResourceUsage {
	if ps == nil {
		return ResourceUsage{Supported: false}
	}
	u := ResourceUsage{
		Supported: true,
		CPUUserMs: ps.UserTime().Milliseconds(),
		CPUSysMs:  ps.SystemTime().Milliseconds(),
	}
	if mem, ok := processStateMemory(ps); ok {
		u.MemoryBytes = mem
	}
	return u
}

// linuxParseProcStat parses /proc/<pid>/stat and returns (ppid, pgid,
// startTicks). comm may contain spaces and parens, so parsing starts after
// the last ')'. startTicks is the starttime field — the process's start
// epoch in clock ticks since boot — used as a pid-recycling guard.
func linuxParseProcStat(stat string) (ppid, pgid int, startTicks uint64, ok bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0, 0, 0, false
	}
	fields := strings.Fields(stat[i+1:])
	// fields: state ppid pgrp session tty_nr tpgid flags minflt cminflt
	// majflt cmajflt utime stime cutime cstime priority nice num_threads
	// itrealvalue starttime ...
	if len(fields) < 20 {
		return 0, 0, 0, false
	}
	ppid, err1 := strconv.Atoi(fields[1])
	pg, err2 := strconv.Atoi(fields[2])
	st, err3 := strconv.ParseUint(fields[19], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, 0, 0, false
	}
	return ppid, pg, st, true
}
