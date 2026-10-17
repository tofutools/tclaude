// Package perfstats holds the cheap, always-on counters agentd exposes on its
// opt-in profiling endpoint. A CPU profile shows where Go code spends time, but
// two common sources of idle daemon CPU hide from it: how often background
// loops wake up, and the CPU burned by the subprocesses (tmux, git, …) those
// loops fork, which top attributes to the children rather than to agentd.
// These counters make both visible next to the pprof profiles.
package perfstats

import (
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	startedAt  = time.Now()
	subprocMu  sync.Mutex
	subprocs   = map[string]*atomic.Int64{}
	subprocAll atomic.Int64
)

// CountSubprocess records that a subprocess identified by key (for example
// "tmux list-sessions") is about to be started. It costs one map lookup under
// a mutex, so it is safe to leave in hot paths unconditionally.
func CountSubprocess(key string) {
	subprocAll.Add(1)
	subprocMu.Lock()
	c, ok := subprocs[key]
	if !ok {
		c = &atomic.Int64{}
		subprocs[key] = c
	}
	subprocMu.Unlock()
	c.Add(1)
}

// SubprocessCount is one row of the subprocess counter table.
type SubprocessCount struct {
	Command string `json:"command"`
	Count   int64  `json:"count"`
}

// Snapshot is a point-in-time view of the process. Counters are cumulative
// since process start; diff two snapshots to get rates.
type Snapshot struct {
	TakenAt          time.Time         `json:"taken_at"`
	UptimeSeconds    float64           `json:"uptime_seconds"`
	Goroutines       int               `json:"goroutines"`
	NumCPU           int               `json:"num_cpu"`
	GOMAXPROCS       int               `json:"gomaxprocs"`
	HeapAllocBytes   uint64            `json:"heap_alloc_bytes"`
	HeapObjects      uint64            `json:"heap_objects"`
	TotalAllocBytes  uint64            `json:"total_alloc_bytes"`
	NumGC            uint32            `json:"num_gc"`
	GCPauseTotalNs   uint64            `json:"gc_pause_total_ns"`
	CPU              CPUTimes          `json:"cpu"`
	SubprocessTotal  int64             `json:"subprocess_total"`
	Subprocesses     []SubprocessCount `json:"subprocesses"`
	SubprocessNotice string            `json:"subprocess_notice"`
}

// CPUTimes is cumulative CPU time in seconds. Children covers only
// subprocesses that have exited and been waited for.
type CPUTimes struct {
	SelfUserSeconds     float64 `json:"self_user_seconds"`
	SelfSystemSeconds   float64 `json:"self_system_seconds"`
	ChildrenUserSeconds float64 `json:"children_user_seconds"`
	ChildrenSysSeconds  float64 `json:"children_system_seconds"`
	Available           bool    `json:"available"`
}

// Take captures a Snapshot. runtime.ReadMemStats briefly stops the world, so
// this is meant for on-demand reads, not tight loops.
func Take() Snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	now := time.Now()
	s := Snapshot{
		TakenAt:         now,
		UptimeSeconds:   now.Sub(startedAt).Seconds(),
		Goroutines:      runtime.NumGoroutine(),
		NumCPU:          runtime.NumCPU(),
		GOMAXPROCS:      runtime.GOMAXPROCS(0),
		HeapAllocBytes:  ms.HeapAlloc,
		HeapObjects:     ms.HeapObjects,
		TotalAllocBytes: ms.TotalAlloc,
		NumGC:           ms.NumGC,
		GCPauseTotalNs:  ms.PauseTotalNs,
		CPU:             readCPUTimes(),
		SubprocessTotal: subprocAll.Load(),
		SubprocessNotice: "subprocess counts cover instrumented call sites (tmux); " +
			"cpu.children_* covers every reaped child process",
	}
	subprocMu.Lock()
	for k, c := range subprocs {
		s.Subprocesses = append(s.Subprocesses, SubprocessCount{Command: k, Count: c.Load()})
	}
	subprocMu.Unlock()
	sortCounts(s.Subprocesses)
	return s
}

func sortCounts(rows []SubprocessCount) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Count != rows[j].Count {
			return rows[i].Count > rows[j].Count
		}
		return rows[i].Command < rows[j].Command
	})
}

// SubprocessDelta returns per-command counts started between before and after,
// highest first, dropping commands that did not run in the window.
func SubprocessDelta(before, after Snapshot) []SubprocessCount {
	prev := make(map[string]int64, len(before.Subprocesses))
	for _, r := range before.Subprocesses {
		prev[r.Command] = r.Count
	}
	var out []SubprocessCount
	for _, r := range after.Subprocesses {
		if d := r.Count - prev[r.Command]; d > 0 {
			out = append(out, SubprocessCount{Command: r.Command, Count: d})
		}
	}
	sortCounts(out)
	return out
}
