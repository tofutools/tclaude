package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Read-through refresh of a Copilot session's context/usage row.
//
// The shape follows the Codex read-through in codex_context_refresh.go: the
// dashboard and the agent API already read every session row, so the cheapest
// place to notice that a Copilot log has grown is the read that was going to
// happen anyway. What it does NOT copy is Codex's batching, perf-phase timing
// and cost-history machinery, because Copilot's durable log gives far less to
// persist — see copilot_telemetry.go for exactly what and why.
//
// The honest summary of what reaches the sessions row:
//
//   - tokens_output advances DURING a session, from assistant.message
//     outputTokens — the one per-turn token figure Copilot persists.
//   - tokens_input, context_pct and context_window_size only advance at an
//     authoritative disclosure: a compaction, a truncation, or a shutdown.
//     Between those the durable log is silent and nothing is written, so an
//     earlier real reading is never overwritten by a zero.
//
// Truly live per-call usage and context would need Copilot's stdout JSONL
// stream, where `assistant.usage` and `session.usage_info` are emitted but
// never persisted. That is a transport integration, not a file tracker, and
// is deliberately out of TCL-980's scope.

const (
	// copilotContextRefreshInterval throttles how often one session's log is
	// re-statted. A Copilot turn is seconds long at best and the dashboard
	// polls far faster than that.
	copilotContextRefreshInterval = 2 * time.Second

	// copilotCheckpointPersistMinInterval bounds how often the durable
	// follower checkpoint is rewritten. The checkpoint exists so a daemon
	// restart resumes at a byte offset instead of rescanning; writing it on
	// every poll would cost more than the rescan it saves.
	copilotCheckpointPersistMinInterval = 30 * time.Second
)

// copilotContextRefreshState is one session's memoized follower plus the
// bookkeeping that keeps polls cheap.
type copilotContextRefreshState struct {
	follower *harness.CopilotTelemetryFollower

	// convID and createdAt identify the SESSION GENERATION this follower was
	// built for. A pruned-and-recreated session can reuse an id while the
	// daemon's follower survives, and continuing the old fold would attribute
	// the previous conversation's totals to the new one.
	convID    string
	createdAt time.Time

	lastRefresh      time.Time
	checkpointLoaded bool
	checkpointAt     time.Time

	// persisted mirrors the last values written to the row, so an unchanged
	// projection costs no SQLite write at all.
	persistedOutput int64
	persistedInput  int64
	persistedWindow int64
	persistedPct    float64
}

var copilotContextRefreshMu struct {
	sync.Mutex
	states   map[string]*copilotContextRefreshState
	stopping bool
}

