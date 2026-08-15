package triggers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlanDwellUnknownBreaksContinuousTruthWithoutRearming(t *testing.T) {
	t0 := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	first := PlanDwell(nil, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactTrue, Now: t0})
	require.False(t, first.Fire)
	assert.Equal(t, t0, first.State.TrueSince)

	unknown := PlanDwell(&first.State, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactUnknown, Now: t0.Add(8 * time.Minute)})
	assert.True(t, unknown.State.TrueSince.IsZero(), "unknown must break continuous truth")

	resumed := PlanDwell(&unknown.State, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactTrue, FactSince: t0, Now: t0.Add(9 * time.Minute)})
	assert.Equal(t, t0.Add(9*time.Minute), resumed.State.TrueSince,
		"a true observation after unknown starts a fresh clock, never the stale fact anchor")
	assert.False(t, resumed.Fire)
	mature := PlanDwell(&resumed.State, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactTrue, Now: t0.Add(19 * time.Minute)})
	assert.True(t, mature.Fire)

	afterUnknown := PlanDwell(&mature.State, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactUnknown, Now: t0.Add(20 * time.Minute)})
	afterTrue := PlanDwell(&afterUnknown.State, DwellInput{RuleRevision: 1, For: 10 * time.Minute, Result: FactTrue, Now: t0.Add(40 * time.Minute)})
	assert.False(t, afterTrue.Fire, "unknown does not re-arm an already-fired episode")
	assert.False(t, afterTrue.State.FiredAt.IsZero())
}

func TestPlanDwellFalseRearmsAndRestartStateDoesNotRefire(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	mature := PlanDwell(nil, DwellInput{RuleRevision: 3, For: time.Minute, Result: FactTrue,
		FactSince: now.Add(-2 * time.Minute), Now: now})
	require.True(t, mature.Fire)

	restart := PlanDwell(&mature.State, DwellInput{RuleRevision: 3, For: time.Minute, Result: FactTrue, Now: now.Add(time.Hour)})
	assert.False(t, restart.Fire)

	falsePlan := PlanDwell(&restart.State, DwellInput{RuleRevision: 3, For: time.Minute, Result: FactFalse, Now: now.Add(2 * time.Hour)})
	assert.True(t, falsePlan.State.FiredAt.IsZero())
	rearmed := PlanDwell(&falsePlan.State, DwellInput{RuleRevision: 3, For: time.Minute, Result: FactTrue, Now: now.Add(2*time.Hour + time.Minute)})
	assert.False(t, rearmed.Fire)
	assert.Equal(t, mature.State.Episode+1, rearmed.State.Episode)
}

func TestPlanDwellRuleEditDoesNotRearmAnEpisode(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	mature := PlanDwell(nil, DwellInput{RuleRevision: 1, For: time.Minute, Result: FactTrue,
		FactSince: now.Add(-2 * time.Minute), Now: now})
	require.True(t, mature.Fire)

	edited := PlanDwell(&mature.State, DwellInput{RuleRevision: 2, For: 30 * time.Second,
		Result: FactTrue, Now: now.Add(time.Hour)})
	assert.False(t, edited.Fire, "only observed false, not a rule edit, re-arms an episode")
	assert.Equal(t, int64(2), edited.State.RuleRevision)
	assert.Equal(t, mature.State.FiredAt, edited.State.FiredAt)
}
