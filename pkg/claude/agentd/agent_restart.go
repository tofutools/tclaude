package agentd

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// agentRestartIdleFailure is the authoritative preflight shared by ordinary
// restarts and temporary sandbox transitions. It intentionally reads one DB
// snapshot rather than pretending to make the later stop race-free: the
// operator asked for a basic idle check, and the actual restart still uses the
// normal lifecycle path.
func agentRestartIdleFailure(sess *db.SessionRow, now time.Time) string {
	if sess == nil {
		return "the agent must be online before it can be restarted"
	}
	subagents := sess.SubagentCount
	if set := db.ParseSubagentSet(sess.SubagentsJSON); set != nil {
		subagents = set.LiveCount(now)
	}
	backgroundShells := db.ParseBgShellSet(sess.BgShellsJSON).LiveCount(now)
	monitors := db.ParseMonitorSet(sess.MonitorsJSON).LiveCount(now)
	if sess.Status == session.StatusIdle && subagents == 0 && backgroundShells == 0 && monitors == 0 {
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
	if monitors > 0 {
		blockers = append(blockers, fmt.Sprintf("%d monitor(s) still running", monitors))
	}
	return "an agent can only restart while fully idle; " + strings.Join(blockers, ", ")
}

// dashboardRestartAgent stops and resumes the same conversation under its
// current durable launch posture. Resume deliberately re-resolves mutable
// sandbox-profile assignments and rules, while preserving any active temporary
// sandbox override.
func dashboardRestartAgent(w http.ResponseWriter, _ *http.Request, convSelector string) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	convID := resolved.ConvID
	agentID := resolved.AgentID
	if agentID == "" {
		writeError(w, http.StatusConflict, "not_agent",
			"restart requires a stable agent identity")
		return
	}

	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	switchedClients, _, fail := restartIdleAgentLocked(agentID, convID, nil)
	if fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":         agentID,
		"conv_id":          convID,
		"restart":          "resumed",
		"switched_clients": switchedClients,
	})
}

// restartIdleAgentLocked stops a fully idle agent and resumes it, for a
// caller holding its resume launch lock. whileStopped, when set, runs between
// the stop and the resume (the profile re-apply writes the agent's launch
// settings there); the agent is resumed whatever it returns, and its failure
// comes back as stoppedFail. fail is the restart's own failure: the agent was
// not stopped, or stopped and could not be resumed.
func restartIdleAgentLocked(agentID, convID string, whileStopped func() *spawnFailure) (switchedClients int, stoppedFail, fail *spawnFailure) {
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		return 0, nil, &spawnFailure{http.StatusConflict, "stale_generation", err.Error()}
	}
	liveSession := pickAliveSession(convID)
	if failure := agentRestartIdleFailure(liveSession, time.Now()); failure != "" {
		return 0, nil, &spawnFailure{http.StatusConflict, "agent_not_idle", failure}
	}
	// Fail before stopping when the durable launch anchor is already unusable.
	// Mutable sandbox-profile validation still belongs to the resume path,
	// because that is where the current policy is resolved and clamped.
	if _, err := durableRelaunchConfigForConv(convID); err != nil {
		return 0, nil, &spawnFailure{http.StatusConflict, "relaunch_profile", err.Error()}
	}
	// Nor stop an agent whose Claude subscription token is gone: it could
	// not come back until its owner sends the token again.
	if err := claudeSubscriptionRelaunchBlocked(agentID); err != nil {
		return 0, nil, &spawnFailure{http.StatusConflict, claudeSubscriptionLostCode, err.Error()}
	}

	// Preserve any attached human terminal across the stop/resume gap. The
	// handoff is best-effort and self-expiring; its deferred finish returns the
	// client to a still-live old pane after shutdown failure or removes the
	// bridge when no useful target survives.
	clientHandoff := beginAgentRestartTmuxHandoff(liveSession.TmuxSession)
	defer clientHandoff.finishForConv(convID)

	stopped := escalateShutdownUnderLaunchLock(convID, shutdownGrace)
	if stopped.Outcome == shutdownFailed {
		return 0, nil, &spawnFailure{http.StatusInternalServerError, "shutdown_failed",
			"could not stop the idle agent before restarting: " + stopped.Detail}
	}
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		return 0, nil, &spawnFailure{http.StatusConflict, "stale_generation",
			"agent generation changed while stopping; it was not restarted: " + err.Error()}
	}
	if whileStopped != nil {
		stoppedFail = whileStopped()
	}

	resume := resumeOneConvUnderLaunchLock(convID, false, nil)
	switchedClients = clientHandoff.finishForConv(convID)
	if resume.Action != "resumed" {
		return switchedClients, stoppedFail, &spawnFailure{http.StatusInternalServerError, "restart_failed",
			"the agent stopped, but could not be restarted: " + resume.Detail +
				"; use the normal wake action to retry"}
	}
	return switchedClients, stoppedFail, nil
}

func requireCurrentAgentGeneration(agentID, convID string) error {
	actor, err := db.GetAgentByConv(convID)
	if err != nil {
		return fmt.Errorf("resolve stable agent generation: %w", err)
	}
	if actor == nil || actor.AgentID != agentID {
		return fmt.Errorf("conversation %s no longer resolves to agent %s", convID, agentID)
	}
	if actor.CurrentConvID != convID {
		return fmt.Errorf("conversation %s is no longer the agent's current generation", convID)
	}
	return nil
}
