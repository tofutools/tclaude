package agentd

import (
	"log/slog"
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
	fast, known = codexFastModeForSession(snap, sess)

	// Keep the ordinary snapshot read-through cache coherent, but do not move
	// its refresh timestamp: this focused proof does not perform the context,
	// effort, usage, cost, or checkpoint persistence work of a dashboard tick.
	codexContextRefreshMu.Lock()
	cached = codexContextRefreshMu.last[sess.ID]
	if cached.follower == follower {
		cached.sessionConvID = sess.ConvID
		cached.sessionCreatedAt = sess.CreatedAt
		cached.runtimeFastMode = fast
		cached.runtimeHasFastMode = known
		codexContextRefreshMu.last[sess.ID] = cached
	}
	codexContextRefreshMu.Unlock()
	if !known {
		profile, profileErr := db.AgentRelaunchProfileForConv(sess.ConvID)
		if profileErr != nil {
			return false, false, profileErr
		}
		if profile != nil && profile.FastMode != nil {
			return *profile.FastMode, true, nil
		}
	}
	return fast, known, nil
}

// dashboardSetCodexFastModeAgent re-proves live Fast state immediately before
// sending Codex's no-argument toggle. Codex currently exposes no directional
// /fast command, so a manual toggle in the tiny interval between this proof
// and pane delivery can still race; callers explicitly accept that limitation.
func dashboardSetCodexFastModeAgent(w http.ResponseWriter, _ *http.Request, convSelector string, desired bool) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	if afterResolveCodexFastModeForTest != nil {
		afterResolveCodexFastModeForTest()
	}
	launchLock := resumeLaunchLock(resolved.ConvID)
	launchLock.Lock()
	defer launchLock.Unlock()
	if resolved.AgentID != "" {
		if err := requireCurrentAgentGeneration(resolved.AgentID, resolved.ConvID); err != nil {
			writeError(w, http.StatusConflict, "stale_generation", err.Error())
			return
		}
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
		writeError(w, http.StatusConflict, "unknown", "Codex has not reported live Fast mode state and the agent has no explicit tclaude launch setting")
		return
	}
	if fast == desired {
		state := "off"
		if desired {
			state = "on"
		}
		writeError(w, http.StatusConflict, "already_"+state, "Codex Fast mode is already "+state)
		return
	}
	if !injectCodexFastModeToggle(sess, h.Life.FastModeCommand(), desired) {
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
		"requested": map[bool]string{true: "fast_on", false: "fast_off"}[desired],
	})
}

// injectCodexFastModeToggle deliberately uses the pane for both Codex drives.
// Codex 0.147's stable app-server protocol has no settings mutation for an
// already-running thread; /fast remains a TUI command even when app-server
// owns turns. Keep this exact compile-time token path separate from the generic
// lifecycle dispatcher, whose app-server branch must continue to fail closed
// for commands without typed RPC equivalents.
func injectCodexFastModeToggle(sess *db.SessionRow, command string, desired bool) bool {
	target := sess.TmuxSession + ":0.0"
	if err := injectTextAndSubmit(target, command); err != nil {
		slog.Warn("Codex Fast mode inject failed", "error", err, "tmux", sess.TmuxSession,
			"conv_id", sess.ConvID, "desired", desired)
		return false
	}
	slog.Info("Codex Fast mode toggle injected via send-keys", "tmux_session", sess.TmuxSession,
		"conv_id", sess.ConvID, "desired", desired)
	return true
}

// afterResolveCodexFastModeForTest is a deterministic seam for proving that
// stable-agent generation changes are rejected after selector resolution.
var afterResolveCodexFastModeForTest func()
