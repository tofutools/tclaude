package agentd

import (
	"errors"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	codexContextRefreshMinInterval = time.Second
	// A durable checkpoint only bounds restart replay; the in-memory follower
	// remains authoritative while agentd is alive. Avoid turning every changed
	// rollout observed by the two-second dashboard poll into its own SQLite
	// commit. Thirty seconds keeps restart replay small while moving routine
	// checkpoint fsyncs out of the poll's median path.
	codexCheckpointPersistMinInterval    = 30 * time.Second
	codexCheckpointFailureEvictThreshold = 3
)

var codexContextRefreshMu struct {
	sync.Mutex
	last map[string]codexReadThroughSnapshot
}

type codexReadThroughSnapshot struct {
	at                    time.Time
	interruptedSubagents  map[string]struct{}
	follower              *harness.CodexTelemetryFollower
	refreshing            bool
	checkpointLoaded      bool
	checkpointData        string
	checkpointFailures    int
	checkpointPersistedAt time.Time
	sessionConvID         string
	sessionCreatedAt      time.Time
	persistedConvID       string
	persistedCreatedAt    time.Time
	persistedContext      harness.ContextTelemetry
	persistedHasContext   bool
	persistedReset        bool
}

// codexTelemetryTiming is accumulated across every live Codex row rendered by
// one dashboard snapshot. The child durations explain the top-level
// codex_telemetry phase without changing its wall-clock accounting.
type codexTelemetryTiming struct {
	total            time.Duration
	claim            time.Duration
	checkpointLoad   time.Duration
	rolloutRead      time.Duration
	checkpointEncode time.Duration
	checkpointWrite  time.Duration
	contextWrite     time.Duration
}

func (t codexTelemetryTiming) add(other codexTelemetryTiming) codexTelemetryTiming {
	t.total += other.total
	t.claim += other.claim
	t.checkpointLoad += other.checkpointLoad
	t.rolloutRead += other.rolloutRead
	t.checkpointEncode += other.checkpointEncode
	t.checkpointWrite += other.checkpointWrite
	t.contextWrite += other.contextWrite
	return t
}

func (t codexTelemetryTiming) perfPhases() []perfPhase {
	accounted := t.claim + t.checkpointLoad + t.rolloutRead + t.checkpointEncode +
		t.checkpointWrite + t.contextWrite
	other := t.total - accounted
	if other < 0 {
		other = 0
	}
	return []perfPhase{
		{Name: "claim", Ms: durMs(t.claim)},
		{Name: "checkpoint_load", Ms: durMs(t.checkpointLoad)},
		{Name: "rollout_read", Ms: durMs(t.rolloutRead)},
		{Name: "checkpoint_encode", Ms: durMs(t.checkpointEncode)},
		{Name: "checkpoint_write", Ms: durMs(t.checkpointWrite)},
		{Name: "context_write", Ms: durMs(t.contextWrite)},
		{Name: "other", Ms: durMs(other)},
	}
}

// refreshCodexContextSnapshotOnRead gives Codex the same dashboard freshness
// Claude Code gets from its command-statusline/hooks: before a live Codex row's
// snapshot is rendered, scan its rollout once to lift the latest token_count
// into sessions.context_* and harvest interrupted collaboration children from
// sub_agent_activity. Context persistence remains best-effort; the returned
// interrupted-child set is a read-through value because Codex's rollout is
// authoritative for that terminal fact when its SubagentStop hook was lost.
func refreshCodexContextSnapshotOnRead(sess *db.SessionRow, alive bool) map[string]struct{} {
	return refreshCodexContextSnapshotOnReadTimed(sess, alive, nil)
}

