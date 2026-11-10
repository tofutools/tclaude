package agentd

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A standalone relaunch (`tclaude conv resume` and the conversation watch TUI)
// starts its pane itself, without agentd, and renders the sandbox snapshot
// stored for the agent. These two human-only routes let it get the sandbox an
// ordinary resume prepares (prepareRelaunchSandbox: the profiles re-read, the
// group's environment, the relaunch policy, the launch gates and the fallback
// to the stored policy), in the two steps an ordinary resume takes around its
// launch:
//
//   - prepare records the prepared snapshot for the agent, as a resume does
//     before it launches, and keeps the previous one;
//   - settle, after the launch, removes the agent-owned directories the
//     launch no longer uses (launched), or restores the previous snapshot and
//     removes the new directories (not launched).
//
// The standalone path keeps working with the daemon down: it then launches
// under the stored policy, with a warning.

type standaloneRelaunch struct {
	agentID             string
	previous, effective *sandboxpolicy.Snapshot
}

var standaloneRelaunches = struct {
	sync.Mutex
	byConv map[string]standaloneRelaunch
}{byConv: map[string]standaloneRelaunch{}}

// StandaloneRelaunchRequest is the body of both routes.
type StandaloneRelaunchRequest struct {
	ConvID string `json:"conv_id"`
	// Launched reports, to settle, whether the pane started.
	Launched bool `json:"launched,omitempty"`
}

// StandaloneRelaunchPrepared is prepare's answer: what to tell the human,
// and, from prepare, the relaunch decisions the pane launches with.
type StandaloneRelaunchPrepared struct {
	Warnings []string `json:"warnings,omitempty"`
	Changes  []string `json:"changes,omitempty"`
	// RemoteControl is whether the pane arms Remote Access: the posture the
	// agent last launched with, or the group's current policy where the
	// relaunch policy re-reads it (relaunchRemoteControl, as on resume).
	RemoteControl *bool `json:"remote_control,omitempty"`
}

func decodeStandaloneRelaunch(w http.ResponseWriter, r *http.Request, action string, body *StandaloneRelaunchRequest) (string, bool) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return "", false
	}
	if !requireHuman(w, r, action) {
		return "", false
	}
	if err := json.NewDecoder(r.Body).Decode(body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "decode body: "+err.Error())
		return "", false
	}
	convID := strings.TrimSpace(body.ConvID)
	if convID == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "conv_id is required")
		return "", false
	}
	return convID, true
}

func handleStandaloneRelaunchPrepare(w http.ResponseWriter, r *http.Request) {
	var body StandaloneRelaunchRequest
	convID, ok := decodeStandaloneRelaunch(w, r, "prepare a standalone relaunch", &body)
	if !ok {
		return
	}
	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	alive, err := pickAliveSessionStrict(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "check whether the conversation is running: "+err.Error())
		return
	}
	if alive != nil {
		writeError(w, http.StatusConflict, "agent_live", "the conversation is already running in "+alive.TmuxSession)
		return
	}
	cfg, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		writeError(w, http.StatusConflict, "resume_profile", err.Error())
		return
	}
	cwd := relaunchCwd(cfg)
	if cwd == "" {
		writeError(w, http.StatusConflict, "resume_profile", "no absolute launch directory is available; recreate this agent")
		return
	}
	if fail := sandboxImplementationPostureFailure(cfg.Harness, cfg.activeSandboxImplementation()); fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	prepared, fail := prepareRelaunchSandbox(relaunchSandboxRequestFor(convID, cwd, generateSpawnLabel(), cfg))
	if fail != nil {
		writeSpawnFailure(w, fail)
		return
	}
	remoteControl, remoteControlChange, remoteControlNote := relaunchRemoteControl(convID, cfg.Harness, cfg.RemoteControl)
	out := StandaloneRelaunchPrepared{Warnings: prepared.warnings, RemoteControl: &remoteControl}
	if remoteControlNote != "" {
		out.Warnings = append(out.Warnings, remoteControlNote)
	}
	// A temporary built-in mode never persists its policy, as on resume.
	if prepared.effective != nil && !cfg.TemporaryHarnessBuiltinMode {
		agentID, err := db.AgentIDForConv(convID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", "record refreshed sandbox snapshot: "+err.Error())
			return
		}
		if agentID != "" {
			if err := db.SetAgentEffectiveSandboxConfig(agentID, prepared.effective); err != nil {
				writeError(w, http.StatusInternalServerError, "io", "record refreshed sandbox snapshot: "+err.Error())
				return
			}
			standaloneRelaunches.Lock()
			standaloneRelaunches.byConv[convID] = standaloneRelaunch{
				agentID: agentID, previous: prepared.policy.Previous, effective: prepared.effective}
			standaloneRelaunches.Unlock()
			out.Changes = prepared.changes
		}
	}
	if remoteControlChange != "" {
		out.Changes = append(out.Changes, remoteControlChange)
	}
	writeJSON(w, http.StatusOK, out)
}

func handleStandaloneRelaunchSettle(w http.ResponseWriter, r *http.Request) {
	var body StandaloneRelaunchRequest
	convID, ok := decodeStandaloneRelaunch(w, r, "settle a standalone relaunch", &body)
	if !ok {
		return
	}
	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	standaloneRelaunches.Lock()
	pending, found := standaloneRelaunches.byConv[convID]
	delete(standaloneRelaunches.byConv, convID)
	standaloneRelaunches.Unlock()
	if found {
		// Another launch (agentd's own resume) may have recorded its own
		// snapshot since: that one is not this relaunch's to settle.
		stored, err := db.AgentEffectiveSandboxConfigForConv(convID)
		found = err == nil && sameSnapshot(stored, pending.effective)
	}
	if !found {
		// Nothing was prepared, or the daemon restarted in between: the
		// stored snapshot stands, as after a resume.
		writeJSON(w, http.StatusOK, StandaloneRelaunchPrepared{})
		return
	}
	var warnings []string
	if body.Launched {
		if pending.previous != nil {
			if _, err := removeSupersededMaterializedAgentDirectories(*pending.previous, *pending.effective); err != nil {
				warnings = append(warnings, "remove superseded agent-owned directories: "+err.Error())
			}
		}
	} else {
		if pending.previous != nil {
			if _, err := removeSupersededMaterializedAgentDirectories(*pending.effective, *pending.previous); err != nil {
				warnings = append(warnings, "remove unused agent-owned directories: "+err.Error())
			}
		}
		if err := db.SetAgentEffectiveSandboxConfig(pending.agentID, pending.previous); err != nil {
			warnings = append(warnings, "restore previous sandbox snapshot: "+err.Error())
		}
	}
	writeJSON(w, http.StatusOK, StandaloneRelaunchPrepared{Warnings: warnings})
}

func sameSnapshot(a, b *sandboxpolicy.Snapshot) bool {
	ra, errA := json.Marshal(a)
	rb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ra) == string(rb)
}
