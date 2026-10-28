package agentd

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usageBudgetsView is the snapshot's compact view of usage profiles for the
// inline controls: the dock's profile list, the toolbar and group-row 💰
// pickers, and each agent row's budget badge. The full editor reads
// /api/usage/rules instead.
type usageBudgetsView struct {
	Profiles []usageBudgetProfile `json:"profiles"`
	// Global names the global default usage profiles.
	Global []string `json:"global"`
	// Groups maps a group id to its usage profiles.
	Groups map[string][]string `json:"groups"`
	// Agents maps an agent id to its tightest budget reading.
	Agents map[string]usageAgentBudget `json:"agents"`
}

type usageBudgetProfile struct {
	ID    int64             `json:"id"`
	Name  string            `json:"name"`
	Descr string            `json:"descr,omitempty"`
	Rules []usagerules.Rule `json:"rules"`
}

// usageAgentBudget is one agent's highest-percentage rule, for its row badge.
type usageAgentBudget struct {
	Name    string            `json:"name"`
	Profile string            `json:"profile,omitempty"`
	Layer   string            `json:"layer,omitempty"`
	Metric  usagerules.Metric `json:"metric"`
	Value   float64           `json:"value"`
	Budget  float64           `json:"budget"`
	Pct     float64           `json:"pct"`
	Reached usagerules.Action `json:"reached,omitempty"`
	Paused  string            `json:"paused,omitempty"`
	DryRun  bool              `json:"dry_run,omitempty"`
	Rules   int               `json:"rules"`
}

// collectUsageBudgetsView reads the profiles and their global and group
// assignments, and condenses the latest per-agent readings. nil when there
// is nothing to show or the read failed.
func collectUsageBudgetsView() *usageBudgetsView {
	profiles, err := db.ListUsageProfiles()
	if err != nil {
		slog.Warn("usage rules: snapshot profiles", "error", err)
		return nil
	}
	if len(profiles) == 0 {
		return nil
	}
	out := &usageBudgetsView{Profiles: make([]usageBudgetProfile, 0, len(profiles)), Global: []string{},
		Groups: map[string][]string{}, Agents: map[string]usageAgentBudget{}}
	names := map[int64]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
		out.Profiles = append(out.Profiles, usageBudgetProfile{ID: p.ID, Name: p.Name, Descr: p.Descr, Rules: p.Rules})
	}
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		slog.Warn("usage rules: snapshot assignments", "error", err)
		return out
	}
	for _, a := range assignments {
		name := names[a.ProfileID]
		if name == "" {
			continue
		}
		switch a.Scope {
		case db.UsageScopeGlobal:
			out.Global = append(out.Global, name)
		case db.UsageScopeGroup:
			out.Groups[a.ScopeID] = append(out.Groups[a.ScopeID], name)
		}
	}
	statuses, _ := latestUsageRuleStatus()
	for _, s := range statuses {
		if s.Scope != string(usagerules.ScopeAgent) || s.AgentID == "" {
			continue
		}
		cur, seen := out.Agents[s.AgentID]
		n := cur.Rules + 1
		if !seen || s.Pct > cur.Pct {
			cur = usageAgentBudget{Name: s.Name, Profile: s.Profile, Layer: s.Layer, Metric: s.Metric, Value: s.Value,
				Budget: s.Budget, Pct: s.Pct, Reached: s.Reached, Paused: s.Paused, DryRun: s.DryRun}
		}
		cur.Rules = n
		out.Agents[s.AgentID] = cur
	}
	return out
}