func refreshCodexContextSnapshotOnReadTimed(sess *db.SessionRow, alive bool, record func(codexTelemetryTiming)) map[string]struct{} {
	if sess == nil || !alive || sess.Harness != harness.CodexName || sess.ID == "" || sess.ConvID == "" {
		return nil
	}
	started := time.Now()
	timing := codexTelemetryTiming{}
	defer func() {
		timing.total = time.Since(started)
		if record != nil {
			record(timing)
		}
	}()
	claimStarted := time.Now()
	cached, refresh := claimCodexContextRefresh(sess.ID, started)
	timing.claim = time.Since(claimStarted)
	if !refresh {
		return cached.interruptedSubagents
	}
	completed := false
	defer func() {
		if !completed {
			releaseCodexRuntimeRefresh(sess.ID)
		}
	}()
	checkpointLoadedThisRefresh := false
	if !cached.checkpointLoaded {
		loadStarted := time.Now()
		checkpoint, err := db.LoadCodexTelemetryCheckpoint(sess.ID)
		if err != nil {
			slog.Warn("codex-telemetry: failed to load durable follower checkpoint",
				"session_id", sess.ID, "error", err, "module", "agentd")
		} else if checkpoint != nil && len(checkpoint.Data) > 0 {
			if err := cached.follower.RestoreCheckpoint(checkpoint.Data); err != nil {
				slog.Warn("codex-telemetry: discarded invalid durable follower checkpoint",
					"session_id", sess.ID, "error", err, "module", "agentd")
				if deleteErr := db.DeleteCodexTelemetryCheckpoint(sess.ID); deleteErr != nil {
					slog.Warn("codex-telemetry: failed to delete invalid durable follower checkpoint",
						"session_id", sess.ID, "error", deleteErr, "module", "agentd")
				}
			} else {
				cached.checkpointData = string(checkpoint.Data)
				cached.checkpointFailures = checkpoint.FailureCount
				cached.checkpointPersistedAt = started
			}
		}
		cached.checkpointLoaded = true
		cacheCodexCheckpointLoad(
			sess.ID,
			cached.checkpointData,
			cached.checkpointFailures,
			cached.checkpointPersistedAt,
		)
		checkpointLoadedThisRefresh = true
		timing.checkpointLoad = time.Since(loadStarted)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("codex-telemetry: cannot resolve home for read-through refresh",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return cached.interruptedSubagents
	}
	rolloutStarted := time.Now()
	snap, err := cached.follower.RuntimeTelemetry(home, sess.ConvID)
	timing.rolloutRead = time.Since(rolloutStarted)
	if err != nil {
		slog.Warn("codex-telemetry: failed read-through refresh",
			"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
		checkpointWriteStarted := time.Now()
		recordCodexCheckpointFailure(sess.ID, cached.checkpointData)
		timing.checkpointWrite = time.Since(checkpointWriteStarted)
		return cached.interruptedSubagents
	}
	checkpointData := cached.checkpointData
	checkpointFailures := cached.checkpointFailures
	checkpointPersistedAt := cached.checkpointPersistedAt
	if cached.sessionConvID != "" &&
		(cached.sessionConvID != sess.ConvID || !cached.sessionCreatedAt.Equal(sess.CreatedAt)) {
		// Session pruning cascades to its durable follower checkpoint. A later
		// resume can recreate the same session ID and conversation ID while the
		// daemon's in-memory follower survives, so make its current checkpoint
		// look dirty and repopulate the new row.
		checkpointData = ""
		checkpointFailures = 0
		checkpointPersistedAt = time.Time{}
	}
	checkpointDue := checkpointLoadedThisRefresh || checkpointData == "" || checkpointFailures > 0 ||
		checkpointPersistedAt.IsZero() || started.Sub(checkpointPersistedAt) >= codexCheckpointPersistMinInterval
	if checkpointDue {
		checkpointEncodeStarted := time.Now()
		if checkpoint, ok, checkpointErr := cached.follower.Checkpoint(); checkpointErr != nil {
			timing.checkpointEncode = time.Since(checkpointEncodeStarted)
			slog.Warn("codex-telemetry: failed to encode durable follower checkpoint",
				"session_id", sess.ID, "error", checkpointErr, "module", "agentd")
			if errors.Is(checkpointErr, harness.ErrCodexTelemetryCheckpointTooLarge) && checkpointData != "" {
				checkpointWriteStarted := time.Now()
				if deleteErr := db.DeleteCodexTelemetryCheckpoint(sess.ID); deleteErr != nil {
					slog.Warn("codex-telemetry: failed to delete oversized durable follower checkpoint",
						"session_id", sess.ID, "error", deleteErr, "module", "agentd")
				} else {
					checkpointData = ""
					checkpointFailures = 0
				}
				timing.checkpointWrite += time.Since(checkpointWriteStarted)
			}
		} else if ok && (string(checkpoint) != checkpointData || checkpointFailures > 0) {
			timing.checkpointEncode = time.Since(checkpointEncodeStarted)
			checkpointWriteStarted := time.Now()
			if saveErr := db.SaveCodexTelemetryCheckpoint(sess.ID, checkpoint); saveErr != nil {
				slog.Warn("codex-telemetry: failed to persist durable follower checkpoint",
					"session_id", sess.ID, "error", saveErr, "module", "agentd")
			} else {
				checkpointData = string(checkpoint)
				checkpointFailures = 0
				checkpointPersistedAt = time.Now()
			}
			timing.checkpointWrite += time.Since(checkpointWriteStarted)
		} else if !ok && checkpointData != "" {
			timing.checkpointEncode = time.Since(checkpointEncodeStarted)
			// Archived/missing/replaced paths may leave no tail-able cursor. Do
			// not keep retrying an obsolete checkpoint on every daemon restart.
			checkpointWriteStarted := time.Now()
			if deleteErr := db.DeleteCodexTelemetryCheckpoint(sess.ID); deleteErr != nil {
				slog.Warn("codex-telemetry: failed to delete obsolete durable follower checkpoint",
					"session_id", sess.ID, "error", deleteErr, "module", "agentd")
			} else {
				checkpointData = ""
				checkpointFailures = 0
			}
			timing.checkpointWrite += time.Since(checkpointWriteStarted)
		} else {
			timing.checkpointEncode = time.Since(checkpointEncodeStarted)
			if ok && checkpointData != "" {
				// The durable bytes already describe the current follower. Treat
				// the comparison as a fresh persistence check so an idle rollout
				// is not re-encoded on every later dashboard poll.
				checkpointPersistedAt = started
			}
		}
	}

	persistedConvID := cached.persistedConvID
	persistedCreatedAt := cached.persistedCreatedAt
	persistedContext := cached.persistedContext
	persistedHasContext := cached.persistedHasContext
	persistedReset := cached.persistedReset
	sameSessionGeneration := persistedConvID == sess.ConvID && persistedCreatedAt.Equal(sess.CreatedAt)
	if snap.ContextReset {
		if !sameSessionGeneration || !persistedReset {
			contextWriteStarted := time.Now()
			if err := db.ResetCompact(sess.ID); err != nil {
				slog.Warn("codex-telemetry: failed to persist compaction reset",
					"session_id", sess.ID, "error", err, "module", "agentd")
			} else {
				persistedConvID = sess.ConvID
				persistedCreatedAt = sess.CreatedAt
				persistedContext = harness.ContextTelemetry{}
				persistedHasContext = false
				persistedReset = true
			}
			timing.contextWrite += time.Since(contextWriteStarted)
		}
	} else if snap.HasContext &&
		(!sameSessionGeneration || persistedReset || !persistedHasContext || persistedContext != snap.Context) {
		ctx := snap.Context
		contextWriteStarted := time.Now()
		if err := db.UpdateContextSnapshot(sess.ID, ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize); err != nil {
			slog.Warn("codex-telemetry: failed to persist read-through snapshot",
				"session_id", sess.ID, "error", err, "module", "agentd")
		} else {
			persistedConvID = sess.ConvID
			persistedCreatedAt = sess.CreatedAt
			persistedContext = ctx
			persistedHasContext = true
			persistedReset = false
		}
		timing.contextWrite += time.Since(contextWriteStarted)
	}

	cacheCodexRuntimeRefresh(
		sess.ID,
		time.Now(),
		snap.InterruptedSubagents,
		checkpointData,
		checkpointFailures,
		checkpointPersistedAt,
		sess.ConvID,
		sess.CreatedAt,
		persistedConvID,
		persistedCreatedAt,
		persistedContext,
		persistedHasContext,
		persistedReset,
	)
	completed = true
	return snap.InterruptedSubagents
}

func claimCodexContextRefresh(sessionID string, now time.Time) (codexReadThroughSnapshot, bool) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	prev := codexContextRefreshMu.last[sessionID]
	if prev.refreshing || (!prev.at.IsZero() && now.Sub(prev.at) < codexContextRefreshMinInterval) {
		return prev, false
	}
	if prev.follower == nil {
		prev.follower = &harness.CodexTelemetryFollower{}
	}
	prev.at = now
	prev.refreshing = true
	codexContextRefreshMu.last[sessionID] = prev
	return prev, true
}

func cacheCodexCheckpointLoad(
	sessionID,
	checkpointData string,
	checkpointFailures int,
	checkpointPersistedAt time.Time,
) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	prev := codexContextRefreshMu.last[sessionID]
	prev.checkpointLoaded = true
	prev.checkpointData = checkpointData
	prev.checkpointFailures = checkpointFailures
	prev.checkpointPersistedAt = checkpointPersistedAt
	codexContextRefreshMu.last[sessionID] = prev
}

