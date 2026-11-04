package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Applying an edited spawn profile to one agent that holds a copy of it
// (tcl-hxuwnc), the write half of the re-apply preview in profile_reapply.go.
// One request per agent, so each change is its own audit row naming the agent.
//
//   - A stopped agent gets the new copy directly; it takes effect on its next
//     resume, reincarnation or clone.
//   - A running, fully idle agent is stopped, given the copy and resumed, as
//     one step under its launch lock (the same stop and resume a restart does).
//   - A busy agent is skipped: nothing is stopped or written.
//
// The copy written keeps the old values of the fields a re-apply does not
// carry yet (profileReapplyNotYet: roles, the model proxy) and of the grant inputs, so the stored
// copy says what the agent really runs with and the preview keeps listing
// those differences as not reapplied. The posture gates run inside
// applySpawnCopy, against the agent's spawner and, for an agent caller, the
// re-applier too; the conferral gates run here, against the re-applier. They
// pass trivially while grant inputs are kept (tcl-fg2kqk re-applies grants).

// Outcomes of one re-apply.
const (
	reapplyApplied          = "applied"             // stopped agent: takes effect on its next launch
	reapplyAppliedRestarted = "applied_restarted"   // idle agent: stopped, applied, resumed
	reapplyBusySkipped      = "busy_skipped"        // running and not idle: untouched
	reapplyRestartedNoApply = "restarted_unapplied" // stopped and resumed, but the apply was refused
)

// reapplyKeepsOld reports whether a profile JSON key keeps the agent's copied
// value on re-apply: a grant input, or a field relaunch does not re-apply yet.
func reapplyKeepsOld(key string) bool {
	if profileReapplyGrantKeys[key] {
		return true
	}
	field, ok := launchFieldForProfileField(spawnProfileJSONGoNames[key])
	return ok && profileReapplyNotYet[field.Key]
}

// reappliedTier is the copy of prof an agent gets on re-apply: the profile as
// it is now, except the keys reapplyKeepsOld names, which keep the copied
// values.
func reappliedTier(old db.SpawnCopyTier, prof *db.SpawnProfile) (db.SpawnCopyTier, error) {
	fresh := spawnCopyTier(old.Tier, prof)
	if fresh == nil {
		return db.SpawnCopyTier{}, fmt.Errorf("copy profile %q", prof.Name)
	}
	merged := profileJSONMap(fresh.Profile)
	copied := profileJSONMap(old.Profile)
	if merged == nil || copied == nil {
		return db.SpawnCopyTier{}, fmt.Errorf("read the copies of profile %q", prof.Name)
	}
	for key := range merged {
		if reapplyKeepsOld(key) {
			delete(merged, key)
		}
	}
	for key, raw := range copied {
		if reapplyKeepsOld(key) {
			merged[key] = raw
		}
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return db.SpawnCopyTier{}, err
	}
	fresh.Profile = raw
	return *fresh, nil
}

