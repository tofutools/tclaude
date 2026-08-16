// Package triggers contains the pure decision layer for tclaude-level events.
// It deliberately owns no database, HTTP, tmux, or spawn handles.
package triggers

import (
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	OutcomeMatched            = "matched"
	OutcomeDeferredDebounce   = "deferred-debounce"
	OutcomeSuppressedCooldown = "suppressed-cooldown"
	OutcomeSuppressedLoop     = "suppressed-loop"
	OutcomeOutOfScope         = "out-of-scope"
	OutcomeDraftFiltered      = "draft-filtered"
	OutcomeDisabled           = "disabled"
	OutcomeRuleTooNew         = "rule-too-new"
)

type Decision struct {
	Fire    bool
	Outcome string
	Detail  string
	DueAt   time.Time
}

// Evaluate decides whether rule should fire for one immutable PR event edge.
// lastFired is the last completed firing for this rule revision family;
// spawnedByRule is supplied by the provenance store for loop protection.
func Evaluate(rule *db.TriggerRule, event db.TriggerPREvent, now time.Time, lastFired time.Time, spawnedByRule bool) Decision {
	if rule == nil || !rule.Enabled {
		return Decision{Outcome: OutcomeDisabled, Detail: "rule is disabled"}
	}
	if rule.Source != event.Source {
		return Decision{Outcome: OutcomeOutOfScope, Detail: "event source does not match rule source"}
	}
	if rule.CreatedAt.After(event.OccurredAt) {
		return Decision{Outcome: OutcomeRuleTooNew, Detail: "rule was installed after this event occurred"}
	}
	if spawnedByRule {
		return Decision{Outcome: OutcomeSuppressedLoop, Detail: "selected agent was spawned by this rule"}
	}
	stateSource := db.IsTriggerStateSource(rule.Source)
	if !stateSource && rule.AuthorIsAgent != nil && !*rule.AuthorIsAgent {
		return Decision{Outcome: OutcomeOutOfScope, Detail: "PR observations are agent-authored"}
	}
	if rule.ScopeKind == db.TriggerScopeGroup && !containsID(event.GroupIDs, rule.GroupID) {
		if stateSource {
			return Decision{Outcome: OutcomeOutOfScope, Detail: fmt.Sprintf("selected agent is not in group %d", rule.GroupID)}
		}
		return Decision{Outcome: OutcomeOutOfScope, Detail: fmt.Sprintf("PR author was not in group %d when the PR was presented", rule.GroupID)}
	}
	if !stateSource {
		switch rule.DraftFilter {
		case db.TriggerDraftExclude:
			if event.Draft {
				return Decision{Outcome: OutcomeDraftFiltered, Detail: "draft PR excluded"}
			}
		case db.TriggerDraftOnly:
			if !event.Draft {
				return Decision{Outcome: OutcomeDraftFiltered, Detail: "non-draft PR excluded"}
			}
		}
	}
	due := event.UpdatedAt.Add(time.Duration(rule.DebounceSeconds) * time.Second)
	if now.Before(due) {
		return Decision{Outcome: OutcomeDeferredDebounce, Detail: "waiting for the trailing-edge debounce window", DueAt: due}
	}
	if rule.CooldownSeconds > 0 && !lastFired.IsZero() {
		cooldownUntil := lastFired.Add(time.Duration(rule.CooldownSeconds) * time.Second)
		if now.Before(cooldownUntil) {
			return Decision{Outcome: OutcomeSuppressedCooldown, Detail: "rule is in cooldown", DueAt: cooldownUntil}
		}
	}
	return Decision{Fire: true, Outcome: OutcomeMatched}
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// RenderTemplate expands the finite, deliberately non-programmable event
// vocabulary shared by spawn and message actions.
func RenderTemplate(raw string, event db.TriggerPREvent, group string) string {
	replacements := []string{
		"{{event.source}}", event.Source,
		"{{event.previous_state}}", event.PreviousState,
		"{{event.current_state}}", event.CurrentState,
		"{{event.fact_result}}", event.FactResult,
		"{{event.fact_observed_at}}", formatTriggerTime(event.FactObservedAt),
		"{{event.dwell_started_at}}", formatTriggerTime(event.DwellStartedAt),
		"{{agent.id}}", event.AgentID,
		"{{agent.harness}}", event.AgentHarness,
		"{{pr.url}}", event.PRURL,
		"{{pr.number}}", fmt.Sprintf("%d", event.PRNumber),
		"{{pr.branch}}", event.PRBranch,
		"{{pr.author_agent}}", event.PRAuthorAgent,
		"{{group}}", group,
	}
	return strings.NewReplacer(replacements...).Replace(raw)
}

func formatTriggerTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
