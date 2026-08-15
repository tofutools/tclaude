package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const (
	sandboxRestartUnlock  = "unlock"
	sandboxRestartRestore = "restore"
)

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

	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		writeError(w, http.StatusConflict, "stale_generation", err.Error())
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

	_, active, err := db.TemporaryHarnessBuiltinModeForConv(convID)
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
	liveSession := pickAliveSession(convID)
	if failure := agentRestartIdleFailure(liveSession, time.Now()); failure != "" {
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
		implementation, implErr := sandboxpolicy.NormalizeImplementation(
			normal.SandboxImplementation,
		)
		if implErr != nil {
			writeError(w, http.StatusConflict, "relaunch_profile",
				"invalid normal sandbox implementation: "+implErr.Error())
			return
		}
		// "Restart without sandbox" trades access confinement away, which is
		// what the operator is asking for. An implementation that has no
		// access confinement has nothing to trade: the relaunch would force
		// harness-builtin and rebuild the snapshot through
		// temporarySandboxLaunchSnapshot, which zeroes ResourceLimits — so the
		// only thing this action would actually remove from a resource-only
		// agent is its per-agent cgroup, under a label that says sandbox.
		// Refuse instead of silently dropping the one boundary it applies.
		if implementation == sandboxpolicy.ImplementationResourceOnly {
			writeError(w, http.StatusConflict, "unsupported",
				fmt.Sprintf("this agent runs under sandbox implementation %q, which already has no OS-level access confinement to unlock; restarting without a sandbox would only remove the per-agent cgroup it does apply",
					implementation))
			return
		}
		h, resolveErr := harness.Resolve(normal.Harness)
		if resolveErr != nil {
			writeError(w, http.StatusConflict, "harness", resolveErr.Error())
			return
		}
		// Codex restores its persisted built-in permission profile while resuming
		// a conversation. In practice that overwrites both the explicit
		// danger-full-access resume flag and a pre-launch repair of Codex's thread
		// row. Refuse before stopping the live agent: offering a temporary unlock
		// here claims a security transition that the resumed process does not make.
		// A Codex launch under tclaude-layer remains eligible because its normal
		// harness mode is already sandbox-off; the temporary transition only
		// removes tclaude's independently controlled outer boundary.
		if h.Name == harness.CodexName &&
			implementation == sandboxpolicy.ImplementationHarnessBuiltin {
			writeError(w, http.StatusConflict, "unsupported",
				"Codex cannot restart this conversation without its harness-built-in sandbox because Codex restores the persisted sandbox policy on resume; choose the tclaude-layer sandbox implementation or start a new Codex conversation without the built-in sandbox")
			return
		}
		mode, modeErr := harness.SandboxOffMode(h)
		if modeErr != nil {
			writeError(w, http.StatusConflict, "unsupported", modeErr.Error())
			return
		}
		override = &mode
	}

	// A same-conversation resume cannot create its replacement tmux session
	// until the old same-named session has exited. Park attached clients on a
	// short-lived bridge before /exit, then carry them onto the resumed pane.
	// The deferred finish covers every error path: it switches back to a still
	// live old pane when shutdown failed, or removes the bridge if no useful
	// target survived.
	clientHandoff := beginAgentRestartTmuxHandoff(liveSession.TmuxSession)
	defer clientHandoff.finishForConv(convID)

	stopped := escalateShutdownUnderLaunchLock(convID, shutdownGrace)
	if stopped.Outcome == shutdownFailed {
		writeError(w, http.StatusInternalServerError, "shutdown_failed",
			"could not stop the idle agent before changing sandbox mode: "+stopped.Detail)
		return
	}
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		writeError(w, http.StatusConflict, "stale_generation",
			"agent generation changed while stopping; sandbox posture was not changed: "+err.Error())
		return
	}
	if err := db.SetTemporaryHarnessBuiltinMode(
		agentID, normal.NormalSandbox, normal.SandboxImplementation,
		normal.NormalSandboxSource, override,
	); err != nil {
		// The posture did not change, so restore availability under the old
		// configuration before reporting the persistence failure.
		resume := resumeOneConvUnderLaunchLock(convID, false, true, nil)
		clientHandoff.finishForConv(convID)
		detail := "persist temporary sandbox mode: " + err.Error()
		if resume.Action != "resumed" && resume.Action != "skipped:already_online" {
			detail += "; restoring the stopped agent also failed: " + resume.Detail
		}
		writeError(w, http.StatusInternalServerError, "io", detail)
		return
	}

	resume := resumeOneConvUnderLaunchLock(convID, false, true, nil)
	switchedClients := clientHandoff.finishForConv(convID)
	if resume.Action != "resumed" {
		writeError(w, http.StatusInternalServerError, "restart_failed",
			"sandbox posture was updated, but the agent could not be restarted: "+resume.Detail+
				"; use the normal wake action to retry")
		return
	}
	mode, active, _ := db.TemporaryHarnessBuiltinModeForConv(convID)
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":               agentID,
		"conv_id":                convID,
		"temporary_sandbox_mode": mode,
		"sandbox_unlocked":       active,
		"restart":                resume.Action,
		"switched_clients":       switchedClients,
	})
}
