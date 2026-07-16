package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Spawn guardrails — runaway-prevention for the case where the human
// grants an AGENT the `groups.spawn` permission. `groups.spawn` is
// default human-only; granting it to an agent used to come with no
// limits at all, so a spawn-capable agent stuck in a loop could fork
// CC instances and tmux sessions until the host fell over.
//
// checkSpawnGuardrails (1 and 2) and claimSpawnRateSlot (3) apply three
// checks before handleGroupSpawn launches anything:
//
//  1. Max group size — a hard property of the group
//     (agent_groups.max_members). Enforced for EVERY caller, the human
//     included; a human raises the cap to add more.
//  2. Group restriction — an agent may only spawn into a group it is a
//     member or owner of (default on; widened by an allowlist;
//     disablable). Skipped for the human.
//  3. Rate limit — at most SpawnMaxPerWindow spawns per caller-agent
//     per SpawnRateWindow. Skipped for the human.
//
// Humans (no claude ancestor — handleGroupSpawn's spawnerConvID is "")
// bypass 2 and 3, exactly as they bypass requirePermission everywhere
// else. Only 1 binds them, because a member cap is a property of the
// group, not a limit on the caller.
//
// The tunables below are package vars rather than constants so flow
// tests can drive the locked/unlocked branches without sleeping —
// same pattern as agentd.CloneCooldown. `tclaude agentd serve`
// overwrites them at startup from config.json via
// resolveSpawnGuardrailConfig.

// defaultSpawnMaxPerWindow is the built-in spawn-rate cap when
// config.json's agent.spawn_max_per_hour is unset. Conservative on
// purpose: a coordinator agent legitimately fanning out a team rarely
// needs more than a handful of spawns an hour, and the human can lift
// the cap when they do.
const defaultSpawnMaxPerWindow = 10

var (
	// SpawnGroupRestriction gates guardrail 2. When true (the
	// default), an agent may only spawn into a group it is a member or
	// owner of, plus any group named in SpawnAllowedGroups.
	SpawnGroupRestriction = true

	// SpawnAllowedGroups is the allowlist that widens guardrail 2: an
	// agent may always spawn into a group whose name appears here, even
	// when it is neither a member nor an owner.
	SpawnAllowedGroups []string

	// SpawnMaxPerWindow is the most spawns one caller-agent may make
	// per SpawnRateWindow (guardrail 3). 0 or negative disables the
	// rate limit entirely.
	SpawnMaxPerWindow = defaultSpawnMaxPerWindow

	// SpawnRateWindow is the rolling window the rate limit counts over.
	// Paired with the agent.spawn_max_per_hour config name, it stays an
	// hour; it is a var only so flow tests can shrink it.
	SpawnRateWindow = time.Hour
)

// resolveSpawnGuardrailConfig applies the config.json `agent` section's
// spawn-guardrail knobs to the package vars above. Called once at
// daemon startup (runServe). An absent field keeps the built-in
// default; a negative spawn_max_per_hour is warned about and treated
// as 0 (unlimited). Returns a short human-readable summary for the
// startup log line.
func resolveSpawnGuardrailConfig(cfg *config.Config) string {
	if cfg != nil && cfg.Agent != nil {
		a := cfg.Agent
		if a.SpawnGroupRestriction != nil {
			SpawnGroupRestriction = *a.SpawnGroupRestriction
		}
		if len(a.SpawnAllowedGroups) > 0 {
			SpawnAllowedGroups = append([]string(nil), a.SpawnAllowedGroups...)
		}
		if a.SpawnMaxPerHour != nil {
			v := *a.SpawnMaxPerHour
			if v < 0 {
				slog.Warn("negative agent.spawn_max_per_hour; treating as 0 (unlimited)", "value", v)
				v = 0
			}
			SpawnMaxPerWindow = v
		}
	}
	rate := "unlimited"
	if SpawnMaxPerWindow > 0 {
		rate = fmt.Sprintf("%d per %s", SpawnMaxPerWindow, SpawnRateWindow)
	}
	return fmt.Sprintf("group_restriction=%t allowed_groups=%v rate=%s",
		SpawnGroupRestriction, SpawnAllowedGroups, rate)
}

// checkSpawnGuardrails runs guardrails 1 and 2 for a request into group g
// by caller spawnerConvID ("" for the human). On a rejection it writes the
// HTTP error and returns false; the caller (handleGroupSpawn) just returns.
// The rate limit (guardrail 3) is claimed separately via claimSpawnRateSlot
// once the request has fully validated — in particular AFTER the dir
// write-proof gate, so the proof challenge round-trip (one 403 + one retry
// per spawn) costs one slot, not two.
func checkSpawnGuardrails(w http.ResponseWriter, g *db.AgentGroup, spawnerConvID string) bool {
	// 1. Max group size. A hard property of the group — binds every
	//    caller, the human included.
	if g.MaxMembers > 0 {
		members, err := db.ListAgentGroupMembers(g.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io",
				"spawn guardrail: count members: "+err.Error())
			return false
		}
		if len(members) >= g.MaxMembers {
			writeError(w, http.StatusConflict, "group_full",
				fmt.Sprintf("group %q is at its member cap (%d/%d); a human must raise "+
					"max_members (`tclaude agent groups set-max-members %s <n>`) before "+
					"more agents can be spawned in", g.Name, len(members), g.MaxMembers, g.Name))
			return false
		}
	}

	// 2 & 3 are agent-only. The human bypasses them.
	if spawnerConvID == "" {
		return true
	}

	// 2. Group restriction.
	return checkSpawnGroupRestriction(w, g, spawnerConvID)
}

