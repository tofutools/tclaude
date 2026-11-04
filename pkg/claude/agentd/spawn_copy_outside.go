package agentd

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Re-applying the spawn-copy fields that live outside the relaunch profile
// (launch slice 6f, tcl-g2q5wz). A first spawn resolves each from the profile
// tiers and the spawn request and stores it where it is read later:
//
//   - include_group_context: the startup snapshot's include_group_ctx, which
//     re-injection after compaction or /clear reads;
//   - profile_startup_context: the profile part of the startup snapshot's
//     profile_context (the role briefs that follow it are left as they are);
//   - profile_env: the launch snapshot's environment overrides, the profile
//     tiers then the explicit entries, which every resume merges over the
//     group's current environment;
//   - usage_profile_links: the agent's usage-profile links that came from a
//     spawn profile (the ones the spawn named itself are its own choice).
//
// resolveSpawnCopyOutside resolves them from the new copy the way the first
// spawn did, keeping every value the spawn request set explicitly, and says
// what to write; db.ApplyAgentSpawnCopy writes it with the copy. A value equal
// to the stored one is not written, so re-applying the same copy changes
// nothing. Roles move with their grants (tcl-fg2kqk) and are not touched.

// spawnCopyKept is a field the re-apply could not write outside the relaunch
// profile, with why. Its old value stays in the copy written, so the preview
// keeps listing the difference instead of hiding it.
type spawnCopyKept struct {
	key, why string
}