func recordCodexCheckpointFailure(sessionID, checkpointData string) {
	if checkpointData == "" {
		return
	}
	failures, err := db.IncrementCodexTelemetryCheckpointFailures(sessionID)
	if err != nil {
		slog.Warn("codex-telemetry: failed to record durable checkpoint failure",
			"session_id", sessionID, "error", err, "module", "agentd")
		return
	}
	evict := failures >= codexCheckpointFailureEvictThreshold
	if evict {
		if err := db.DeleteCodexTelemetryCheckpoint(sessionID); err != nil {
			slog.Warn("codex-telemetry: failed to evict repeatedly failing durable checkpoint",
				"session_id", sessionID, "failures", failures, "error", err, "module", "agentd")
			evict = false
		} else {
			slog.Warn("codex-telemetry: evicted repeatedly failing durable checkpoint",
				"session_id", sessionID, "failures", failures, "module", "agentd")
		}
	}
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	prev := codexContextRefreshMu.last[sessionID]
	if evict {
		prev.follower = &harness.CodexTelemetryFollower{}
		prev.checkpointData = ""
		prev.checkpointFailures = 0
	} else {
		prev.checkpointFailures = failures
	}
	codexContextRefreshMu.last[sessionID] = prev
}

func cacheCodexRuntimeRefresh(
	sessionID string,
	now time.Time,
	interrupted map[string]struct{},
	checkpointData string,
	checkpointFailures int,
	checkpointPersistedAt time.Time,
	sessionConvID string,
	sessionCreatedAt time.Time,
	persistedConvID string,
	persistedCreatedAt time.Time,
	persistedContext harness.ContextTelemetry,
	persistedHasContext bool,
	persistedReset bool,
) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	prev := codexContextRefreshMu.last[sessionID]
	prev.at = now
	prev.interruptedSubagents = interrupted
	prev.checkpointLoaded = true
	prev.checkpointData = checkpointData
	prev.checkpointFailures = checkpointFailures
	prev.checkpointPersistedAt = checkpointPersistedAt
	prev.sessionConvID = sessionConvID
	prev.sessionCreatedAt = sessionCreatedAt
	prev.persistedConvID = persistedConvID
	prev.persistedCreatedAt = persistedCreatedAt
	prev.persistedContext = persistedContext
	prev.persistedHasContext = persistedHasContext
	prev.persistedReset = persistedReset
	prev.refreshing = false
	codexContextRefreshMu.last[sessionID] = prev
}

func releaseCodexRuntimeRefresh(sessionID string) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	prev := codexContextRefreshMu.last[sessionID]
	prev.refreshing = false
	codexContextRefreshMu.last[sessionID] = prev
}

func sessionRowAliveIn(sess *db.SessionRow, aliveSet map[string]struct{}) bool {
	if sess == nil || sess.TmuxSession == "" {
		return false
	}
	_, ok := aliveSet[sess.TmuxSession]
	return ok
}
