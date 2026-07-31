package agentd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
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
	// checkpoint fsyncs out of the poll's median path; graceful shutdown also
	// flushes any newer in-memory cursor.
	codexCheckpointPersistMinInterval    = 30 * time.Second
	codexCheckpointFailureEvictThreshold = 3
)

var codexContextRefreshMu struct {
	sync.Mutex
	last     map[string]codexReadThroughSnapshot
	stopping bool
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
	runtimeContext        harness.ContextTelemetry
	runtimeHasContext     bool
	runtimeReset          bool
	persistedConvID       string
	persistedCreatedAt    time.Time
	persistedContext      harness.ContextTelemetry
	persistedHasContext   bool
	persistedReset        bool
	persistedEffort       string
	persistedUsageAt      time.Time
	persistedVirtualCost  float64
	persistedCostAuth     bool
	persistedCostHistory  []harness.CodexTokenCostDailySnapshot
}

type codexContextRefreshResult struct {
	interruptedSubagents map[string]struct{}
	context              *harness.ContextTelemetry
	contextReset         bool
}

func codexCostHistoriesEqual(a, b []harness.CodexTokenCostDailySnapshot) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func codexContextRefreshResultFromCache(
	sess *db.SessionRow,
	cached codexReadThroughSnapshot,
) codexContextRefreshResult {
	result := codexContextRefreshResult{
		interruptedSubagents: cached.interruptedSubagents,
	}
	if cached.sessionConvID != sess.ConvID || !cached.sessionCreatedAt.Equal(sess.CreatedAt) {
		return result
	}
	result.contextReset = cached.runtimeReset
	if cached.runtimeHasContext && !cached.runtimeReset {
		context := cached.runtimeContext
		result.context = &context
	}
	return result
}

type codexContextPersistenceState struct {
	convID     string
	createdAt  time.Time
	context    harness.ContextTelemetry
	hasContext bool
	reset      bool
}

type pendingCodexContextPersistence struct {
	sessionID        string
	operationIndex   int
	invalidateOnNoop bool
	before           codexContextPersistenceState
	after            codexContextPersistenceState
}

// codexContextWriteBatch is lazy: an idle snapshot opens no write
// transaction. Once one Codex row needs persistence, later rows share the same
// transaction and the snapshot pays one WAL durability commit in flush.
type codexContextWriteBatch struct {
	dbBatch *db.ContextSnapshotWriteBatch
	pending []pendingCodexContextPersistence
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
	contextReset     contextWriteTiming
	contextFast      contextWriteTiming
	contextProject   contextWriteTiming
	contextBatch     contextWriteTiming
}

type contextWriteTiming struct {
	total      time.Duration
	open       time.Duration
	begin      time.Duration
	update     time.Duration
	projection time.Duration
	commit     time.Duration
	execCommit time.Duration
	result     time.Duration
}

func contextWriteTimingFromDB(t db.ContextSnapshotWriteTiming) contextWriteTiming {
	return contextWriteTiming{
		total:      t.Total,
		open:       t.Open,
		begin:      t.Begin,
		update:     t.Update,
		projection: t.Projection,
		commit:     t.Commit,
		execCommit: t.ExecCommit,
		result:     t.Result,
	}
}

func (t contextWriteTiming) add(other contextWriteTiming) contextWriteTiming {
	t.total += other.total
	t.open += other.open
	t.begin += other.begin
	t.update += other.update
	t.projection += other.projection
	t.commit += other.commit
	t.execCommit += other.execCommit
	t.result += other.result
	return t
}

func contextWriteOther(total time.Duration, accounted ...time.Duration) time.Duration {
	for _, d := range accounted {
		total -= d
	}
	if total < 0 {
		return 0
	}
	return total
}

