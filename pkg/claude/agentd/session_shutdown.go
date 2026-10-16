package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// --- recently shut-down sessions, remembered in memory ---
//
// A harness fires its SessionEnd hook on its way out and exits without
// waiting for it, so that last callback reaches the daemon from an orphan
// whose pane is going or gone, and the live-pane proof refuses it. That is
// ordinary teardown, not a starved agent. The daemon knows when a session is
// shutting down — it started the soft exit (retire, stop), or it recorded the
// pane's exit itself (reaper, soft-exit escalation, SessionEnd recording) — so
// it remembers that here for a short window and the hook broker treats a
// failed SessionEnd claiming such a session as teardown (hook_broker.go).
//
// Exits recorded by the separate `tclaude session exit-callback` process that
// tmux's pane-died hook runs are NOT seen here: that process has its own
// memory. A self-initiated exit (/exit, a crash) therefore relies on the
// proof-side inference (layerProof.orphanTeardown), as does everything after
// a daemon restart, since this is memory only, like the refusal recorder.

// sessionShutdownTTL bounds how long a shutdown is remembered: long enough to
// cover the shutdown window (the last hook lands within about a second), short
// enough that a reused or relaunched session id is soon unaffected. Even inside
// it the entry changes nothing for a caller that passes the proof — it only
// turns an already-refused SessionEnd's WARN into an info line.
const sessionShutdownTTL = 2 * time.Minute

var sessionShutdowns = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// markSessionShuttingDown records that sessionID (the session row id) is
// shutting down or has shut down.
func markSessionShuttingDown(sessionID string) {
	if sessionID == "" {
		return
	}
	now := time.Now()
	sessionShutdowns.Lock()
	defer sessionShutdowns.Unlock()
	for id, at := range sessionShutdowns.at {
		if now.Sub(at) > sessionShutdownTTL {
			delete(sessionShutdowns.at, id)
		}
	}
	sessionShutdowns.at[sessionID] = now
}

// sessionRecentlyShutDown reports whether sessionID was marked within the TTL.
func sessionRecentlyShutDown(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	sessionShutdowns.Lock()
	defer sessionShutdowns.Unlock()
	at, ok := sessionShutdowns.at[sessionID]
	return ok && time.Since(at) <= sessionShutdownTTL
}

func init() {
	db.AgentExitObservedHook = markSessionShuttingDown
}
