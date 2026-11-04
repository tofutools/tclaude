package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Recording of an agent's launch layers at first spawn (db.AgentLaunchLayers):
// the spawn profiles that took part, copied whole with their tier and ID, and
// the values the spawn request set explicitly. applySpawnCopy replaces the
// copy later, and every relaunch reads the replaced copy's fields through
// db.AgentRelaunchProfileForConv (docs/launch-resolution.md).

// applySpawnCopy re-applies a spawn copy to a stopped agent: it resolves the
// copy for relaunch and, in one transaction, writes the fields the copy owns
// into the agent's relaunch profile and replaces the copy. The next resume,
// reincarnation or clone launches with them; nothing about a resume changes.
// It is the one way to re-apply a spawn copy.
//
// The agent must be stopped (agent_live otherwise): a running session
// projects its own launch values into the relaunch profile on every save and
// would undo the write. A caller that wants a running, idle agent updated
// stops it, applies, and resumes it.
//
// The posture the agent ends up with passes the gates a fresh launch does
// (launchPostureFailure) against the agent that spawned it, when it differs
// from the stored one. The apply is refused, naming why, for an agent spawned
// before launch layers existed (its explicit values are not known), one with
// no startup record, and one whose spawner or spawn group can no longer be
// read.
func applySpawnCopy(agentID string, spawnCopy db.AgentSpawnCopy) *spawnFailure {
	a, err := db.GetAgent(agentID)
	if err != nil || a == nil || a.CurrentConvID == "" {
		return &spawnFailure{http.StatusNotFound, "not_found", "unknown agent " + agentID}
	}
	if !a.Active() {
		return &spawnFailure{http.StatusConflict, "agent_retired", "agent " + agentID + " is retired"}
	}
	convID := a.CurrentConvID
	lock := resumeLaunchLock(convID)
	lock.Lock()
	defer lock.Unlock()
	return applySpawnCopyLocked(agentID, convID, spawnCopy)
}

// applySpawnCopyLocked is applySpawnCopy for a caller already holding the
// agent's resume launch lock for convID: the profile re-apply's stop, apply
// and resume of a running agent holds it across all three.
func applySpawnCopyLocked(agentID, convID string, spawnCopy db.AgentSpawnCopy) *spawnFailure {
	// A reincarnation or morph may have moved the agent to a new
	// conversation while this waited for the lock; that one is not locked.
	if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
		return &spawnFailure{http.StatusConflict, "stale_generation", err.Error() + "; apply it again"}
	}
	alive, err := pickAliveSessionStrict(convID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "check whether the agent is running: " + err.Error()}
	}
	if alive != nil {
		return &spawnFailure{http.StatusConflict, "agent_live",
			"the agent is running; stop it before applying a spawn copy (or stop, apply and resume it as one step)"}
	}
	layers, err := db.AgentLaunchLayersFor(agentID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "load launch layers: " + err.Error()}
	}
	if layers == nil {
		return &spawnFailure{http.StatusConflict, "no_launch_layers",
			"this agent was spawned before launch layers were recorded, so its explicit values are not known; respawn it to use a new spawn profile"}
	}
	conversation, err := db.ConversationResumeProfileForConv(convID)
	if err != nil || conversation == nil {
		return &spawnFailure{http.StatusConflict, "no_resume_profile", "the agent's conversation has no resume record"}
	}
	stored, storedRaw, err := db.AgentRelaunchProfileRaw(agentID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "load relaunch profile: " + err.Error()}
	}
	if stored == nil {
		return &spawnFailure{http.StatusConflict, "no_relaunch_profile", "the agent has no stored launch settings to apply a copy to"}
	}
	// The spawner bounds what the copy may give the agent, and the spawn
	// group supplies the group settings the resolver reads. Neither may be
	// guessed: a missing record is refused, not treated as a human spawn.
	snapshot, err := db.GetAgentStartupSnapshot(agentID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "load the agent's startup record: " + err.Error()}
	}
	if snapshot == nil {
		return &spawnFailure{http.StatusConflict, "no_startup_record",
			"the agent has no startup record, so who spawned it and into which group cannot be checked"}
	}
	var group *db.AgentGroup
	if snapshot.SpawnGroupID != 0 {
		if group, err = db.GetAgentGroupByID(snapshot.SpawnGroupID); err != nil || group == nil {
			return &spawnFailure{http.StatusConflict, "spawn_group_gone",
				fmt.Sprintf("the group this agent was spawned into (id %d) no longer exists, so its group settings cannot be resolved", snapshot.SpawnGroupID)}
		}
	}
	spawnerConv := ""
	if snapshot.SpawnedByAgent != "" {
		spawner, err := db.GetAgent(snapshot.SpawnedByAgent)
		if err != nil || spawner == nil || spawner.CurrentConvID == "" {
			return &spawnFailure{http.StatusConflict, "spawner_gone",
				fmt.Sprintf("the agent that spawned this one (%s) no longer exists, so what it may grant cannot be checked", snapshot.SpawnedByAgent)}
		}
		spawnerConv = spawner.CurrentConvID
	}

	harnessName := harnessOrDefault(conversation.Harness)
	owned, fail := resolveSpawnCopyRelaunch(group, spawnerConv, harnessName, stored, spawnCopy, layers.Explicit)
	if fail != nil {
		return fail
	}
	next := db.ComposeAgentRelaunchProfile(stored, owned)
	// The same launch-config validation a morph runs: a profile resume would
	// reject is refused here, so a resume still always works.
	cfg, err := durableRelaunchConfigForConvWith(convID, func(p *db.AgentRelaunchProfile) { *p = *next })
	if err != nil {
		return &spawnFailure{http.StatusConflict, "relaunch_profile", "the agent could not be launched with this copy: " + err.Error()}
	}
	if fail := spawnCopyPostureFailure(spawnerConv, harnessName, convID, stored, next, cfg); fail != nil {
		return fail
	}
	if err := db.ApplyAgentSpawnCopy(agentID, convID, storedRaw, *next, spawnCopy); err != nil {
		if errors.Is(err, db.ErrRelaunchProfileChanged) {
			return &spawnFailure{http.StatusConflict, "changed", err.Error()}
		}
		return &spawnFailure{http.StatusInternalServerError, "io", "apply spawn copy: " + err.Error()}
	}
	return nil
}

