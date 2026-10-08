package agentd

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/hostmetrics"
)

const PermHostRead = "host.read"
const hostMetricsInterval = 15 * time.Second

type hostStatus struct {
	hostmetrics.Snapshot
	Status                string                 `json:"status"` // warming, current, stale
	AgeSeconds            *int64                 `json:"age_seconds,omitempty"`
	SampleIntervalSeconds int                    `json:"sample_interval_seconds"`
	Thresholds            hostmetrics.Thresholds `json:"thresholds"`
	Warnings              []hostmetrics.Warning  `json:"warnings"`
}

var hostCache struct {
	sync.RWMutex
	key   string
	value hostStatus // immutable after publication
}

// Sampling never runs on an API request. Slow/missing platform readers leave
// explicit partial observations; an older cached snapshot ages into stale.
func startHostMetricsPoller(stop <-chan struct{}) {
	go func() {
		refreshHostMetrics()
		ticker := time.NewTicker(hostMetricsInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				refreshHostMetrics()
			}
		}
	}()
}

var hostRefreshMu sync.Mutex

func refreshHostMetrics() {
	hostRefreshMu.Lock()
	defer hostRefreshMu.Unlock()
	key := config.DataDir()
	cfg, err := config.Load()
	paths := []hostmetrics.Path{{Kind: "data", Path: key}}
	if home, e := os.UserHomeDir(); e == nil {
		paths = append(paths, hostmetrics.Path{Kind: "work", Path: home})
	}
	groups, groupsErr := db.ListAgentGroups()
	if groupsErr == nil {
		for _, g := range groups {
			if !g.IsArchived() && g.DefaultCwd != "" {
				paths = append(paths, hostmetrics.Path{Kind: "work", Path: g.DefaultCwd})
			}
		}
	}
	if cfg != nil && cfg.Host != nil {
		for _, dir := range cfg.Host.WorkDirs {
			paths = append(paths, hostmetrics.Path{Kind: "work", Path: dir})
		}
	}
	s := hostmetrics.Read(paths)
	if err != nil {
		s.Errors = append(s.Errors, "Config: "+err.Error())
	}
	if groupsErr != nil {
		s.Errors = append(s.Errors, "Work directories: "+groupsErr.Error())
	}
	s.Tclaude, err = collectHostAgentLoad()
	if err != nil {
		s.Errors = append(s.Errors, "Agent load: "+err.Error())
	}
	load, ram, disk := cfg.HostWarningThresholds()
	thresholds := hostmetrics.Thresholds{LoadPerCore: load, RAMAvailablePercent: ram, DiskAvailablePercent: disk}
	result := hostStatus{Snapshot: s, Status: "current", SampleIntervalSeconds: int(hostMetricsInterval / time.Second), Thresholds: thresholds, Warnings: hostmetrics.Warnings(s, thresholds)}
	hostCache.Lock()
	hostCache.key = key
	hostCache.value = result
	hostCache.Unlock()
}
func collectHostAgentLoad() (*hostmetrics.AgentLoad, error) {
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return nil, fmt.Errorf("tmux: %w", err)
	}
	refs, err := db.HostSessionRefs()
	if err != nil {
		return nil, err
	}
	active, _, err := db.ListAgentRosterState()
	if err != nil {
		return nil, err
	}
	actors := map[string]bool{}
	for _, conv := range active {
		actors[conv] = true
	}
	liveSessions, liveAgents := map[string]bool{}, map[string]bool{}
	for _, r := range refs {
		if _, ok := alive[r.TmuxSession]; !ok {
			continue
		}
		liveSessions[r.TmuxSession] = true
		if actors[r.ConvID] {
			liveAgents[r.ConvID] = true
		}
	}
	return &hostmetrics.AgentLoad{LiveAgents: len(liveAgents), LiveSessions: len(liveSessions)}, nil
}
func cachedHostStatus() hostStatus {
	key := config.DataDir()
	hostCache.RLock()
	value, cacheKey := hostCache.value, hostCache.key
	hostCache.RUnlock()
	if cacheKey != key || value.ObservedAt.IsZero() {
		return hostStatus{Snapshot: hostmetrics.Snapshot{OS: runtime.GOOS, Arch: runtime.GOARCH, CPU: hostmetrics.CPU{LogicalCores: runtime.NumCPU()}, Disks: []hostmetrics.Disk{}}, Status: "warming", SampleIntervalSeconds: int(hostMetricsInterval / time.Second), Warnings: []hostmetrics.Warning{}}
	}
	age := max(int64(0), int64(time.Since(value.ObservedAt)/time.Second))
	value.AgeSeconds = &age
	if time.Since(value.ObservedAt) > 3*hostMetricsInterval {
		value.Status = "stale"
	}
	return value
}
func handleHostStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermHostRead); !ok {
		return
	}
	after, done, ok := beginForcedRead(w, r)
	if !ok {
		return
	}
	defer done()
	if !after.IsZero() {
		refreshHostMetrics()
		perfSpanFrom(r).mark("host_snapshot_forced")
	}
	writeJSON(w, http.StatusOK, cachedHostStatus())
}
