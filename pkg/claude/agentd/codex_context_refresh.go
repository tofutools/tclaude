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
	codexContextRefreshMinInterval       = time.Second
	codexCheckpointFailureEvictThreshold = 3
)

var codexContextRefreshMu struct {
	sync.Mutex
	last map[string]codexReadThroughSnapshot
}

type codexReadThroughSnapshot struct {
	at                   time.Time
	interruptedSubagents map[string]struct{}
	follower             *harness.CodexTelemetryFollower
	refreshing           bool
	checkpointLoaded     bool
	checkpointData       string
	checkpointFailures   int
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

func refreshCodexContextSnapshotOnReadTimed(sess *db.SessionRow, alive bool, record func(time.Duration)) map[string]struct{} {
	if sess == nil || !alive || sess.Harness != harness.CodexName || sess.ID == "" || sess.ConvID == "" {
		return nil
	}
	started := time.Now()
	defer func() {
		if record != nil {
			record(time.Since(started))
		}
	}()
	cached, refresh := claimCodexContextRefresh(sess.ID, started)
	if !refresh {
		return cached.interruptedSubagents
	}
	completed := false
	defer func() {
		if !completed {
			releaseCodexRuntimeRefresh(sess.ID)
		}
	}()
	if !cached.checkpointLoaded {
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
			}
		}
		cached.checkpointLoaded = true
		cacheCodexCheckpointLoad(sess.ID, cached.checkpointData, cached.checkpointFailures)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("codex-telemetry: cannot resolve home for read-through refresh",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return cached.interruptedSubagents
	}
	snap, err := cached.follower.RuntimeTelemetry(home, sess.ConvID)
	if err != nil {
		slog.Warn("codex-telemetry: failed read-through refresh",
			"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
		recordCodexCheckpointFailure(sess.ID, cached.checkpointData)
		return cached.interruptedSubagents
	}
	checkpointData := cached.checkpointData
	checkpointFailures := cached.checkpointFailures
	if checkpoint, ok, checkpointErr := cached.follower.Checkpoint(); checkpointErr != nil {
		slog.Warn("codex-telemetry: failed to encode durable follower checkpoint",
			"session_id", sess.ID, "error", checkpointErr, "module", "agentd")
		if errors.Is(checkpointErr, harness.ErrCodexTelemetryCheckpointTooLarge) && checkpointData != "" {
			if deleteErr := db.DeleteCodexTelemetryCheckpoint(sess.ID); deleteErr != nil {
				slog.Warn("codex-telemetry: failed to delete oversized durable follower checkpoint",
					"session_id", sess.ID, "error", deleteErr, "module", "agentd")
			} else {
				checkpointData = ""
				checkpointFailures = 0
			}
		}
	} else if ok && (string(checkpoint) != checkpointData || cached.checkpointFailures > 0) {
		if saveErr := db.SaveCodexTelemetryCheckpoint(sess.ID, checkpoint); saveErr != nil {
			slog.Warn("codex-telemetry: failed to persist durable follower checkpoint",
				"session_id", sess.ID, "error", saveErr, "module", "agentd")
		} else {
			checkpointData = string(checkpoint)
			checkpointFailures = 0
		}
	} else if !ok && checkpointData != "" {
		// Archived/missing/replaced paths may leave no tail-able cursor. Do
		// not keep retrying an obsolete checkpoint on every daemon restart.
		if deleteErr := db.DeleteCodexTelemetryCheckpoint(sess.ID); deleteErr != nil {
			slog.Warn("codex-telemetry: failed to delete obsolete durable follower checkpoint",
				"session_id", sess.ID, "error", deleteErr, "module", "agentd")
		} else {
			checkpointData = ""
			checkpointFailures = 0
		}
	}
	cacheCodexRuntimeRefresh(sess.ID, time.Now(), snap.InterruptedSubagents, checkpointData, checkpointFailures)
	completed = true
	if snap.ContextReset {
		if err := db.ResetCompact(sess.ID); err != nil {
			slog.Warn("codex-telemetry: failed to persist compaction reset",
				"session_id", sess.ID, "error", err, "module", "agentd")
		}
		return snap.InterruptedSubagents
	}
	if !snap.HasContext {
		return snap.InterruptedSubagents
	}
	ctx := snap.Context
	if err := db.UpdateContextSnapshot(sess.ID, ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize); err != nil {
		slog.Warn("codex-telemetry: failed to persist read-through snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
	}
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

func cacheCodexCheckpointLoad(sessionID, checkpointData string, checkpointFailures int) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	prev := codexContextRefreshMu.last[sessionID]
	prev.checkpointLoaded = true
	prev.checkpointData = checkpointData
	prev.checkpointFailures = checkpointFailures
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
