package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const (
	sandboxRestartUnlock  = "unlock"
	sandboxRestartRestore = "restore"
)

// sandboxRestartIdleFailure is the authoritative preflight for the temporary
// sandbox transition. It intentionally reads one DB snapshot rather than
// pretending to make the later stop race-free: the operator asked for a basic
// idle check, and the actual restart still uses the normal lifecycle path.
func sandboxRestartIdleFailure(sess *db.SessionRow, now time.Time) string {
	if sess == nil {
		return "the agent must be online before its sandbox mode can be restarted"
	}
	subagents := db.ParseSubagentSet(sess.SubagentsJSON).LiveCount(now)
	backgroundShells := db.ParseBgShellSet(sess.BgShellsJSON).LiveCount(now)
	if sess.Status == session.StatusIdle && subagents == 0 && backgroundShells == 0 {
		return ""
	}
	var blockers []string
	if sess.Status != session.StatusIdle {
		status := strings.TrimSpace(sess.Status)
		if status == "" {
			status = "unknown"
		}
		blockers = append(blockers, "status is "+status)
	}
	if subagents > 0 {
		blockers = append(blockers, fmt.Sprintf("%d background agent(s) still running", subagents))
	}
	if backgroundShells > 0 {
		blockers = append(blockers, fmt.Sprintf("%d background shell command(s) still running", backgroundShells))
	}
	return "temporary sandbox mode can only change while the agent is fully idle; " +
		strings.Join(blockers, ", ")
}

// dashboardSandboxRestartAgent stops and resumes the same conversation under
// either a temporary harness-native sandbox-off mode or its preserved normal
// relaunch posture. The durable override survives the stop→resume gap and a
// daemon restart, but never replaces the stable normal mode.
func dashboardSandboxRestartAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	convID := resolved.ConvID
	agentID := resolved.AgentID
	if agentID == "" {
		writeError(w, http.StatusConflict, "not_agent",
			"temporary sandbox restart requires a stable agent identity")
		return
	}

	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "decode request: "+err.Error())
		return
	}
	action := strings.TrimSpace(body.Action)
	if action != sandboxRestartUnlock && action != sandboxRestartRestore {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("action must be %q or %q", sandboxRestartUnlock, sandboxRestartRestore))
		return
	}

	_, active, err := db.TemporarySandboxModeForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read temporary sandbox mode: "+err.Error())
		return
	}
	if action == sandboxRestartUnlock && active {
		writeError(w, http.StatusConflict, "already_unlocked",
			"the agent is already running with its temporary sandbox override")
		return
	}
	if action == sandboxRestartRestore && !active {
		writeError(w, http.StatusConflict, "already_restored",
			"the agent is already using its normal sandbox configuration")
		return
	}
	if failure := sandboxRestartIdleFailure(pickAliveSession(convID), time.Now()); failure != "" {
		writeError(w, http.StatusConflict, "agent_not_idle", failure)
		return
	}

	normal, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		writeError(w, http.StatusConflict, "relaunch_profile", err.Error())
		return
	}
	var override *string
	if action == sandboxRestartUnlock {
		h, resolveErr := harness.Resolve(normal.Harness)
		if resolveErr != nil {
			writeError(w, http.StatusConflict, "harness", resolveErr.Error())
			return
		}
		mode, modeErr := harness.SandboxOffMode(h)
		if modeErr != nil {
			writeError(w, http.StatusConflict, "unsupported", modeErr.Error())
			return
		}
		override = &mode
	}

	stopped := escalateShutdown(convID, shutdownGrace)
	if stopped.Outcome == shutdownFailed {
		writeError(w, http.StatusInternalServerError, "shutdown_failed",
			"could not stop the idle agent before changing sandbox mode: "+stopped.Detail)
		return
	}
	if err := db.SetTemporarySandboxMode(
		agentID, normal.NormalSandbox, normal.NormalSandboxSource, override,
	); err != nil {
		// The posture did not change, so restore availability under the old
		// configuration before reporting the persistence failure.
		resume := resumeOneConvLocked(convID, false, true)
		detail := "persist temporary sandbox mode: " + err.Error()
		if resume.Action != "resumed" && resume.Action != "skipped:already_online" {
			detail += "; restoring the stopped agent also failed: " + resume.Detail
		}
		writeError(w, http.StatusInternalServerError, "io", detail)
		return
	}

	resume := resumeOneConvLocked(convID, false, true)
	if resume.Action != "resumed" && resume.Action != "skipped:already_online" {
		writeError(w, http.StatusInternalServerError, "restart_failed",
			"sandbox posture was updated, but the agent could not be restarted: "+resume.Detail+
				"; use the normal wake action to retry")
		return
	}
	mode, active, _ := db.TemporarySandboxModeForConv(convID)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":               agentID,
		"conv_id":                convID,
		"temporary_sandbox_mode": mode,
		"sandbox_unlocked":       active,
		"restart":                resume.Action,
	})
}
