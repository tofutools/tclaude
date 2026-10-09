package agentd

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// One budget covers every fresh-read surface, so switching endpoints cannot
// turn the debugging escape hatch into a high-frequency poller.
const forcedReadInterval = 1500 * time.Millisecond

var forcedReads = struct {
	sync.Mutex
	callers map[string]forcedRead
}{callers: map[string]forcedRead{}}

type forcedRead struct {
	started time.Time
	active  bool
}

func beginForcedRead(w http.ResponseWriter, r *http.Request) (time.Time, func(), bool) {
	if r.URL.Query().Get("fresh") != "1" {
		return time.Time{}, func() {}, true
	}
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return time.Time{}, nil, false
	}
	if human {
		caller = "human"
	} else {
		if stable := peerAgentID(caller); stable != "" {
			caller = stable
		}
	}
	key := config.DataDir() + "\x00" + caller
	now := time.Now()
	forcedReads.Lock()
	entry := forcedReads.callers[key]
	if entry.active || now.Sub(entry.started) < forcedReadInterval {
		forcedReads.Unlock()
		w.Header().Set("Retry-After", "2")
		slog.Debug("forced snapshot read rate limited", "caller", caller, "path", r.URL.Path)
		writeError(w, http.StatusTooManyRequests, "fresh_rate_limit", "fresh reads are limited to one at a time and one per 1.5s per caller")
		return time.Time{}, nil, false
	}
	for id, old := range forcedReads.callers {
		if !old.active && now.Sub(old.started) > time.Minute {
			delete(forcedReads.callers, id)
		}
	}
	forcedReads.callers[key] = forcedRead{started: now, active: true}
	forcedReads.Unlock()
	slog.Debug("forced snapshot read", "caller", caller, "path", r.URL.Path)
	return now, func() {
		forcedReads.Lock()
		entry := forcedReads.callers[key]
		entry.active = false
		forcedReads.callers[key] = entry
		forcedReads.Unlock()
	}, true
}

// Call only after the endpoint's ordinary authorization. The timestamp is
// conservative: any joined gather must have started after this authorized read.
func requestStatusSnapshot(w http.ResponseWriter, r *http.Request, record func([]perfPhase)) (*statusSnapshot, bool) {
	after, done, ok := beginForcedRead(w, r)
	if !ok {
		return nil, false
	}
	defer done()
	span := perfSpanFrom(r)
	snapshot := gatheredStatusSnapshotAfter(after, record)
	if !after.IsZero() {
		span.mark("status_snapshot_forced")
	}
	return snapshot, true
}
