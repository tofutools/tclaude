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
	last map[string]time.Time
}

// refreshCodexContextSnapshotOnRead gives Codex the same dashboard freshness
// Claude Code gets from its command-statusline: before a live Codex row's
// snapshot is rendered, lift the latest token_count off its rollout and write
// the existing sessions.context_* columns. The write remains best-effort and
// read-through; every surface still consumes db.GetContextSnapshot.
func refreshCodexContextSnapshotOnRead(sess *db.SessionRow, alive bool) {
	if sess == nil || !alive || sess.Harness != harness.CodexName || sess.ID == "" || sess.ConvID == "" {
		return
	}
	if current, err := db.GetContextSnapshot(sess.ID); err == nil && snapshotPopulated(current) {
		if !claimCodexContextRefresh(sess.ID, time.Now()) {
			return
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		slog.Warn("codex-telemetry: cannot resolve home for read-through refresh",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	snap, ok, err := harness.CodexContextTelemetry(home, sess.ConvID)
	if err != nil {
		slog.Warn("codex-telemetry: failed read-through refresh",
			"session_id", sess.ID, "conv_id", sess.ConvID, "error", err, "module", "agentd")
		return
	}
	if !ok {
		return
	}
	if err := db.UpdateContextSnapshot(sess.ID, snap.Pct, snap.TokensInput, snap.TokensOutput, snap.WindowSize); err != nil {
		slog.Warn("codex-telemetry: failed to persist read-through snapshot",
			"session_id", sess.ID, "error", err, "module", "agentd")
		return
	}
	markCodexContextRefresh(sess.ID, time.Now())
}

func claimCodexContextRefresh(sessionID string, now time.Time) bool {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]time.Time{}
	}
	if prev, ok := codexContextRefreshMu.last[sessionID]; ok && now.Sub(prev) < codexContextRefreshMinInterval {
		return false
	}
	codexContextRefreshMu.last[sessionID] = now
	return true
}

func markCodexContextRefresh(sessionID string, now time.Time) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]time.Time{}
	}
	codexContextRefreshMu.last[sessionID] = now
}

func sessionRowAliveIn(sess *db.SessionRow, aliveSet map[string]struct{}) bool {
	if sess == nil || sess.TmuxSession == "" {
		return false
	}
	_, ok := aliveSet[sess.TmuxSession]
	return ok
}
