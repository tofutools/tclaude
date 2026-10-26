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
	group string
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
	for _, l := range limits {
		if !l.Enabled || l.Rule.DryRun || !usageRuleHasAction(l.Rule, usagerules.ActionBlockSpawn) {
			continue
		}
		if l.Scope == db.UsageScopeGroup && l.GroupID != groupID {
			continue
		}
		firings, err := db.ListUsageRuleFiringsForSubject(usageLimitRuleKey(l.ID), usageLimitSubject(l))
		if err != nil {
			return nil, err
		}
		for _, f := range firings {
			if f.Action == string(usagerules.ActionBlockSpawn) && !f.DryRun && usageFiringInCurrentPeriod(l.Rule, f.PeriodKey, now) {
				out = append(out, usageSpawnBlock{limit: l})
				break
			}
		}
	}
	return out, nil
}

func usageRuleHasAction(r usagerules.Rule, a usagerules.Action) bool {
	for _, s := range r.Steps {
		if s.Action == a {
			return true
		}
	}
	return false
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
			"(dashboard Config → Usage, or `tclaude usage limits`) to spawn now", scope, l.Rule.Name, usagePeriodNoun(l.Rule.Period)),
	}
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
