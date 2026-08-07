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

	// refreshing is the in-flight guard. Everything below the claim is
	// mutated WITHOUT the package mutex, so exactly one goroutine per session
	// may be past a claim at a time. The throttle alone cannot provide that:
	// it measures from the START of the previous refresh, so a full rebuild of
	// a large log outlives its own throttle window and a second caller would
	// otherwise enter concurrently.
	refreshing bool

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
	defer releaseCopilotContextRefresh(state)

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
	persistCopilotCheckpoint(sess, state)
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
		// A generation change abandons the old state entirely — including any
		// refresh still in flight against it, which now writes into an object
		// nothing will read again.
		state = &copilotContextRefreshState{
			follower:  &harness.CopilotTelemetryFollower{},
			convID:    sess.ConvID,
			createdAt: sess.CreatedAt,
		}
		copilotContextRefreshMu.states[sess.ID] = state
	} else if state.refreshing || now.Sub(state.lastRefresh) < copilotContextRefreshInterval {
		return nil, false
	}
	state.refreshing = true
	state.lastRefresh = now
	return state, true
}

func releaseCopilotContextRefresh(state *copilotContextRefreshState) {
	copilotContextRefreshMu.Lock()
	defer copilotContextRefreshMu.Unlock()
	state.refreshing = false
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

func persistCopilotCheckpoint(sess *db.SessionRow, state *copilotContextRefreshState) {
	if !state.checkpointAt.IsZero() &&
		time.Since(state.checkpointAt) < copilotCheckpointPersistMinInterval {
		return
	}
	data, ok, err := state.follower.Checkpoint()
	if err != nil {
		slog.Warn("copilot-telemetry: failed to encode durable follower checkpoint",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if !ok {
		return
	}
	// Generation-guarded: a session pruned and recreated between the claim and
	// this write must not inherit the previous conversation's cursor. A refused
	// write is a normal outcome, not an error.
	saved, err := db.SaveCopilotTelemetryCheckpoint(sess.ID, sess.ConvID, sess.CreatedAt, data)
	if err != nil {
		slog.Warn("copilot-telemetry: failed to persist durable follower checkpoint",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if saved {
		state.checkpointAt = time.Now()
	}
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

	// PRECEDENCE, when the read-only sweep of Copilot's own SQLite store
	// (copilot_usage_poller.go) has rows for this session.
	//
	// Those rows are the fine-grained live source: they carry per-call tokens
	// that the durable event log cannot, because the events that would report
	// them are `ephemeral` and never written to disk. The durable log stays
	// authoritative for the two things the store genuinely cannot supply — the
	// context DENOMINATOR (the store has no token limit, and neither does any
	// other file: the CLI reads it from an in-memory model catalog) and the
	// compaction/truncation/shutdown disclosures that produce it.
	//
	// So the numerator is taken from the sweep UNCONDITIONALLY once it has seen
	// a row, and the durable log's occupancy figures are not consulted for it.
	// That needs no clock comparison between the two sources and cannot go
	// stale across a compaction: a compaction RESETS the window, and the very
	// next model call's input_tokens already reflects the post-compaction
	// window, so the store is self-correcting and strictly fresher. Before the
	// sweep's first row, everything below behaves exactly as it did.
	//
	// Both writers reaching the same values matters as much as which wins. They
	// share copilotContextPct AND copilotEffectiveContextWindow (configured
	// cap, else the observed model's static assumption, else the disclosed
	// window), so the sweep and this refresh converge rather than flapping the
	// row between two answers on alternating polls.
	live, hasLive := lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)

	if snap.Usage != nil {
		if snap.Usage.InputTokens > 0 {
			input = snap.Usage.InputTokens
		}
		// The larger of the two, NOT the shutdown figure. A shutdown record in
		// a log that is still growing means the session was RESUMED, which is
		// the case this whole follower exists for: taking the shutdown total
		// unconditionally would pin tokens_output to its pre-resume value and
		// stop the one durable per-turn figure from ever advancing again.
		// Copilot's own accounting wins while it is ahead; the running sum
		// takes over once the new lifetime passes it.
		output = max(output, snap.Usage.OutputTokens)
	}
	if snap.HasContext {
		// The window and the percentage move TOGETHER or not at all. They
		// describe the same observation, and carrying one forward past a
		// change in the other produces a row that is internally false: a
		// session.truncation discloses a limit but no total occupancy, so
		// updating the window alone would pair a fresh (possibly much larger)
		// denominator with a stale numerator's percentage and render an
		// occupancy Copilot never reported.
		observedPct := snap.Context.Pct()
		observedWindow := snap.Context.TokenLimit
		if observedPct > 0 {
			window, pct = observedWindow, observedPct
		} else if observedWindow > 0 {
			// A limit with no computable occupancy: record the limit and drop
			// the percentage rather than keep one that no longer applies.
			//
			// This deliberately does NOT require the limit to have CHANGED.
			// session.truncation normally restates the same limit — the window
			// only moves on a model change — so gating on a changed limit
			// would leave the common case stale: a truncation that cut the
			// conversation to 1000 tokens would keep rendering the 93% the
			// preceding compaction measured. What makes the percentage invalid
			// is the new observation, not a new denominator.
			window, pct = observedWindow, 0
		}
	}

	if hasLive {
		// The sweep owns the numerator, and the percentage is recomputed here
		// against the same EFFECTIVE denominator the sweep resolves — the
		// configured cap, else the observed model's static assumption, else
		// whatever window the durable log disclosed (already absorbed into
		// `window` above). Using the raw `window` alone was the TCL-1048
		// follow-up bug: Copilot rarely discloses a window, so this branch
		// recomputed pct against 0 and overwrote the sweep's real percentage
		// on every token advance while the counts kept landing. `window`
		// itself is still what the observed column persists — the effective
		// denominator shapes only the percentage.
		input = live.ContextTokens
		// max() for the same reason the shutdown total uses it: the two sources
		// count different things (the sweep counts calls it has consumed, the
		// durable log's shutdown total is restored across a resume), and
		// whichever is ahead is the one that has seen more of the session.
		output = max(output, live.OutputTokens)
		pct = copilotContextPct(input,
			copilotEffectiveContextWindow(sess.ConvID, live.Model, window))
	}

	if output == state.persistedOutput && input == state.persistedInput &&
		window == state.persistedWindow && pct == state.persistedPct {
		return
	}
	// Generation-guarded, for the same reason the checkpoint write is. This
	// projection was derived from a log read that began BEFORE the write: a
	// session pruned and recreated in between would otherwise receive the
	// previous conversation's tokens and window on its brand-new row.
	updated, err := db.UpdateContextSnapshotForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, pct, input, output, window)
	if err != nil {
		slog.Warn("copilot-telemetry: failed to persist context snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	if !updated {
		// The generation moved on. Leave `persisted*` alone so this state is
		// not mistaken for a mirror of a row it never wrote; the next claim
		// rebuilds the follower for the new generation anyway.
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
