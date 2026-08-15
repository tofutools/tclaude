package triggers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestEvaluatePROpenedControls(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	rule := &db.TriggerRule{Enabled: true, ScopeKind: db.TriggerScopeGroup, GroupID: 7,
		DraftFilter: db.TriggerDraftExclude, DebounceSeconds: 10, CooldownSeconds: 60,
		CreatedAt: now.Add(-time.Hour)}
	event := db.TriggerPREvent{GroupIDs: []int64{7}, OccurredAt: now.Add(-time.Minute), UpdatedAt: now.Add(-5 * time.Second)}
	d := Evaluate(rule, event, now, time.Time{}, false)
	assert.Equal(t, OutcomeDeferredDebounce, d.Outcome)
	event.UpdatedAt = now.Add(-time.Minute)
	d = Evaluate(rule, event, now, now.Add(-30*time.Second), false)
	assert.Equal(t, OutcomeSuppressedCooldown, d.Outcome)
	d = Evaluate(rule, event, now, time.Time{}, true)
	assert.Equal(t, OutcomeSuppressedLoop, d.Outcome)
	d = Evaluate(rule, event, now, time.Time{}, false)
	require.True(t, d.Fire)
}

func TestRenderTemplate(t *testing.T) {
	e := db.TriggerPREvent{PRURL: "https://github.com/o/r/pull/42", PRNumber: 42, PRBranch: "topic", PRAuthorAgent: "agt_a"}
	assert.Equal(t, "review https://github.com/o/r/pull/42 #42 topic agt_a team",
		RenderTemplate("review {{pr.url}} #{{pr.number}} {{pr.branch}} {{pr.author_agent}} {{group}}", e, "team"))
}
