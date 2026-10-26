package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usageSpawnBlock is a limit currently refusing spawns.
type usageSpawnBlock struct {
	limit *db.UsageLimit
}

// activeUsageSpawnBlocks lists the enabled limits whose block_spawn step has
// fired in the current period and that cover a spawn into groupID (0 for a
// spawn into no group: only global limits cover it). A block lifts by itself
// when the period rolls over, and at once when the operator raises the budget
// or disables or deletes the limit, because the firing is keyed by period and
// budget. Dry-run firings never block.
func activeUsageSpawnBlocks(groupID int64, now time.Time) ([]usageSpawnBlock, error) {
	limits, err := db.ListUsageLimits()
	if err != nil {
		return nil, err
	}
	var out []usageSpawnBlock
	statuses, _ := latestUsageRuleStatus()
	for _, l := range limits {
		if !l.Enabled || l.Rule.DryRun {
			continue
		}
		if l.Scope == db.UsageScopeGroup && l.GroupID != groupID {
			continue
		}
		step := usageBlockStep(l.Rule)
		if step == nil {
			continue
		}
		// The firing proves the step was crossed this period; the latest
		// reading can still show the value back under it (a member left a
		// group), and then the block no longer holds.
		if s := usageLimitStatus(statuses, l.ID); s != nil && s.Pct < step.At {
			continue
		}
		periods, err := db.UsageRuleFiringPeriods(usageLimitRuleKey(l.ID), usageLimitSubject(l), step.Key())
		if err != nil {
			return nil, err
		}
		for _, period := range periods {
			if usageFiringInCurrentPeriod(l.Rule, period, now) {
				out = append(out, usageSpawnBlock{limit: l})
				break
			}
		}
	}
	return out, nil
}

// usageBlockStep is the rule's block_spawn step, or nil. Validation allows
// at most one step per action and threshold; the lowest blocking step wins.
func usageBlockStep(r usagerules.Rule) *usagerules.Step {
	for i := range r.Steps {
		if r.Steps[i].Action == usagerules.ActionBlockSpawn {
			return &r.Steps[i]
		}
	}
	return nil
}

func usageLimitStatus(statuses []usageRuleStatus, limitID int64) *usageRuleStatus {
	for i := range statuses {
		if statuses[i].LimitID == limitID && statuses[i].Scope != string(usagerules.ScopeAgent) {
			return &statuses[i]
		}
	}
	return nil
}

// usageFiringInCurrentPeriod reports whether a firing's period key names the
// span containing now at the rule's current budget. A quota window span is
// keyed by its reset time, so it is current until that time passes.
func usageFiringInCurrentPeriod(r usagerules.Rule, periodKey string, now time.Time) bool {
	budget := "|b=" + formatUsageBudget(r.Budget)
	span, ok := strings.CutSuffix(periodKey, budget)
	if !ok {
		return false
	}
	if r.Period == usagerules.PeriodWindow {
		resets, err := time.Parse(time.RFC3339, span)
		return err == nil && resets.After(now)
	}
	return span == usagerules.PeriodKey(r.Period, now, time.Time{})
}

// checkUsageSpawnBlock refuses a spawn into g while a limit blocks it. It
// binds every caller, the human included: lifting it is an explicit act
// (raise the budget, or disable the limit).
func checkUsageSpawnBlock(g *db.AgentGroup) *spawnFailure {
	var groupID int64
	if g != nil {
		groupID = g.ID
	}
	blocks, err := activeUsageSpawnBlocks(groupID, time.Now())
	if err != nil {
		// Fail open: a broken read must not wedge every spawn.
		slog.Warn("usage rules: read spawn blocks", "error", err)
		return nil
	}
	if len(blocks) == 0 {
		return nil
	}
	l := blocks[0].limit
	scope := "the global usage limit"
	if l.Scope == db.UsageScopeGroup && g != nil {
		scope = fmt.Sprintf("group %q's usage limit", g.Name)
	}
	return &spawnFailure{
		Status: http.StatusConflict,
		Kind:   "usage_limit_reached",
		Msg: fmt.Sprintf("%s %q blocks new spawns until its %s rolls over; raise its budget or disable it "+
			"(dashboard Usage limits & budgets, or `tclaude usage limit set`) to spawn now", scope, l.Rule.Name, usagePeriodNoun(l.Rule.Period)),
	}
}

// checkUsageSpawnBlockForGroups refuses a new agent joining groups (none:
// only global limits apply) while a limit blocks any of them.
func checkUsageSpawnBlockForGroups(groups []*db.AgentGroup) *spawnFailure {
	if len(groups) == 0 {
		return checkUsageSpawnBlock(nil)
	}
	for _, g := range groups {
		if fail := checkUsageSpawnBlock(g); fail != nil {
			return fail
		}
	}
	return nil
}

// copyAgentUsageAssignments gives a clone its source's agent-layer usage
// profiles, so cloning never sheds a budget set on the agent itself.
func copyAgentUsageAssignments(sourceConv, cloneAgentID string) error {
	sourceID, err := db.AgentIDForConv(sourceConv)
	if err != nil || sourceID == "" {
		return err
	}
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return err
	}
	// Own and linked assignments are separate sets; copy each with its source.
	type set struct {
		source string
		ids    []int64
	}
	var own, linked set
	for _, a := range assignments {
		if a.Scope != db.UsageScopeAgent || a.ScopeID != sourceID {
			continue
		}
		s := &own
		if strings.HasPrefix(a.Source, db.UsageLinkedSourcePrefix) {
			s = &linked
		}
		if len(s.ids) == 0 {
			s.source = a.Source
		}
		s.ids = append(s.ids, a.ProfileID)
	}
	for _, s := range []set{own, linked} {
		if len(s.ids) == 0 {
			continue
		}
		if err := db.SetUsageProfileAssignments(db.UsageScopeAgent, cloneAgentID, s.ids, s.source); err != nil {
			return err
		}
	}
	return nil
}

func usagePeriodNoun(p usagerules.Period) string {
	switch p {
	case usagerules.PeriodDay:
		return "day"
	case usagerules.PeriodWeek:
		return "week"
	case usagerules.PeriodMonth:
		return "month"
	case usagerules.PeriodWindow:
		return "quota window"
	default:
		return "period"
	}
}
