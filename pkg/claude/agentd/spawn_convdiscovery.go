package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Spawn-time conv-id discovery for a harness that does NOT report its conv-id
// through an immediate launch hook.
//
// Claude Code fires a SessionStart hook the moment its TUI comes up, so the
// daemon learns the new conv-id within ~1s and executeSpawn's DB poll wins.
// Codex fires NO hook until the first user turn — but it writes its rollout
// file (whose name + session_meta carry the session-id) ~1s after launch. So
// for Codex the conv-id is knowable from the conv store long before any hook;
// executeSpawn falls back to discovering it here instead of blocking until a
// human (or a peer) sends the first message — the spawn-freeze in JOH-205.

const (
	// convStoreDiscoveryGrace is how long after launch executeSpawn waits
	// before it starts scanning the harness conv store, giving an immediate
	// launch hook (Claude Code) the first shot and the rollout file (Codex)
	// time to appear.
	convStoreDiscoveryGrace = 1 * time.Second

	// convStoreDiscoveryScanInterval throttles the (tree-walking) conv-store
	// scan so a slow-to-materialise conv doesn't trigger a fresh scan on every
	// 250ms poll iteration.
	convStoreDiscoveryScanInterval = 1 * time.Second

	// convStoreDiscoverySkew absorbs sub-second / rounding differences between
	// the daemon's recorded launch time and the harness-recorded conversation
	// creation time, so a conv created right at launch is not excluded as
	// "pre-existing".
	convStoreDiscoverySkew = 3 * time.Second
)

// discoverSpawnedConvID resolves the conv-id of a session just launched in
// cwd at/after `since` by scanning the harness's conv store. It returns the
// newest conversation in cwd whose creation time is at/after the launch
// (minus a small skew) and that is NOT already another session's conv-id; ""
// when nothing matches yet (the caller keeps polling). It is the fallback for
// a harness (Codex) that does not report its conv-id through an immediate
// launch hook.
//
// A harness with no conv store (or a nil descriptor — an empty/unknown
// `--harness`) yields "", so the caller simply stays on the hook-poll path
// (Claude Code's behaviour is unchanged: its hook delivers the conv-id before
// the grace elapses).
func discoverSpawnedConvID(h *harness.Harness, cwd string, since time.Time) string {
	if h == nil || h.Convs == nil {
		return ""
	}
	entries, err := h.Convs.ListConvs(cwd)
	if err != nil {
		slog.Warn("spawn: conv-store discovery scan failed",
			"harness", h.Name, "cwd", cwd, "error", err)
		return ""
	}
	cutoff := since.Add(-convStoreDiscoverySkew)
	var bestID string
	var bestAt time.Time
	for _, e := range entries {
		if e.SessionID == "" {
			continue
		}
		created := convEntryCreated(e)
		if created.IsZero() || created.Before(cutoff) {
			continue // a pre-existing conversation, not the one we just launched
		}
		// Don't claim a conv-id that already belongs to another session row —
		// guards against grabbing a concurrent spawn's conv in the same cwd.
		// The current spawn's own row still has an empty conv_id here (we set
		// it only once we return), so it never self-excludes.
		if rows, err := db.FindSessionsByConvID(e.SessionID); err == nil && len(rows) > 0 {
			continue
		}
		if bestID == "" || created.After(bestAt) {
			bestID, bestAt = e.SessionID, created
		}
	}
	return bestID
}

// convEntryCreated returns a conversation's creation time, preferring the
// harness-recorded Created timestamp (RFC3339) and falling back to the file
// mtime. Zero time when neither is usable (the caller treats that as "not a
// match").
func convEntryCreated(e convops.SessionEntry) time.Time {
	if e.Created != "" {
		if t, err := time.Parse(time.RFC3339, e.Created); err == nil {
			return t
		}
	}
	if e.FileMtime > 0 {
		return time.Unix(e.FileMtime, 0)
	}
	return time.Time{}
}