type profileReapplyResult struct {
	AgentID string `json:"agent_id"`
	ConvID  string `json:"conv_id,omitempty"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// handleSpawnProfileReapply is POST /v1/spawn-profiles/{name}/reapply
// {agent_id}: re-apply the profile to one agent holding a copy of it.
// profiles.manage, like the preview.
func handleSpawnProfileReapply(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProfilesManage)
	if !ok {
		return
	}
	var body struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || strings.TrimSpace(body.AgentID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", `body must be {"agent_id": "..."}`)
		return
	}
	agentID := strings.TrimSpace(body.AgentID)
	prof, err := db.ResolveSpawnProfile(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if prof == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such profile")
		return
	}
	a, err := db.GetAgent(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if a == nil || a.CurrentConvID == "" {
		writeError(w, http.StatusNotFound, "not_found", "unknown agent "+agentID)
		return
	}
	if !a.Active() {
		writeError(w, http.StatusConflict, "agent_retired", "agent "+agentID+" is retired")
		return
	}
	convID := a.CurrentConvID
	setAuditTargetConv(r, convID)
	setAuditTargetLabel(r, fmt.Sprintf("re-apply profile %s to %s", prof.Name, auditConvLabel(convID)))
	// An agent caller acts on another agent's launch settings and may stop
	// and resume it, which is what morph does: it needs agent.morph over the
	// target, and the profile must be one it could spawn with (not disabled,
	// not operator-only). A disabled profile is refused for everyone.
	if caller != "" {
		if _, ok := requireCrossAgentPermission(w, r, PermAgentMorph, convID); !ok {
			return
		}
	}
	if fail := profileSpawnFailure(prof, caller); fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}

	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		writeError(w, http.StatusConflict, "stale_generation", err.Error()+"; apply it again")
		return
	}
	// Built under the lock, so a concurrent re-apply of another tier cannot
	// be overwritten by a copy read before it committed.
	next, fail := reapplyCopyFor(r, caller, agentID, prof)
	if fail != nil {
		writeError(w, fail.Status, fail.Kind, fail.Msg)
		return
	}
	res := profileReapplyResult{AgentID: agentID, ConvID: convID}
	if pickAliveSession(convID) == nil {
		if fail := applySpawnCopyLocked(agentID, convID, next, caller); fail != nil {
			writeError(w, fail.Status, fail.Kind, fail.Msg)
			return
		}
		res.Outcome = reapplyApplied
		res.Detail = "takes effect on the agent's next resume, reincarnation or clone"
		setAuditDetail(r, res.Outcome)
		writeJSON(w, http.StatusOK, res)
		return
	}
	// Running: only a fully idle agent is stopped, applied and resumed.
	if failure := agentRestartIdleFailure(pickAliveSession(convID), time.Now()); failure != "" {
		res.Outcome, res.Detail = reapplyBusySkipped, "busy, skipped: "+strings.TrimPrefix(failure, "an agent can only restart while fully idle; ")
		setAuditDetail(r, res.Outcome)
		writeJSON(w, http.StatusOK, res)
		return
	}
	_, applyFail, restartFail := restartIdleAgentLocked(agentID, convID, func() *spawnFailure {
		return applySpawnCopyLocked(agentID, convID, next, caller)
	})
	switch {
	case restartFail != nil && applyFail == nil && restartFail.Kind != "restart_failed":
		// Nothing was written: it was not stopped (it got busy, or cannot be
		// relaunched), or its generation changed while stopping, which the
		// restart's message says.
		if restartFail.Kind == "agent_not_idle" {
			res.Outcome, res.Detail = reapplyBusySkipped, "busy, skipped: "+restartFail.Msg
			setAuditDetail(r, res.Outcome)
			writeJSON(w, http.StatusOK, res)
			return
		}
		writeError(w, restartFail.Status, restartFail.Kind, restartFail.Msg)
		return
	case restartFail != nil:
		msg := restartFail.Msg
		if applyFail == nil {
			msg = "the copy was applied, but " + msg
		} else {
			msg = "the copy was not applied (" + applyFail.Msg + "), and " + msg
		}
		writeError(w, restartFail.Status, restartFail.Kind, msg)
		return
	case applyFail != nil:
		res.Outcome = reapplyRestartedNoApply
		res.Detail = "refused, so the agent was resumed with its old settings: " + applyFail.Msg
		setAuditDetail(r, res.Outcome+": "+applyFail.Kind)
		writeJSON(w, applyFail.Status, map[string]any{
			"error": res.Detail, "code": applyFail.Kind, "agent_id": agentID, "conv_id": convID, "outcome": res.Outcome,
		})
		return
	}
	res.Outcome = reapplyAppliedRestarted
	res.Detail = "stopped, applied and resumed"
	setAuditDetail(r, res.Outcome)
	writeJSON(w, http.StatusOK, res)
}

// reapplyCopyFor builds the spawn copy the agent gets: its current copy with
// the tier holding prof replaced by reappliedTier. The conferral gates a
// profile edit runs are re-run against the caller, for the grants the new
// copy would add over the agent's copy.
func reapplyCopyFor(r *http.Request, caller, agentID string, prof *db.SpawnProfile) (db.AgentSpawnCopy, *spawnFailure) {
	layers, err := db.AgentLaunchLayersFor(agentID)
	if err != nil {
		return db.AgentSpawnCopy{}, &spawnFailure{http.StatusInternalServerError, "io", "load launch layers: " + err.Error()}
	}
	if layers == nil || layers.SpawnCopy == nil {
		return db.AgentSpawnCopy{}, &spawnFailure{http.StatusConflict, "no_launch_layers",
			"this agent was spawned before launch layers were recorded, so its explicit values are not known; respawn it to use a new spawn profile"}
	}
	next := db.AgentSpawnCopy{CopiedAt: time.Now().UTC(), Tiers: append([]db.SpawnCopyTier(nil), layers.SpawnCopy.Tiers...)}
	found := false
	for i, tier := range next.Tiers {
		if tier.ProfileID != prof.ID {
			continue
		}
		found = true
		fresh, err := reappliedTier(tier, prof)
		if err != nil {
			return db.AgentSpawnCopy{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		before, err := copiedGrantInputs(tier.Profile)
		if err != nil {
			return db.AgentSpawnCopy{}, &spawnFailure{http.StatusConflict, "invalid_spawn_copy", "this agent's copy of the profile could not be read"}
		}
		after, err := copiedGrantInputs(fresh.Profile)
		if err != nil {
			return db.AgentSpawnCopy{}, &spawnFailure{http.StatusInternalServerError, "io", err.Error()}
		}
		after.Name = prof.Name
		if fail := profileConferralFailure(r, caller, before, after, reapplyGroupName(agentID), "re-applying it to "+agentID); fail != nil {
			return db.AgentSpawnCopy{}, fail
		}
		next.Tiers[i] = fresh
	}
	if !found {
		return db.AgentSpawnCopy{}, &spawnFailure{http.StatusConflict, "no_copy",
			fmt.Sprintf("agent %s holds no copy of profile %q", agentID, prof.Name)}
	}
	return next, nil
}

// reapplyGroupName is the group the agent was spawned into, which bounds a
// group-scoped conferral; "" when it has none (or it cannot be read, which
// asks the stricter every-group question).
func reapplyGroupName(agentID string) string {
	snapshot, err := db.GetAgentStartupSnapshot(agentID)
	if err != nil || snapshot == nil || snapshot.SpawnGroupID == 0 {
		return ""
	}
	g, err := db.GetAgentGroupByID(snapshot.SpawnGroupID)
	if err != nil || g == nil {
		return ""
	}
	return g.Name
}

// reapplyDefaultTierNote says, for a default profile, that re-applying it
// reaches every agent spawned under it, not only the ones a caller named it
// for.
func reapplyDefaultTierNote(agents []profileReapplyAgent) string {
	for _, a := range agents {
		if a.Tier == db.SpawnCopyTierGroupDefault || a.Tier == db.SpawnCopyTierGlobalDefault {
			return "This profile is a default: re-applying it reaches every agent spawned under that default, including ones whose spawn never named it."
		}
	}
	return ""
}