// claimSpawnRateSlot enforces guardrail 3 for an agent caller ("" — the
// human — passes untouched): at most SpawnMaxPerWindow spawns per caller per
// SpawnRateWindow. A successful claim records the attempt, so the caller
// must proceed to the actual spawn (a later failure still counts — the
// intended runaway-prevention behaviour). Called after every validation
// gate, so a request refused earlier (bad cwd, missing write-proof, …)
// costs no slot.
func claimSpawnRateSlot(w http.ResponseWriter, spawnerConvID string) bool {
	if spawnerConvID == "" {
		return true
	}
	if err := db.ClaimSpawnSlot(spawnerConvID, SpawnMaxPerWindow, SpawnRateWindow, time.Now().UTC()); err != nil {
		if errors.Is(err, db.ErrSpawnRateLimited) {
			writeError(w, http.StatusTooManyRequests, "rate_limited",
				fmt.Sprintf("spawn rate limit reached for agent %s: at most %d spawns per %s. "+
					"A human can raise it via agent.spawn_max_per_hour in ~/.tclaude/config.json.",
					short8(spawnerConvID), SpawnMaxPerWindow, SpawnRateWindow))
			return false
		}
		writeError(w, http.StatusInternalServerError, "io",
			"spawn rate-limit check: "+err.Error())
		return false
	}
	return true
}

// checkSpawnGroupRestriction enforces guardrail 2: an agent caller may
// only spawn into a group it is a member or owner of, unless the group
// is allowlisted or the restriction is globally off. Owner counts
// alongside member because a group owner already wields unilateral
// power over the group's members elsewhere in the daemon (cross-agent
// lifecycle) — and a coordinator agent that grows a team is typically
// an owner rather than a member.
func checkSpawnGroupRestriction(w http.ResponseWriter, g *db.AgentGroup, spawnerConvID string) bool {
	if !SpawnGroupRestriction {
		return true
	}
	for _, name := range SpawnAllowedGroups {
		if name == g.Name {
			return true
		}
	}
	member, err := db.FindMemberInGroup(g.ID, spawnerConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"spawn guardrail: membership check: "+err.Error())
		return false
	}
	if member != nil {
		return true
	}
	owner, err := db.IsAgentGroupOwner(g.ID, spawnerConvID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"spawn guardrail: ownership check: "+err.Error())
		return false
	}
	if owner {
		return true
	}
	writeError(w, http.StatusForbidden, "group_restricted",
		fmt.Sprintf("agent %s may only spawn into a group it is a member or owner of, "+
			"and it is neither for group %q. A human can widen this with "+
			"agent.spawn_allowed_groups, or disable it with "+
			"agent.spawn_group_restriction, in ~/.tclaude/config.json.",
			short8(spawnerConvID), g.Name))
	return false
}

// spawnHarnessPolicyFailure enforces the operator's directed cross-harness
// spawn matrix after all profile/default tiers have resolved the target
// harness. Human-initiated spawns have no spawner conv-id and bypass it; the
// matrix exists specifically to constrain agents that otherwise hold
// groups.spawn. Same-harness delegation is never restricted by this feature.
func spawnHarnessPolicyFailure(g *db.AgentGroup, spawnerConvID, targetHarness string) *spawnFailure {
	if spawnerConvID == "" {
		return nil
	}
	sourceHarness := harnessForConv(spawnerConvID).Name
	targetHarness = strings.TrimSpace(targetHarness)
	if targetHarness == "" {
		targetHarness = harness.DefaultName
	}
	if sourceHarness == targetHarness {
		return nil
	}
	var groupID int64
	if g != nil {
		groupID = g.ID
	}
	rule, scope, _, err := db.ResolveSpawnHarnessRule(groupID, sourceHarness, targetHarness)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "spawn harness policy: " + err.Error()}
	}
	if rule.Decision != db.SpawnHarnessDeny {
		return nil
	}
	policy := "global"
	if scope == "group" && g != nil {
		policy = fmt.Sprintf("group %q", g.Name)
	}
	return &spawnFailure{
		http.StatusForbidden,
		"cross_harness_spawn_denied",
		fmt.Sprintf("cross-harness spawn denied by %s policy: %s → %s. Reason: %s",
			policy, sourceHarness, targetHarness, rule.Reason),
	}
}

// spawnHarnessPolicyFailureForGroups applies the effective edge for every
// destination group a clone will join. All must allow: otherwise cloning a
// multi-group agent could bypass one group's deny merely because another group
// explicitly allowed the same edge. An empty slice is the global scope.
func spawnHarnessPolicyFailureForGroups(groups []*db.AgentGroup, spawnerConvID, targetHarness string) *spawnFailure {
	if len(groups) == 0 {
		return spawnHarnessPolicyFailure(nil, spawnerConvID, targetHarness)
	}
	for _, g := range groups {
		if fail := spawnHarnessPolicyFailure(g, spawnerConvID, targetHarness); fail != nil {
			return fail
		}
	}
	return nil
}
