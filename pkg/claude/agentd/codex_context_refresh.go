package agentd

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const codexContextRefreshMinInterval = time.Second

var codexContextRefreshMu struct {
	sync.Mutex
	last map[string]codexReadThroughSnapshot
}

type codexReadThroughSnapshot struct {
	at                   time.Time
	interruptedSubagents map[string]struct{}
	follower             *harness.CodexTelemetryFollower
	refreshing           bool
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
		return cached.interruptedSubagents
	}
	cacheCodexRuntimeRefresh(sess.ID, time.Now(), snap.InterruptedSubagents)
	completed = true
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

func cacheCodexRuntimeRefresh(sessionID string, now time.Time, interrupted map[string]struct{}) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	prev := codexContextRefreshMu.last[sessionID]
	prev.at = now
	prev.interruptedSubagents = interrupted
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