func resolveSpawnCopyOutside(agentID string, g *db.AgentGroup, harnessName string, oldCopy *db.AgentSpawnCopy,
	newCopy db.AgentSpawnCopy, explicit map[string]db.ExplicitLaunchValue, startup *db.AgentStartupSnapshot,
	reapplierConv string) (db.SpawnCopyOutside, []spawnCopyKept, *spawnFailure) {
	var out db.SpawnCopyOutside
	var kept []spawnCopyKept
	next, fail := decodeSpawnCopyTiers(newCopy)
	if fail != nil {
		return out, nil, fail
	}
	tiers := next.launchTiers()
	var prevTiers []launchProfileTier
	if oldCopy != nil {
		prev, fail := decodeSpawnCopyTiers(*oldCopy)
		if fail != nil {
			return out, nil, fail
		}
		prevTiers = prev.launchTiers()
	}
	h, err := harness.Resolve(harnessOrDefault(harnessName))
	if err != nil {
		return out, nil, &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	isExplicit := func(key string) bool { return explicit[key].Provenance != "" }

	// include_group_context: only a change the copy decides is written. What
	// an unset value means depends on the spawn path (a direct spawn includes,
	// a one-shot or scribe does not), so a copy that stops setting it keeps
	// the stored value; so does any case the copy did not change.
	if startup != nil && !isExplicit("include_group_context") {
		was, fail := resolveIncludeGroupContext(h, prevTiers)
		if fail != nil {
			return out, nil, fail
		}
		now, fail := resolveIncludeGroupContext(h, tiers)
		if fail != nil {
			return out, nil, fail
		}
		switch {
		case was == now:
		case now.set:
			if now.value != startup.IncludeGroupContext {
				out.IncludeGroupContext = &now.value
			}
		default:
			kept = append(kept, spawnCopyKept{"include_group_context",
				"the profile no longer sets it, and what unset means depends on how the agent was spawned"})
		}
	}

	if startup != nil {
		was, _ := resolveProfileStartupContext(harnessName, prevTiers)
		now, _ := resolveProfileStartupContext(harnessName, tiers)
		if was != now {
			stored := strings.TrimSpace(startup.ProfileContext)
			if strings.HasPrefix(stored, was) {
				combined := joinContextParts(now, strings.TrimSpace(stored[len(was):]))
				out.ProfileContext = &combined
			} else {
				kept = append(kept, spawnCopyKept{"profile_startup_context",
					"the agent's stored startup context does not start with the profile's old text"})
			}
		}
	} else {
		kept = append(kept, spawnCopyKept{"profile_startup_context", "the agent has no startup record"})
	}

	// The explicit entries stay on top of whatever the copy's tiers now give,
	// as they did at the first spawn.
	env, why, fail := resolveSpawnCopyEnvironment(agentID, g, tiers, prevTiers, explicit["profile_env"])
	if fail != nil {
		return out, nil, fail
	}
	switch {
	case why != "":
		kept = append(kept, spawnCopyKept{"profile_env", why})
	case env != nil && reapplierConv != "":
		// An environment reaches the agent's next launch: model endpoints,
		// proxies, tool hooks. Morphing an agent cannot change it, so neither
		// can an agent re-applying a profile to another one.
		return out, nil, &spawnFailure{http.StatusForbidden, "environment_operator_only",
			"this re-apply would change the agent's environment, which only the operator can re-apply; ask them to apply it"}
	case env != nil:
		out.EffectiveSandbox, out.EffectiveSandboxRaw = env.snapshot, env.raw
	}

	if !isExplicit("usage_profile_links") {
		ids, source, changed, err := spawnCopyUsageLinks(agentID, next)
		if err != nil {
			return out, nil, &spawnFailure{http.StatusInternalServerError, "io", "read usage profile links: " + err.Error()}
		}
		if changed {
			out.UsageLinkedSet, out.UsageLinkedIDs, out.UsageLinkedSource = true, ids, source
		}
	}
	return out, kept, nil
}

type includeGroupContextResolution struct {
	value, set bool
}

func resolveIncludeGroupContext(h *harness.Harness, tiers []launchProfileTier) (includeGroupContextResolution, *spawnFailure) {
	var res launchBoolResolution
	if _, fail := resolveLaunchBoolFields(h, tiers, &res, launchBoolIncludeGroupContext); fail != nil {
		return includeGroupContextResolution{}, fail
	}
	return includeGroupContextResolution{value: res.Value[launchBoolIncludeGroupContext], set: res.Set[launchBoolIncludeGroupContext]}, nil
}

// joinContextParts joins non-empty blocks the way appendRoleBlock does.
func joinContextParts(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n")
}

type spawnCopyEnvironment struct {
	snapshot *sandboxpolicy.Snapshot
	raw      string
}

// resolveSpawnCopyEnvironment is the agent's launch snapshot with its
// environment overrides recomputed from the copy's tiers and the spawn's
// explicit entries, or nil when they would not change. why, when set, says
// why a change the copy makes cannot be written (the agent has no resolved
// launch environment to replace).
func resolveSpawnCopyEnvironment(agentID string, g *db.AgentGroup, tiers, prevTiers []launchProfileTier,
	explicit db.ExplicitLaunchValue) (env *spawnCopyEnvironment, why string, fail *spawnFailure) {
	var explicitEnv []sandboxpolicy.EnvironmentEntry
	if raw, ok := explicit.Fields["Environment"]; ok {
		if err := json.Unmarshal(raw, &explicitEnv); err != nil {
			return nil, "", &spawnFailure{http.StatusConflict, "invalid_launch_layers", "the spawn's explicit environment cannot be read: " + err.Error()}
		}
	}
	environment, overrides, err := resolveCommonLaunchEnvironmentParts(g, tiers, explicitEnv)
	if err != nil {
		return nil, "", &spawnFailure{http.StatusBadRequest, "invalid_environment", err.Error()}
	}
	current, raw, err := db.AgentEffectiveSandboxConfigRaw(agentID)
	if err != nil {
		return nil, "", &spawnFailure{http.StatusInternalServerError, "io", "load the agent's launch snapshot: " + err.Error()}
	}
	if current == nil || !current.RefreshGroupEnvironment {
		// No resolved launch environment to replace. Only a change the copy
		// makes is worth saying so for.
		_, prevOverrides, err := resolveCommonLaunchEnvironmentParts(g, prevTiers, explicitEnv)
		if err == nil && sameEnvironment(overrides, prevOverrides) {
			return nil, "", nil
		}
		return nil, "the agent's launch record has no resolved environment to update", nil
	}
	if sameEnvironment(overrides, current.LaunchEnvironmentOverrides) {
		return nil, "", nil
	}
	next := *current
	next.LaunchEnvironment, next.LaunchEnvironmentOverrides = environment, overrides
	return &spawnCopyEnvironment{snapshot: &next, raw: raw}, "", nil
}

func sameEnvironment(a, b []sandboxpolicy.EnvironmentEntry) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// spawnCopyUsageLinks are the usage profiles the copy links the agent to, as a
// first spawn picks them (applySpawnUsageProfiles): those of the first copied
// profile with any, named then group default then global default, read live
// from the usage rules store. changed reports whether they differ from the
// agent's current linked set.
func spawnCopyUsageLinks(agentID string, d decodedSpawnCopy) (ids []int64, source string, changed bool, err error) {
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return nil, "", false, err
	}
	for _, prof := range []*db.SpawnProfile{d.caller, d.slots.group, d.slots.global} {
		if prof == nil || prof.ID == 0 {
			continue
		}
		scopeID := strconv.FormatInt(prof.ID, 10)
		for _, a := range assignments {
			if a.Scope == db.UsageScopeSpawnProfile && a.ScopeID == scopeID {
				ids = append(ids, a.ProfileID)
			}
		}
		if len(ids) > 0 {
			source = usageLinkedSourcePrefix + prof.Name
			break
		}
	}
	var have []int64
	haveSource := ""
	for _, a := range assignments {
		if a.Scope == db.UsageScopeAgent && a.ScopeID == agentID && strings.HasPrefix(a.Source, usageLinkedSourcePrefix) {
			have = append(have, a.ProfileID)
			haveSource = a.Source
		}
	}
	slices.Sort(ids)
	slices.Sort(have)
	changed = !slices.Equal(ids, have) || (len(ids) > 0 && source != haveSource)
	return ids, source, changed, nil
}