func (t contextWriteTiming) fastPerfPhase() perfPhase {
	children := []perfPhase{{Name: "open", Ms: durMs(t.open)}}
	if t.update > 0 {
		children = append(children, perfPhase{Name: "exec", Ms: durMs(t.update)})
	}
	if t.execCommit > 0 {
		children = append(children, perfPhase{Name: "exec_commit", Ms: durMs(t.execCommit)})
	}
	children = append(children,
		perfPhase{Name: "result", Ms: durMs(t.result)},
		perfPhase{Name: "other", Ms: durMs(contextWriteOther(t.total, t.open, t.update, t.execCommit, t.result))},
	)
	return perfPhase{
		Name: "fast_update", Ms: durMs(t.total), Children: children,
	}
}

func (t contextWriteTiming) projectPerfPhase() perfPhase {
	return perfPhase{
		Name: "full_projection",
		Ms:   durMs(t.total),
		Children: []perfPhase{
			{Name: "open", Ms: durMs(t.open)},
			{Name: "begin", Ms: durMs(t.begin)},
			{Name: "session_update", Ms: durMs(t.update)},
			{Name: "profile_projection", Ms: durMs(t.projection)},
			{Name: "commit", Ms: durMs(t.commit)},
			{Name: "other", Ms: durMs(contextWriteOther(
				t.total, t.open, t.begin, t.update, t.projection, t.commit,
			))},
		},
	}
}

func (t contextWriteTiming) resetPerfPhase() perfPhase {
	children := []perfPhase{{Name: "open", Ms: durMs(t.open)}}
	if t.update > 0 {
		children = append(children, perfPhase{Name: "exec", Ms: durMs(t.update)})
	}
	if t.execCommit > 0 {
		children = append(children, perfPhase{Name: "exec_commit", Ms: durMs(t.execCommit)})
	}
	children = append(children, perfPhase{
		Name: "other", Ms: durMs(contextWriteOther(t.total, t.open, t.update, t.execCommit)),
	})
	return perfPhase{
		Name: "reset", Ms: durMs(t.total), Children: children,
	}
}

func (t contextWriteTiming) batchPerfPhase() perfPhase {
	return perfPhase{
		Name: "batch_transaction",
		Ms:   durMs(t.total),
		Children: []perfPhase{
			{Name: "open", Ms: durMs(t.open)},
			{Name: "begin", Ms: durMs(t.begin)},
			{Name: "commit", Ms: durMs(t.commit)},
			{Name: "other", Ms: durMs(contextWriteOther(t.total, t.open, t.begin, t.commit))},
		},
	}
}

func (t codexTelemetryTiming) add(other codexTelemetryTiming) codexTelemetryTiming {
	t.total += other.total
	t.claim += other.claim
	t.checkpointLoad += other.checkpointLoad
	t.rolloutRead += other.rolloutRead
	t.checkpointEncode += other.checkpointEncode
	t.checkpointWrite += other.checkpointWrite
	t.contextWrite += other.contextWrite
	t.contextReset = t.contextReset.add(other.contextReset)
	t.contextFast = t.contextFast.add(other.contextFast)
	t.contextProject = t.contextProject.add(other.contextProject)
	t.contextBatch = t.contextBatch.add(other.contextBatch)
	return t
}

