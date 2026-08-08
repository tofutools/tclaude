package agentd

import (
	"net/http"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// codexFastModeFromFollowerNow bypasses the dashboard's one-second presentation
// throttle while reusing the same incremental follower and cursor. It never
// reparses an unchanged prefix: RuntimeTelemetry stats the memoized rollout and
// consumes only complete records appended since the follower's last read.
func codexFastModeFromFollowerNow(sess *db.SessionRow) (fast, known bool, err error) {
	if sess == nil || sess.ID == "" || sess.ConvID == "" || sess.Harness != harness.CodexName {
		return false, false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, false, err
	}

	codexContextRefreshMu.Lock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	cached := codexContextRefreshMu.last[sess.ID]
	if cached.follower == nil {
		cached.follower = &harness.CodexTelemetryFollower{}
		codexContextRefreshMu.last[sess.ID] = cached
	}
	follower := cached.follower
	codexContextRefreshMu.Unlock()

	snap, err := follower.RuntimeTelemetry(home, sess.ConvID)
	if err != nil {
		return false, false, err
	}

	// Keep the ordinary snapshot read-through cache coherent, but do not move
	// its refresh timestamp: this focused proof does not perform the context,
	// effort, usage, cost, or checkpoint persistence work of a dashboard tick.
	codexContextRefreshMu.Lock()
	cached = codexContextRefreshMu.last[sess.ID]
	if cached.follower == follower {
		cached.sessionConvID = sess.ConvID
		cached.sessionCreatedAt = sess.CreatedAt
		cached.runtimeFastMode = snap.FastMode
		cached.runtimeHasFastMode = snap.HasFastMode
		codexContextRefreshMu.last[sess.ID] = cached
	}
	codexContextRefreshMu.Unlock()
	return snap.FastMode, snap.HasFastMode, nil
}

// dashboardDisableCodexFastModeAgent re-proves live Fast state immediately
// before sending Codex's no-argument toggle. Codex currently exposes no
// directional /fast off command, so a manual toggle in the tiny interval
// between this proof and pane delivery can still race; callers explicitly
// accept that limitation.
func dashboardDisableCodexFastModeAgent(w http.ResponseWriter, _ *http.Request, convSelector string) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	sess := aliveSessionForConv(resolved.ConvID)
	if sess == nil {
		writeError(w, http.StatusConflict, "offline", "agent has no live tmux session")
		return
	}
	h, err := harness.Resolve(sess.Harness)
	if err != nil || h.Life == nil || h.Life.FastModeCommand() == "" {
		writeError(w, http.StatusConflict, "unsupported", "agent harness does not support the Fast mode action")
		return
	}
	fast, known, err := codexFastModeFromFollowerNow(sess)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read live Codex Fast mode: "+err.Error())
		return
	}
	if !known {
		writeError(w, http.StatusConflict, "unknown", "Codex has not reported authoritative live Fast mode state")
		return
	}
	if !fast {
		writeError(w, http.StatusConflict, "already_off", "Codex Fast mode is already off")
		return
	}
	if !injectSlashCommand(resolved.ConvID, h.Life.FastModeCommand(), "", "dashboard fast-mode disable") {
		writeError(w, http.StatusConflict, "delivery_failed", "could not deliver the Fast mode toggle to the live Codex pane")
		return
	}

	// Allow the next dashboard poll to inspect the rollout immediately rather
	// than waiting out a presentation-cache interval that began before the
	// command was injected.
	codexContextRefreshMu.Lock()
	cached := codexContextRefreshMu.last[sess.ID]
	cached.at = time.Time{}
	codexContextRefreshMu.last[sess.ID] = cached
	codexContextRefreshMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":   resolved.ConvID,
		"requested": "fast_off",
	})
}