// spawnCopyPostureFailure gates the posture a re-applied copy gives an agent
// as a fresh launch is gated: the lineage guards against its spawner, the
// confined-approval rule and the Copilot API loopback rule. A posture equal to
// the stored one passed when the agent launched with it and is not re-judged,
// so re-applying an unchanged copy never fails on a rule the agent's own
// launch acknowledged.
func spawnCopyPostureFailure(spawnerConv, harnessName, convID string, stored, next *db.AgentRelaunchProfile, cfg *durableRelaunchConfig) *spawnFailure {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	flag := func(p *bool) bool { return p != nil && *p }
	same := str(stored.HarnessBuiltinMode) == str(next.HarnessBuiltinMode) &&
		str(stored.SandboxImplementation) == str(next.SandboxImplementation) &&
		str(stored.ApprovalPolicy) == str(next.ApprovalPolicy) &&
		flag(stored.ApprovalAutoReview) == flag(next.ApprovalAutoReview) &&
		flag(stored.CopilotAPI) == flag(next.CopilotAPI)
	if same {
		return nil
	}
	h, err := harness.ResolveSpawnable(harnessName)
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	// Judge the posture the next resume will launch with: the validated
	// config's normal mode and implementation, which can differ from the raw
	// stored spelling (a restored layered implementation, a temporary mode).
	mode, implementation := cfg.Sandbox, cfg.SandboxImplementation
	if cfg.TemporaryHarnessBuiltinMode {
		mode = cfg.NormalSandbox
	}
	if fail := launchPostureFailure(spawnerConv, h, harnessName, mode,
		implementation, str(next.ApprovalPolicy), flag(next.ApprovalAutoReview), false); fail != nil {
		return fail
	}
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "load the agent's sandbox policy: " + err.Error()}
	}
	if fail := copilotAPILoopbackFailure(flag(next.CopilotAPI), snapshot, implementation); fail != nil {
		return fail
	}
	// The sandbox-profile gates a resume runs (executeSpawn), so a posture the
	// agent's sandbox profile cannot carry is refused now, with the reason,
	// rather than at its next wake.
	if fail := sandboxProfileCapabilityFailure(harnessName, mode, snapshot, implementation); fail != nil {
		return fail
	}
	_, fail := planSandboxProfileAccessForLaunch(harnessName, mode, snapshot, implementation,
		session.ModelTransportLaunchContext{Model: cfg.Model, ModelProxy: cfg.ModelProxy, Cwd: cfg.Cwd}, false)
	return fail
}