// keepCopyFields is newCopy with the given fields' values taken back from the
// old copy, tier by tier (matched by tier and profile ID), so the stored copy
// says what the agent really runs with.
func keepCopyFields(newCopy db.AgentSpawnCopy, oldCopy *db.AgentSpawnCopy, kept []spawnCopyKept) db.AgentSpawnCopy {
	if oldCopy == nil || len(kept) == 0 {
		return newCopy
	}
	keys := map[string]bool{}
	for _, k := range kept {
		keys[k.key] = true
	}
	keep := func(jsonKey string) bool {
		f, ok := launchFieldForProfileField(spawnProfileJSONGoNames[jsonKey])
		return ok && keys[f.Key]
	}
	out := db.AgentSpawnCopy{CopiedAt: newCopy.CopiedAt, Tiers: append([]db.SpawnCopyTier(nil), newCopy.Tiers...)}
	for i, tier := range out.Tiers {
		var old *db.SpawnCopyTier
		for j := range oldCopy.Tiers {
			if oldCopy.Tiers[j].Tier == tier.Tier && oldCopy.Tiers[j].ProfileID == tier.ProfileID {
				old = &oldCopy.Tiers[j]
				break
			}
		}
		merged, copied := profileJSONMap(tier.Profile), map[string]json.RawMessage{}
		if old != nil {
			copied = profileJSONMap(old.Profile)
		}
		if merged == nil {
			continue
		}
		for key := range merged {
			if keep(key) {
				delete(merged, key)
			}
		}
		for key, raw := range copied {
			if keep(key) {
				merged[key] = raw
			}
		}
		if raw, err := json.Marshal(merged); err == nil {
			out.Tiers[i].Profile = raw
		}
	}
	return out
}
