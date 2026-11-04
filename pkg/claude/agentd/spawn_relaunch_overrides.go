package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// Per-agent relaunch overrides (launch slice 11). A spawn's explicit rows
// and its spawn profiles' rows are resolved once, at the first spawn, into
// the agent's launch layers (AgentLaunchLayers.RelaunchOverrides); every
// relaunch then reads the global table overlaid with them
// (relaunchPolicyFor). Precedence per row, highest first: the spawn's
// explicit row, the first profile tier (named, group default, global
// default) that sets it, the global table.

const (
	relaunchSourceExplicit      = "explicit"
	relaunchSourceProfilePrefix = "profile:"
)

// resolveRelaunchOverrides resolves the rows a spawn records for its agent.
// An explicit "inherit" is recorded too, as an explicit row: it keeps a
// profile's row (now or on a profile re-apply) from reaching the agent, and
// reads as "follow the global table". tiers is highest precedence first;
// nil entries are skipped.
func resolveRelaunchOverrides(explicit map[string]string, tiers []*db.SpawnProfile) []db.RelaunchPolicyOverride {
	var out []db.RelaunchPolicyOverride
	for _, f := range launchspec.Fields {
		if !f.Configurable {
			continue
		}
		if mode, ok := explicit[f.Key]; ok {
			out = append(out, db.RelaunchPolicyOverride{Field: f.Key, Mode: mode, Source: relaunchSourceExplicit})
			continue
		}
		if mode, name, ok := profileRelaunchRow(f.Key, tiers); ok {
			out = append(out, db.RelaunchPolicyOverride{Field: f.Key, Mode: mode, Source: relaunchSourceProfilePrefix + name})
		}
	}
	return out
}

// profileRelaunchRow is the first tier's row for field.
func profileRelaunchRow(field string, tiers []*db.SpawnProfile) (mode, profile string, ok bool) {
	for _, p := range tiers {
		if p == nil {
			continue
		}
		if m, set := p.RelaunchPolicy[field]; set && m != "" {
			return m, p.Name, true
		}
	}
	return "", "", false
}

// gateSpawnRelaunchOverrides enforces who may set a spawn's explicit rows.
// A human may set any. An agent caller may set, freely, any row that applies
// the operator's current settings (inherit, re-resolve, tighten-only,
// chain-wins) or that the agent would inherit anyway; a row that keeps old
// settings (reuse, explicit-wins) where the agent would otherwise get
// something else needs relaunch-policy.manage, since a later tightening of a
// group or profile would then not reach the agent.
func gateSpawnRelaunchOverrides(w http.ResponseWriter, r *http.Request, explicit map[string]string, tiers []*db.SpawnProfile) bool {
	if len(explicit) == 0 || classify(peerFromContext(r.Context())) == classHuman {
		return true
	}
	global := currentRelaunchPolicy()
	var widened []string
	for _, f := range launchspec.Fields {
		mode, ok := explicit[f.Key]
		if !ok || !launchspec.KeepsOldSettings(launchspec.RelaunchMode(mode)) {
			continue
		}
		inherited := string(global.Mode(f.Key))
		if m, _, ok := profileRelaunchRow(f.Key, tiers); ok {
			inherited = m
		}
		if mode != inherited {
			widened = append(widened, fmt.Sprintf("%s=%s", f.Key, mode))
		}
	}
	if len(widened) == 0 {
		return true
	}
	if _, ok := requirePermissionEx(w, r, PermRelaunchPolicyManage); !ok {
		return false
	}
	return true
}

// relaunchPolicySource names where one agent's row comes from: "this
// agent", "profile <name>" or "global".
func relaunchPolicySource(convID, field string) string {
	_, sources := relaunchPolicyFor(convID)
	if s := sources[field]; s != "" {
		return s
	}
	return "global"
}

// relaunchOverridesNote is the resume and reincarnate echo of the rows this
// agent does not take from the global table, with their source, or "" when
// it follows the global table throughout.
func relaunchOverridesNote(convID string) string {
	policy, sources := relaunchPolicyFor(convID)
	var parts []string
	for _, f := range launchspec.Fields {
		if src := sources[f.Key]; f.Configurable && src != "" && src != "global" {
			parts = append(parts, fmt.Sprintf("%s %s (%s)", f.Key, policy.Mode(f.Key), src))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "relaunch policy for this agent: " + strings.Join(parts, ", ") + "; other rows follow the global table"
}

// relaunchPolicyMode is the mode one agent's relaunch uses for field.
func relaunchPolicyMode(convID, field string) launchspec.RelaunchMode {
	p, _ := relaunchPolicyFor(convID)
	return p.Mode(field)
}

// relaunchPolicyFor is the relaunch policy for one agent: the global table
// overlaid with the rows recorded at its spawn. sources names where each
// configurable row comes from: "this agent", "profile <name>" or "global".
// An agent with no recorded rows (or none at all) follows the global table.
func relaunchPolicyFor(convID string) (launchspec.RelaunchPolicy, map[string]string) {
	policy := currentRelaunchPolicy()
	sources := map[string]string{}
	for _, f := range launchspec.Fields {
		if f.Configurable {
			sources[f.Key] = "global"
		}
	}
	if strings.TrimSpace(convID) == "" {
		return policy, sources
	}
	layers, err := db.AgentLaunchLayersForConv(convID)
	if err != nil || layers == nil || len(layers.RelaunchOverrides) == 0 {
		return policy, sources
	}
	rows := map[string]launchspec.RelaunchMode{}
	for _, o := range layers.RelaunchOverrides {
		mode := launchspec.RelaunchMode(o.Mode)
		if mode == launchspec.ModeInherit {
			continue
		}
		f, ok := launchspec.Lookup(o.Field)
		if !ok || !f.Configurable {
			continue
		}
		rows[o.Field] = mode
		if name, ok := strings.CutPrefix(o.Source, relaunchSourceProfilePrefix); ok {
			sources[o.Field] = "profile " + name
		} else {
			sources[o.Field] = "this agent"
		}
	}
	out := policy.WithOverrides(rows)
	for field, mode := range rows {
		if out.Mode(field) != mode {
			sources[field] = "global" // a stored mode the registry no longer offers
		}
	}
	return out, sources
}

// profileRelaunchRowsFailure gates an agent caller's write of a spawn
// profile's relaunch rows, so a profile cannot launder what a spawn's own row
// would need: a row that keeps old settings (reuse, explicit-wins), new or
// changed by this write, needs relaunch-policy.manage. A row the profile
// already carried, and every other mode, is free.
func profileRelaunchRowsFailure(caller string, before, after *db.SpawnProfile, why string) *spawnFailure {
	var added []string
	for _, f := range launchspec.Fields {
		mode, ok := after.RelaunchPolicy[f.Key]
		if !ok || !launchspec.KeepsOldSettings(launchspec.RelaunchMode(mode)) {
			continue
		}
		if before != nil && before.RelaunchPolicy[f.Key] == mode {
			continue
		}
		added = append(added, fmt.Sprintf("%s=%s", f.Key, mode))
	}
	if len(added) == 0 || standingPermissionAllows(caller, PermRelaunchPolicyManage, ActionContext{}) {
		return nil
	}
	return &spawnFailure{http.StatusForbidden, "forbidden", fmt.Sprintf(
		"profile %q sets relaunch policy rows that keep old settings on its agents' relaunches (%s), which requires %s (%s)",
		after.Name, strings.Join(added, ", "), PermRelaunchPolicyManage, why)}
}