func (t codexTelemetryTiming) perfPhases() []perfPhase {
	accounted := t.claim + t.checkpointLoad + t.rolloutRead + t.checkpointEncode +
		t.checkpointWrite + t.contextWrite
	other := t.total - accounted
	if other < 0 {
		other = 0
	}
	contextChildren := make([]perfPhase, 0, 4)
	if t.contextReset.total > 0 {
		contextChildren = append(contextChildren, t.contextReset.resetPerfPhase())
	}
	if t.contextFast.total > 0 {
		contextChildren = append(contextChildren, t.contextFast.fastPerfPhase())
	}
	if t.contextProject.total > 0 {
		contextChildren = append(contextChildren, t.contextProject.projectPerfPhase())
	}
	if t.contextBatch.total > 0 {
		contextChildren = append(contextChildren, t.contextBatch.batchPerfPhase())
	}
	contextAccounted := t.contextReset.total + t.contextFast.total + t.contextProject.total + t.contextBatch.total
	if contextOther := contextWriteOther(t.contextWrite, contextAccounted); contextOther > 0 {
		contextChildren = append(contextChildren, perfPhase{
			Name: "other",
			Ms:   durMs(contextOther),
		})
	}
	return []perfPhase{
		{Name: "claim", Ms: durMs(t.claim)},
		{Name: "checkpoint_load", Ms: durMs(t.checkpointLoad)},
		{Name: "rollout_read", Ms: durMs(t.rolloutRead)},
		{Name: "checkpoint_encode", Ms: durMs(t.checkpointEncode)},
		{Name: "checkpoint_write", Ms: durMs(t.checkpointWrite)},
		{Name: "context_write", Ms: durMs(t.contextWrite), Children: contextChildren},
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
	return refreshCodexContextSnapshotOnReadBatched(sess, alive, nil, record).interruptedSubagents
}

func refreshCodexContextSnapshotOnReadBatched(
	sess *db.SessionRow,
	alive bool,
	batch *codexContextWriteBatch,
	record func(codexTelemetryTiming),
) codexContextRefreshResult {
	if sess == nil || !alive || sess.Harness != harness.CodexName || sess.ID == "" || sess.ConvID == "" {
		return codexContextRefreshResult{}
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
		return codexContextRefreshResultFromCache(sess, cached)
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
		return codexContextRefreshResultFromCache(sess, cached)
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
		return codexContextRefreshResultFromCache(sess, cached)
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
	persistedVirtualCost := cached.persistedVirtualCost
	persistedEffort := cached.persistedEffort
	persistedCostAuth := cached.persistedCostAuth
	persistedCostHistory := cached.persistedCostHistory
	if cached.sessionConvID != "" &&
		(cached.sessionConvID != sess.ConvID || !cached.sessionCreatedAt.Equal(sess.CreatedAt)) {
		persistedVirtualCost = 0
		persistedEffort = ""
		persistedCostAuth = false
		persistedCostHistory = nil
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
			persisted, saveErr := db.SaveCodexTelemetryCheckpointForSessionGenerationContext(
				context.Background(), sess.ID, sess.ConvID, sess.CreatedAt, checkpoint,
			)
			if saveErr != nil {
				slog.Warn("codex-telemetry: failed to persist durable follower checkpoint",
					"session_id", sess.ID, "error", saveErr, "module", "agentd")
			} else if persisted {
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
	currentVirtualCost := 0.0
	if snap.HasCost {
		currentVirtualCost = snap.Cost.CostUSD
	}
	if snap.CostAuthoritative && (!persistedCostAuth || currentVirtualCost != persistedVirtualCost ||
		!codexCostHistoriesEqual(snap.CostHistory, persistedCostHistory)) {
		daily := make([]db.VirtualCostDailySnapshot, 0, len(snap.CostHistory))
		for _, item := range snap.CostHistory {
			daily = append(daily, db.VirtualCostDailySnapshot{
				Day: item.Day, CostUSD: item.CostUSD, UpdatedAt: item.Observed, Model: item.Model,
			})
		}
		persisted, err := db.ReplaceSessionVirtualCostHistoryForGeneration(
			sess.ID, sess.ConvID, sess.CreatedAt, currentVirtualCost, daily,
		)
		if err != nil {
			slog.Warn("codex-cost: failed to persist authoritative virtual cost history",
				"session_id", sess.ID, "model", snap.Cost.Model, "error", err, "module", "agentd")
		} else if persisted {
			persistedVirtualCost = currentVirtualCost
			persistedCostAuth = true
			persistedCostHistory = append([]harness.CodexTokenCostDailySnapshot(nil), snap.CostHistory...)
		}
	}
	if snap.HasEffort && snap.Effort != persistedEffort {
		persisted, err := db.UpdateSessionEffortForGeneration(
			sess.ID, sess.ConvID, sess.CreatedAt, snap.Effort,
		)
		if err != nil {
			slog.Warn("codex-telemetry: failed to persist incremental effort",
				"session_id", sess.ID, "effort", snap.Effort, "error", err, "module", "agentd")
		} else if persisted {
			persistedEffort = snap.Effort
		}
	}
	persistedUsageAt := cached.persistedUsageAt
	if snap.Usage != nil && !snap.Usage.Observed.IsZero() && snap.Usage.Observed.After(persistedUsageAt) {
		saveCodexUsageSnapshot(snap.Usage, "telemetry-follower")
		persistedUsageAt = snap.Usage.Observed
	}

	persistedConvID := cached.persistedConvID
	persistedCreatedAt := cached.persistedCreatedAt
	persistedContext := cached.persistedContext
	persistedHasContext := cached.persistedHasContext
	persistedReset := cached.persistedReset
	sameSessionGeneration := persistedConvID == sess.ConvID && persistedCreatedAt.Equal(sess.CreatedAt)
	beforePersistence := codexContextPersistenceState{
		convID: persistedConvID, createdAt: persistedCreatedAt, context: persistedContext,
		hasContext: persistedHasContext, reset: persistedReset,
	}
	contextPersistenceDeferred := false
	deferredOperationIndex := -1
	deferredFastUpdate := false
	if snap.ContextReset {
		if !sameSessionGeneration || !persistedReset {
			contextWriteStarted := time.Now()
			var err error
			if batch == nil {
				err = db.ResetCompactTimed(sess.ID, func(detail db.ContextSnapshotWriteTiming) {
					timing.contextReset = contextWriteTimingFromDB(detail)
				})
			} else {
				deferredOperationIndex, err = batch.resetCompact(sess.ID, sess.ConvID, sess.CreatedAt)
			}
			if err != nil {
				slog.Warn("codex-telemetry: failed to persist compaction reset",
					"session_id", sess.ID, "error", err, "module", "agentd")
			} else {
				persistedConvID = sess.ConvID
				persistedCreatedAt = sess.CreatedAt
				persistedContext = harness.ContextTelemetry{}
				persistedHasContext = false
				persistedReset = true
				if batch != nil {
					contextPersistenceDeferred = true
					batch.pending = append(batch.pending, pendingCodexContextPersistence{
						sessionID:      sess.ID,
						operationIndex: deferredOperationIndex,
						before:         beforePersistence,
						after: codexContextPersistenceState{
							convID: sess.ConvID, createdAt: sess.CreatedAt, reset: true,
						},
					})
				}
			}
			timing.contextWrite += time.Since(contextWriteStarted)
		}
	} else if snap.HasContext &&
		(!sameSessionGeneration || persistedReset || !persistedHasContext || persistedContext != snap.Context) {
		ctx := snap.Context
		contextWriteStarted := time.Now()
		contextPersisted := false
		var err error
		if sameSessionGeneration && !persistedReset && persistedHasContext &&
			persistedContext.WindowSize == ctx.WindowSize {
			if batch == nil {
				contextPersisted, err = db.UpdateContextSnapshotIfWindowUnchangedTimed(
					sess.ID, sess.ConvID, sess.CreatedAt,
					ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize,
					func(detail db.ContextSnapshotWriteTiming) {
						timing.contextFast = contextWriteTimingFromDB(detail)
					},
				)
			} else {
				deferredFastUpdate = true
				deferredOperationIndex, err = batch.updateIfWindowUnchanged(
					sess.ID, sess.ConvID, sess.CreatedAt,
					ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize,
				)
				contextPersisted = err == nil
			}
		} else {
			if batch == nil {
				err = db.UpdateContextSnapshotTimed(
					sess.ID, ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize,
					func(detail db.ContextSnapshotWriteTiming) {
						timing.contextProject = contextWriteTimingFromDB(detail)
					},
				)
			} else {
				deferredOperationIndex, err = batch.updateContextSnapshot(
					sess.ID, sess.ConvID, sess.CreatedAt,
					ctx.Pct, ctx.TokensInput, ctx.TokensOutput, ctx.WindowSize,
				)
			}
			contextPersisted = err == nil
		}
		if err != nil {
			slog.Warn("codex-telemetry: failed to persist read-through snapshot",
				"session_id", sess.ID, "error", err, "module", "agentd")
		} else if contextPersisted {
			persistedConvID = sess.ConvID
			persistedCreatedAt = sess.CreatedAt
			persistedContext = ctx
			persistedHasContext = true
			persistedReset = false
			if batch != nil {
				contextPersistenceDeferred = true
				batch.pending = append(batch.pending, pendingCodexContextPersistence{
					sessionID:        sess.ID,
					operationIndex:   deferredOperationIndex,
					invalidateOnNoop: deferredFastUpdate,
					before:           beforePersistence,
					after: codexContextPersistenceState{
						convID: sess.ConvID, createdAt: sess.CreatedAt, context: ctx, hasContext: true,
					},
				})
			}
		} else {
			// The row generation or window changed after the dashboard preload.
			// Force the next poll through the full projecting path.
			persistedHasContext = false
		}
		timing.contextWrite += time.Since(contextWriteStarted)
	}

	cachePersistedConvID := persistedConvID
	cachePersistedCreatedAt := persistedCreatedAt
	cachePersistedContext := persistedContext
	cachePersistedHasContext := persistedHasContext
	cachePersistedReset := persistedReset
	if contextPersistenceDeferred {
		cachePersistedConvID = beforePersistence.convID
		cachePersistedCreatedAt = beforePersistence.createdAt
		cachePersistedContext = beforePersistence.context
		cachePersistedHasContext = beforePersistence.hasContext
		cachePersistedReset = beforePersistence.reset
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
		snap.Context,
		snap.HasContext,
		snap.ContextReset,
		contextPersistenceDeferred,
		cachePersistedConvID,
		cachePersistedCreatedAt,
		cachePersistedContext,
		cachePersistedHasContext,
		cachePersistedReset,
		persistedEffort,
		persistedUsageAt,
		persistedVirtualCost,
		persistedCostAuth,
		persistedCostHistory,
	)
	completed = true
	result := codexContextRefreshResult{
		interruptedSubagents: snap.InterruptedSubagents,
		contextReset:         snap.ContextReset,
	}
	if snap.HasContext && !snap.ContextReset {
		ctx := snap.Context
		result.context = &ctx
	}
	return result
}

func (b *codexContextWriteBatch) ensureDBBatch() error {
	if b.dbBatch != nil {
		return nil
	}
	b.dbBatch = db.NewContextSnapshotWriteBatch()
	return nil
}

func (b *codexContextWriteBatch) resetCompact(
	sessionID, convID string,
	createdAt time.Time,
) (int, error) {
	if err := b.ensureDBBatch(); err != nil {
		return 0, err
	}
	return b.dbBatch.ResetCompact(sessionID, convID, createdAt), nil
}

func (b *codexContextWriteBatch) updateIfWindowUnchanged(
	sessionID, convID string,
	createdAt time.Time,
	pct float64,
	tokensInput, tokensOutput, windowSize int64,
) (int, error) {
	if err := b.ensureDBBatch(); err != nil {
		return 0, err
	}
	return b.dbBatch.UpdateContextSnapshotIfWindowUnchanged(
		sessionID, convID, createdAt, pct, tokensInput, tokensOutput, windowSize,
	), nil
}

func (b *codexContextWriteBatch) updateContextSnapshot(
	sessionID, convID string,
	createdAt time.Time,
	pct float64,
	tokensInput, tokensOutput, windowSize int64,
) (int, error) {
	if err := b.ensureDBBatch(); err != nil {
		return 0, err
	}
	return b.dbBatch.UpdateContextSnapshot(
		sessionID, convID, createdAt, pct, tokensInput, tokensOutput, windowSize,
	), nil
}

// flush commits all context updates discovered by one dashboard snapshot and
// reports both the per-operation work and the one shared transaction.
func (b *codexContextWriteBatch) flush() (codexTelemetryTiming, error) {
	if b == nil || b.dbBatch == nil {
		return codexTelemetryTiming{}, nil
	}
	defer func() {
		released := make(map[string]struct{}, len(b.pending))
		for _, pending := range b.pending {
			if _, ok := released[pending.sessionID]; ok {
				continue
			}
			released[pending.sessionID] = struct{}{}
			releaseCodexRuntimeRefresh(pending.sessionID)
		}
	}()
	result, err := b.dbBatch.Commit()
	timing := codexTelemetryTiming{
		contextReset:   contextWriteTimingFromDB(result.Reset),
		contextFast:    contextWriteTimingFromDB(result.Fast),
		contextProject: contextWriteTimingFromDB(result.Project),
		contextBatch:   contextWriteTimingFromDB(result.Transaction),
	}
	timing.contextBatch.total = timing.contextBatch.open + timing.contextBatch.begin + timing.contextBatch.commit
	timing.contextWrite = timing.contextReset.total + timing.contextFast.total +
		timing.contextProject.total + timing.contextBatch.total
	timing.total = timing.contextWrite
	if err == nil {
		for _, pending := range b.pending {
			if pending.operationIndex >= 0 &&
				pending.operationIndex < len(result.Applied) &&
				result.Applied[pending.operationIndex] {
				cacheCodexContextPersistence(pending)
			} else if pending.invalidateOnNoop {
				pending.after = pending.before
				pending.after.hasContext = false
				cacheCodexContextPersistence(pending)
			}
		}
	}
	return timing, errors.Join(append([]error{err}, result.OperationErrors...)...)
}

func cacheCodexContextPersistence(pending pendingCodexContextPersistence) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	state := codexContextRefreshMu.last[pending.sessionID]
	current := codexContextPersistenceState{
		convID: state.persistedConvID, createdAt: state.persistedCreatedAt,
		context: state.persistedContext, hasContext: state.persistedHasContext,
		reset: state.persistedReset,
	}
	if current != pending.before ||
		state.sessionConvID != pending.after.convID ||
		!state.sessionCreatedAt.Equal(pending.after.createdAt) {
		return
	}
	state.persistedConvID = pending.after.convID
	state.persistedCreatedAt = pending.after.createdAt
	state.persistedContext = pending.after.context
	state.persistedHasContext = pending.after.hasContext
	state.persistedReset = pending.after.reset
	codexContextRefreshMu.last[pending.sessionID] = state
}

func claimCodexContextRefresh(sessionID string, now time.Time) (codexReadThroughSnapshot, bool) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	prev := codexContextRefreshMu.last[sessionID]
	if codexContextRefreshMu.stopping || prev.refreshing ||
		(!prev.at.IsZero() && now.Sub(prev.at) < codexContextRefreshMinInterval) {
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
	runtimeContext harness.ContextTelemetry,
	runtimeHasContext bool,
	runtimeReset bool,
	keepRefreshing bool,
	persistedConvID string,
	persistedCreatedAt time.Time,
	persistedContext harness.ContextTelemetry,
	persistedHasContext bool,
	persistedReset bool,
	persistedEffort string,
	persistedUsageAt time.Time,
	persistedVirtualCost float64,
	persistedCostAuth bool,
	persistedCostHistory []harness.CodexTokenCostDailySnapshot,
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
	prev.runtimeContext = runtimeContext
	prev.runtimeHasContext = runtimeHasContext
	prev.runtimeReset = runtimeReset
	prev.persistedConvID = persistedConvID
	prev.persistedCreatedAt = persistedCreatedAt
	prev.persistedContext = persistedContext
	prev.persistedHasContext = persistedHasContext
	prev.persistedReset = persistedReset
	prev.persistedEffort = persistedEffort
	prev.persistedUsageAt = persistedUsageAt
	prev.persistedVirtualCost = persistedVirtualCost
	prev.persistedCostAuth = persistedCostAuth
	prev.persistedCostHistory = append([]harness.CodexTokenCostDailySnapshot(nil), persistedCostHistory...)
	prev.refreshing = keepRefreshing
	codexContextRefreshMu.last[sessionID] = prev
}

func releaseCodexRuntimeRefresh(sessionID string) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	prev := codexContextRefreshMu.last[sessionID]
	prev.refreshing = false
	codexContextRefreshMu.last[sessionID] = prev
}

// stopCodexContextRefreshes prevents requests already admitted by an HTTP
// server from starting new follower work while graceful shutdown drains.
func stopCodexContextRefreshes() {
	codexContextRefreshMu.Lock()
	codexContextRefreshMu.stopping = true
	codexContextRefreshMu.Unlock()
}

// flushCodexTelemetryCheckpoints persists follower state deferred by the
// dashboard-poll write throttle. It waits for refreshes that were already in
// flight when shutdown stopped new claims. Hard termination still falls back
// to the most recent periodic checkpoint.
func flushCodexTelemetryCheckpoints(ctx context.Context) (int, error) {
	type candidate struct {
		sessionID          string
		follower           *harness.CodexTelemetryFollower
		checkpointData     string
		checkpointFailures int
		sessionConvID      string
		sessionCreatedAt   time.Time
	}

	processed := make(map[string]struct{})
	saved := 0
	var errs []error
	for {
		codexContextRefreshMu.Lock()
		refreshing := false
		candidates := make([]candidate, 0, len(codexContextRefreshMu.last))
		for sessionID, state := range codexContextRefreshMu.last {
			if _, ok := processed[sessionID]; ok {
				continue
			}
			if state.refreshing {
				refreshing = true
				continue
			}
			processed[sessionID] = struct{}{}
			if state.follower == nil || state.sessionConvID == "" {
				continue
			}
			candidates = append(candidates, candidate{
				sessionID:          sessionID,
				follower:           state.follower,
				checkpointData:     state.checkpointData,
				checkpointFailures: state.checkpointFailures,
				sessionConvID:      state.sessionConvID,
				sessionCreatedAt:   state.sessionCreatedAt,
			})
		}
		codexContextRefreshMu.Unlock()
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].sessionID < candidates[j].sessionID
		})

		for _, candidate := range candidates {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				return saved, errors.Join(errs...)
			}
			checkpoint, ok, err := candidate.follower.Checkpoint()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: encode checkpoint: %w", candidate.sessionID, err))
				continue
			}
			if !ok || (string(checkpoint) == candidate.checkpointData && candidate.checkpointFailures == 0) {
				continue
			}
			persisted, err := db.SaveCodexTelemetryCheckpointForSessionGenerationContext(
				ctx,
				candidate.sessionID,
				candidate.sessionConvID,
				candidate.sessionCreatedAt,
				checkpoint,
			)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: save checkpoint: %w", candidate.sessionID, err))
				continue
			}
			if !persisted {
				// Session deletion cascades its checkpoint. Do not attach an old
				// in-memory follower to a later row that reused the same ID.
				continue
			}
			codexContextRefreshMu.Lock()
			state := codexContextRefreshMu.last[candidate.sessionID]
			if state.follower == candidate.follower &&
				state.checkpointData == candidate.checkpointData &&
				state.sessionConvID == candidate.sessionConvID &&
				state.sessionCreatedAt.Equal(candidate.sessionCreatedAt) {
				state.checkpointData = string(checkpoint)
				state.checkpointFailures = 0
				state.checkpointPersistedAt = time.Now()
				codexContextRefreshMu.last[candidate.sessionID] = state
			}
			codexContextRefreshMu.Unlock()
			saved++
		}
		if !refreshing {
			return saved, errors.Join(errs...)
		}

		select {
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return saved, errors.Join(errs...)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func sessionRowAliveIn(sess *db.SessionRow, aliveSet map[string]struct{}) bool {
	if sess == nil || sess.TmuxSession == "" {
		return false
	}
	_, ok := aliveSet[sess.TmuxSession]
	return ok
}
