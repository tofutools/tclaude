package agentd

import (
	"encoding/json"
	"log/slog"
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

func resolveSpawnCopyOutside(agentID string, g *db.AgentGroup, harnessName string, oldCopy *db.AgentSpawnCopy,
	newCopy db.AgentSpawnCopy, explicit map[string]db.ExplicitLaunchValue, startup *db.AgentStartupSnapshot) (db.SpawnCopyOutside, *spawnFailure) {
	var out db.SpawnCopyOutside
	next, fail := decodeSpawnCopyTiers(newCopy)
	if fail != nil {
		return out, fail
	}
	tiers := next.launchTiers()
	h, err := harness.Resolve(harnessOrDefault(harnessName))
	if err != nil {
		return out, &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	isExplicit := func(key string) bool { return explicit[key].Provenance != "" }

	if startup != nil && !isExplicit("include_group_context") {
		var res launchBoolResolution
		if _, fail := resolveLaunchBoolFields(h, tiers, &res, launchBoolIncludeGroupContext); fail != nil {
			return out, fail
		}
		include := !res.Set[launchBoolIncludeGroupContext] || res.Value[launchBoolIncludeGroupContext]
		// Stored as a first spawn stores it (recordStartupSnapshot): a group
		// with no standing context counts as included.
		include = strings.TrimSpace(g.GetDefaultContext()) == "" || include
		if include != startup.IncludeGroupContext {
			out.IncludeGroupContext = &include
		}
	}

	if startup != nil && oldCopy != nil {
		prev, fail := decodeSpawnCopyTiers(*oldCopy)
		if fail != nil {
			return out, fail
		}
		was, _ := resolveProfileStartupContext(harnessName, prev.launchTiers())
		now, _ := resolveProfileStartupContext(harnessName, tiers)
		if was != now {
			stored := strings.TrimSpace(startup.ProfileContext)
			if strings.HasPrefix(stored, was) {
				combined := joinContextParts(now, strings.TrimSpace(stored[len(was):]))
				out.ProfileContext = &combined
			} else {
				// Something other than this copy wrote the stored text; only
				// the profile's own part is ours to replace, and it is not
				// there to find.
				slog.Info("re-apply: stored startup context does not start with the copy's; keeping it", "agent", agentID)
			}
		}
	}

	// The explicit entries stay on top of whatever the copy's tiers now give,
	// as they did at the first spawn.
	env, fail := resolveSpawnCopyEnvironment(agentID, g, tiers, explicit["profile_env"])
	if fail != nil {
		return out, fail
	}
	if env != nil {
		out.EffectiveSandbox, out.EffectiveSandboxRaw = env.snapshot, env.raw
	}

	if !isExplicit("usage_profile_links") {
		ids, source, changed, err := spawnCopyUsageLinks(agentID, next)
		if err != nil {
			return out, &spawnFailure{http.StatusInternalServerError, "io", "read usage profile links: " + err.Error()}
		}
		if changed {
			out.UsageLinkedSet, out.UsageLinkedIDs, out.UsageLinkedSource = true, ids, source
		}
	}
	return out, nil
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
// explicit entries, or nil when the agent has no launch environment to
// change or it would not change.
func resolveSpawnCopyEnvironment(agentID string, g *db.AgentGroup, tiers []launchProfileTier, explicit db.ExplicitLaunchValue) (*spawnCopyEnvironment, *spawnFailure) {
	rec, err := db.ActiveAgentLaunchRecord(agentID)
	if err != nil {
		return nil, &spawnFailure{http.StatusInternalServerError, "io", "load the agent's launch snapshot: " + err.Error()}
	}
	if rec == nil || rec.EffectiveSandbox == nil || !rec.EffectiveSandbox.RefreshGroupEnvironment {
		// No resolved launch environment to replace (an agent whose spawn
		// path recorded none keeps what it has).
		return nil, nil
	}
	var explicitEnv []sandboxpolicy.EnvironmentEntry
	if raw, ok := explicit.Fields["Environment"]; ok {
		if err := json.Unmarshal(raw, &explicitEnv); err != nil {
			return nil, &spawnFailure{http.StatusConflict, "invalid_launch_layers", "the spawn's explicit environment cannot be read: " + err.Error()}
		}
	}
	environment, overrides, err := resolveCommonLaunchEnvironmentParts(g, tiers, explicitEnv)
	if err != nil {
		return nil, &spawnFailure{http.StatusBadRequest, "invalid_environment", err.Error()}
	}
	current := rec.EffectiveSandbox
	if sameEnvironment(overrides, current.LaunchEnvironmentOverrides) {
		return nil, nil
	}
	next := *current
	next.LaunchEnvironment, next.LaunchEnvironmentOverrides = environment, overrides
	return &spawnCopyEnvironment{snapshot: &next, raw: rec.EffectiveSandboxRaw}, nil
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