// refreshCopilotContextSnapshotOnRead brings one live Copilot session's
// context/usage columns up to date from its event log.
//
// It is a no-op for every other harness, for a dead session (a finished log
// cannot grow, and the row already holds its final reading), and for a session
// with no conversation id yet.
func refreshCopilotContextSnapshotOnRead(sess *db.SessionRow, alive bool) {
	if sess == nil || !alive || sess.Harness != harness.CopilotName ||
		sess.ID == "" || sess.ConvID == "" {
		return
	}
	state, ok := claimCopilotContextRefresh(sess, time.Now())
	if !ok {
		return
	}

	home := harness.CopilotHome()
	if home == "" {
		// No COPILOT_HOME means no session-state tree to follow. Not an error:
		// the same condition the conversation store reports as "no
		// conversations", and it must not spam the log on every poll.
		return
	}

	if !state.checkpointLoaded {
		loadCopilotCheckpoint(sess.ID, state)
		state.checkpointLoaded = true
	}

	snap, hydrated, err := state.follower.RuntimeTelemetry(home, sess.ConvID)
	if err != nil {
		slog.Warn("copilot-telemetry: failed read-through refresh",
			"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
		return
	}
	if !hydrated {
		// Copilot creates the session directory before the log. Nothing to say
		// yet, and writing zeros would blank a genuine earlier reading.
		return
	}

	persistCopilotContextSnapshot(sess, state, snap)
	persistCopilotCheckpoint(sess.ID, state)
}

// claimCopilotContextRefresh returns the session's follower state when this
// caller may do the work now. It rebuilds the state whenever the session
// generation changed, and refuses while the daemon is shutting down.
func claimCopilotContextRefresh(sess *db.SessionRow, now time.Time) (*copilotContextRefreshState, bool) {
	copilotContextRefreshMu.Lock()
	defer copilotContextRefreshMu.Unlock()
	if copilotContextRefreshMu.stopping {
		return nil, false
	}
	if copilotContextRefreshMu.states == nil {
		copilotContextRefreshMu.states = map[string]*copilotContextRefreshState{}
	}
	state := copilotContextRefreshMu.states[sess.ID]
	if state == nil || state.convID != sess.ConvID || !state.createdAt.Equal(sess.CreatedAt) {
		state = &copilotContextRefreshState{
			follower:  &harness.CopilotTelemetryFollower{},
			convID:    sess.ConvID,
			createdAt: sess.CreatedAt,
		}
		copilotContextRefreshMu.states[sess.ID] = state
	} else if now.Sub(state.lastRefresh) < copilotContextRefreshInterval {
		return nil, false
	}
	state.lastRefresh = now
	return state, true
}

// loadCopilotCheckpoint primes the follower from its durable checkpoint once
// per daemon lifetime per session. An unusable checkpoint is DELETED rather
// than retried: it would otherwise be reloaded and rejected on every restart.
func loadCopilotCheckpoint(sessionID string, state *copilotContextRefreshState) {
	checkpoint, err := db.LoadCopilotTelemetryCheckpoint(sessionID)
	if err != nil {
		slog.Warn("copilot-telemetry: failed to load durable follower checkpoint",
			"session_id", sessionID, "error", err, "module", "agentd")
		return
	}
	if checkpoint == nil || len(checkpoint.Data) == 0 {
		return
	}
	if err := state.follower.RestoreCheckpoint(checkpoint.Data); err != nil {
		slog.Warn("copilot-telemetry: discarded invalid durable follower checkpoint",
			"session_id", sessionID, "error", err, "module", "agentd")
		if delErr := db.DeleteCopilotTelemetryCheckpoint(sessionID); delErr != nil {
			slog.Warn("copilot-telemetry: failed to delete invalid durable follower checkpoint",
				"session_id", sessionID, "error", delErr, "module", "agentd")
		}
		return
	}
	state.checkpointAt = time.Now()
}

func persistCopilotCheckpoint(sessionID string, state *copilotContextRefreshState) {
	if !state.checkpointAt.IsZero() &&
		time.Since(state.checkpointAt) < copilotCheckpointPersistMinInterval {
		return
	}
	data, ok, err := state.follower.Checkpoint()
	if err != nil {
		slog.Warn("copilot-telemetry: failed to encode durable follower checkpoint",
			"session_id", sessionID, "error", err, "module", "agentd")
		return
	}
	if !ok {
		return
	}
	if err := db.SaveCopilotTelemetryCheckpoint(sessionID, data); err != nil {
		slog.Warn("copilot-telemetry: failed to persist durable follower checkpoint",
			"session_id", sessionID, "error", err, "module", "agentd")
		return
	}
	state.checkpointAt = time.Now()
}

// persistCopilotContextSnapshot writes the harness-agnostic context columns
// the dashboard already renders.
//
// Two rules make this safe on a surface shared with the other harnesses:
//
//   - A field Copilot has not disclosed carries FORWARD the last persisted
//     value instead of writing zero. db.UpdateContextSnapshot writes all four
//     columns at once, so sending a zero for the window would erase a real
//     limit an earlier compaction reported.
//   - Nothing is written when the projection is unchanged, which is the
//     common case: most polls observe a log that has not grown.
func persistCopilotContextSnapshot(
	sess *db.SessionRow,
	state *copilotContextRefreshState,
	snap harness.CopilotRuntimeSnapshot,
) {
	output := snap.AssistantOutputTokens
	input := state.persistedInput
	window := state.persistedWindow
	pct := state.persistedPct

	if snap.Usage != nil {
		// The shutdown record's totals supersede the running per-turn sum:
		// they are Copilot's own accounting for the same session.
		input = snap.Usage.InputTokens
		if snap.Usage.OutputTokens > 0 {
			output = snap.Usage.OutputTokens
		}
	}
	if snap.HasContext {
		if snap.Context.TokenLimit > 0 {
			window = snap.Context.TokenLimit
		}
		if computed := snap.Context.Pct(); computed > 0 {
			pct = computed
		}
	}

	if output == state.persistedOutput && input == state.persistedInput &&
		window == state.persistedWindow && pct == state.persistedPct {
		return
	}
	if err := db.UpdateContextSnapshot(sess.ID, pct, input, output, window); err != nil {
		slog.Warn("copilot-telemetry: failed to persist context snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	state.persistedOutput = output
	state.persistedInput = input
	state.persistedWindow = window
	state.persistedPct = pct
}

// stopCopilotContextRefreshes blocks further refreshes during daemon shutdown
// and drops the memoized followers, mirroring the Codex path.
func stopCopilotContextRefreshes() {
	copilotContextRefreshMu.Lock()
	defer copilotContextRefreshMu.Unlock()
	copilotContextRefreshMu.stopping = true
	copilotContextRefreshMu.states = nil
}