// spawnCopyTier copies one participating profile, in the profile export
// format so the stored copy has stable JSON names. nil is no tier.
func spawnCopyTier(tier string, prof *db.SpawnProfile) *db.SpawnCopyTier {
	if prof == nil {
		return nil
	}
	raw, err := json.Marshal(profileToJSON(prof))
	if err != nil {
		slog.Warn("spawn: failed to copy spawn profile", "profile", prof.Name, "error", err)
		return nil
	}
	return &db.SpawnCopyTier{Tier: tier, ProfileID: prof.ID, ProfileName: prof.Name, Profile: raw}
}

// spawnCopyTiers keeps the non-nil tiers, in order.
func spawnCopyTiers(tiers ...*db.SpawnCopyTier) []db.SpawnCopyTier {
	var out []db.SpawnCopyTier
	for _, t := range tiers {
		if t != nil {
			out = append(out, *t)
		}
	}
	return out
}

// captureExplicitLaunch records, per registry field, the spawn request fields
// the caller set. It reads the request as decoded, before any profile tier
// fills it in, so only the caller's own choices count as explicit. Spawn-only
// fields (the brief, access grants, the call's own shape) are left out: the
// first spawn consumes them and a relaunch has no use for them.
func captureExplicitLaunch(req *agent.SpawnRequest, at time.Time) map[string]db.ExplicitLaunchValue {
	if req == nil {
		return nil
	}
	v := reflect.ValueOf(*req)
	out := map[string]db.ExplicitLaunchValue{}
	for _, f := range launchspec.Fields {
		if f.Layer == launchspec.LayerSpawnOnly {
			continue
		}
		fields := map[string]json.RawMessage{}
		for _, name := range f.RequestFields {
			fv := v.FieldByName(name)
			if !fv.IsValid() || fv.IsZero() {
				continue
			}
			raw, err := json.Marshal(fv.Interface())
			if err != nil {
				slog.Warn("spawn: failed to record explicit launch value", "field", name, "error", err)
				continue
			}
			fields[name] = raw
		}
		if len(fields) > 0 {
			out[f.Key] = db.ExplicitLaunchValue{Fields: fields, Provenance: db.ExplicitAtSpawn, SetAt: at}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// spawnLaunchLayers assembles the layers a first spawn records: the caller's
// own tiers (inline, named) above the default tiers. A profile that fills two
// slots (a trigger worker's profile sits in the group-default slot as well)
// is recorded once, at its higher tier.
//
// nil means nothing is recorded, and the agent reads as pre-change: a launch
// whose tiers this process did not resolve (an enrollment the pending-spawn
// sweeper rebuilt from the pending row) or one that brings an existing
// identity (a federation arrival or bundle move) has no first-spawn copy to
// take here.
func spawnLaunchLayers(p spawnParams, at time.Time) *db.AgentLaunchLayers {
	if !p.spawnCopyResolved || p.Arrival != nil || p.BundleHistory != nil {
		return nil
	}
	seen := map[int64]bool{}
	tiers := []db.SpawnCopyTier{}
	for _, group := range [][]db.SpawnCopyTier{p.spawnCopyCallerTiers, p.spawnCopyDefaultTiers} {
		for _, t := range group {
			if t.ProfileID != 0 {
				if seen[t.ProfileID] {
					continue
				}
				seen[t.ProfileID] = true
			}
			tiers = append(tiers, t)
		}
	}
	return &db.AgentLaunchLayers{
		SpawnCopy: &db.AgentSpawnCopy{CopiedAt: at, Tiers: tiers},
		Explicit:  p.explicitLaunch,
	}
}
