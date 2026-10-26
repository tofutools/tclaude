package usagerules

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func agentRule() Rule {
	return Rule{Name: "budget", Period: PeriodLifetime, Budget: 20, Steps: []Step{
		{At: 100, Action: ActionWrapUp},
		{At: 50, Notify: []Target{TargetAgent}},
		{At: 80, Notify: []Target{TargetAgent, TargetOperator, TargetAgent}, RepeatEveryPct: 5},
		{At: 110, Action: ActionHardStop},
	}}
}

func TestNormalizeAndValidate(t *testing.T) {
	r := agentRule()
	r.Normalize()
	require.Equal(t, MetricTotalUSD, r.Metric)
	require.Equal(t, []float64{50, 80, 100, 110}, []float64{r.Steps[0].At, r.Steps[1].At, r.Steps[2].At, r.Steps[3].At})
	require.Equal(t, ActionNotify, r.Steps[0].Action)
	require.Equal(t, []Target{TargetAgent, TargetOperator}, r.Steps[1].Notify)
	require.NoError(t, r.Validate(ScopeAgent))
	require.ErrorContains(t, r.Validate(ScopeGlobal), "lifetime")

	w := Rule{Name: "5h", Metric: MetricWindowPct, Window: "anthropic:five_hour", Steps: []Step{{At: 90, Action: ActionBlockSpawn}}}
	w.Normalize()
	require.Equal(t, PeriodWindow, w.Period)
	require.Equal(t, 100.0, w.Budget)
	require.NoError(t, w.Validate(ScopeGlobal))
	require.ErrorContains(t, w.Validate(ScopeGroup), "account-wide")
	require.ErrorContains(t, w.Validate(ScopeAgent), "account-wide")

	b := Rule{Name: "x", Period: PeriodDay, Budget: 5, Steps: []Step{{At: 50, Action: ActionBlockSpawn}}}
	b.Normalize()
	require.ErrorContains(t, b.Validate(ScopeAgent), "block_spawn")
	require.NoError(t, b.Validate(ScopeGroup))

	for name, bad := range map[string]Rule{
		"no steps":     {Name: "a", Period: PeriodDay, Budget: 1},
		"zero budget":  {Name: "a", Period: PeriodDay, Steps: []Step{{At: 1, Notify: []Target{TargetOperator}}}},
		"no target":    {Name: "a", Period: PeriodDay, Budget: 1, Steps: []Step{{At: 1}}},
		"bad name":     {Name: "", Period: PeriodDay, Budget: 1, Steps: []Step{{At: 1, Notify: []Target{TargetOperator}}}},
		"dup step":     {Name: "a", Period: PeriodDay, Budget: 1, Steps: []Step{{At: 1, Notify: []Target{TargetOperator}}, {At: 1, Notify: []Target{TargetAgent}}}},
		"short repeat": {Name: "a", Period: PeriodDay, Budget: 1, Steps: []Step{{At: 1, Notify: []Target{TargetOperator}, RepeatEveryMinutes: 1}}},
		"bad period":   {Name: "a", Period: "year", Budget: 1, Steps: []Step{{At: 1, Notify: []Target{TargetOperator}}}},
	} {
		bad.Normalize()
		require.Error(t, bad.Validate(ScopeAgent), name)
	}
}

func TestValidateProfileRulesRejectsDuplicateNames(t *testing.T) {
	_, err := ValidateProfileRules([]Rule{agentRule(), func() Rule { r := agentRule(); r.Name = "Budget"; return r }()})
	require.ErrorContains(t, err, "duplicate rule name")
}

func TestPeriodKeys(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local) // Wednesday
	require.Equal(t, "2026-10-28", PeriodKey(PeriodDay, now, time.Time{}))
	require.Equal(t, "2026-W44", PeriodKey(PeriodWeek, now, time.Time{}))
	require.Equal(t, "2026-10", PeriodKey(PeriodMonth, now, time.Time{}))
	require.Equal(t, "lifetime", PeriodKey(PeriodLifetime, now, time.Time{}))
	reset := time.Date(2026, 10, 28, 18, 0, 31, 0, time.UTC)
	require.Equal(t, "2026-10-28T18:00:00Z", PeriodKey(PeriodWindow, now, reset))
	require.Equal(t, "2026-10-26", PeriodStartDay(PeriodWeek, now))
	require.Equal(t, "2026-10-01", PeriodStartDay(PeriodMonth, now))
	require.Equal(t, "", PeriodStartDay(PeriodLifetime, now))
	sunday := time.Date(2026, 11, 1, 9, 0, 0, 0, time.Local)
	require.Equal(t, "2026-10-26", PeriodStartDay(PeriodWeek, sunday))
}

func keys(fs []Firing) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Key)
	}
	return out
}

func TestDue(t *testing.T) {
	r := agentRule()
	r.Normalize()
	now := time.Now()
	require.Empty(t, Due(r, 49.9, now, nil))
	require.Equal(t, []string{"notify@50"}, keys(Due(r, 50, now, nil)))
	require.Equal(t, []string{"notify@50", "notify@80"}, keys(Due(r, 84, now, nil)))
	// A jump lists only the newest percentage repeat.
	require.Equal(t, []string{"notify@50", "notify@80", "notify@80+p4", "wrap_up@100"}, keys(Due(r, 101, now, nil)))
	require.Equal(t, []string{"notify@50", "notify@80", "notify@80+p6", "wrap_up@100", "hard_stop@110"}, keys(Due(r, 111, now, nil)))

	timed := Rule{Name: "t", Period: PeriodDay, Budget: 10, Steps: []Step{{At: 50, Notify: []Target{TargetOperator}, RepeatEveryMinutes: 30}}}
	timed.Normalize()
	base := now.Add(-65 * time.Minute)
	fired := func(key string) (time.Time, bool) { return base, key == "notify@50" }
	require.Equal(t, []string{"notify@50", "notify@50+t2"}, keys(Due(timed, 60, now, fired)))
	require.Equal(t, []string{"notify@50"}, keys(Due(timed, 60, now, func(string) (time.Time, bool) { return time.Time{}, false })))
}
