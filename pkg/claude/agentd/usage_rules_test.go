package agentd

import (
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

type usageRulesFixture struct {
	t       *testing.T
	mu      sync.Mutex
	alive   map[string]struct{}
	stopped []string
}

func newUsageRulesFixture(t *testing.T) *usageRulesFixture {
	setupTestDB(t)
	t.Cleanup(SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	f := &usageRulesFixture{t: t, alive: map[string]struct{}{}}
	prevStop, prevLive := usageRulesHardStop, usageRulesLiveSessions
	usageRulesHardStop = func(conv string) memberOpResult {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stopped = append(f.stopped, conv)
		delete(f.alive, "tmux-"+conv) // a stopped agent's pane is gone
		return memberOpResult{ConvID: conv, Action: "killed"}
	}
	usageRulesLiveSessions = func() (map[string]struct{}, error) { return f.alive, nil }
	t.Cleanup(func() { usageRulesHardStop, usageRulesLiveSessions = prevStop, prevLive })
	return f
}

// agent seeds a running agent with the given recorded API spend today and
// returns its stable id.
func (f *usageRulesFixture) agent(conv string, cost float64) string {
	f.t.Helper()
	require.NoError(f.t, db.SaveSession(&db.SessionRow{ID: "s-" + conv, TmuxSession: "tmux-" + conv, ConvID: conv, Cwd: "/tmp/" + conv, Status: "idle"}))
	require.NoError(f.t, db.UpdateSessionCost("s-"+conv, cost))
	id, _, err := db.EnsureAgentForConv(conv, "spawn")
	require.NoError(f.t, err)
	f.alive["tmux-"+conv] = struct{}{}
	return id
}

func (f *usageRulesFixture) setCost(conv string, cost float64) {
	f.t.Helper()
	require.NoError(f.t, db.UpdateSessionCost("s-"+conv, cost))
}

func (f *usageRulesFixture) operatorSubjects() []string {
	f.t.Helper()
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, db.MaxHumanMessagesPage)
	require.NoError(f.t, err)
	var out []string
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

func (f *usageRulesFixture) agentSubjects(conv string) []string {
	f.t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 100)
	require.NoError(f.t, err)
	var out []string
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

func profileWith(t *testing.T, name string, rules ...usagerules.Rule) *db.UsageProfile {
	t.Helper()
	rules, err := usagerules.ValidateProfileRules(rules)
	require.NoError(t, err)
	p := &db.UsageProfile{Name: name, Rules: rules}
	require.NoError(t, db.CreateUsageProfile(p))
	return p
}

func ladderRule() usagerules.Rule {
	return usagerules.Rule{Name: "budget", Period: usagerules.PeriodLifetime, Budget: 10, Steps: []usagerules.Step{
		{At: 50, Notify: []usagerules.Target{usagerules.TargetAgent}},
		{At: 80, Notify: []usagerules.Target{usagerules.TargetAgent, usagerules.TargetOperator}},
		{At: 100, Action: usagerules.ActionWrapUp},
		{At: 120, Action: usagerules.ActionHardStop},
	}}
}

func TestUsageRules_AgentLadder(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 4)
	p := profileWith(t, "default", ladderRule())
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	now := time.Now()

	pollUsageRules(now)
	require.Empty(t, f.agentSubjects("conv-a"))

	f.setCost("conv-a", 5)
	pollUsageRules(now)
	pollUsageRules(now) // deduped
	require.Equal(t, []string{`Usage budget: 50% of "budget"`}, f.agentSubjects("conv-a"))
	require.Empty(t, f.operatorSubjects())

	f.setCost("conv-a", 10.5) // jumps over 80% and 100%
	pollUsageRules(now)
	require.Equal(t, []string{
		`Usage budget reached: please wrap up ("budget")`,
		`Usage budget: 105% of "budget"`,
		`Usage budget: 50% of "budget"`,
	}, f.agentSubjects("conv-a"))
	require.Len(t, f.operatorSubjects(), 2) // the 80% notify and the wrap-up
	require.Empty(t, f.stopped)

	f.setCost("conv-a", 12)
	pollUsageRules(now)
	require.Equal(t, []string{"conv-a"}, f.stopped)
	pollUsageRules(now)
	require.Equal(t, []string{"conv-a"}, f.stopped, "a hard stop fires once")

	// Resumed while still past the hard stop: stopped again, and the
	// operator is told why.
	f.alive["tmux-conv-a"] = struct{}{}
	pollUsageRules(now)
	require.Equal(t, []string{"conv-a", "conv-a"}, f.stopped, "a hard stop stays in force for its period")
	require.True(t, slices.ContainsFunc(f.operatorSubjects(), func(s string) bool { return strings.Contains(s, "still past its hard stop") }),
		"%v", f.operatorSubjects())

	// Extending the budget lifts it.
	agentID, err := db.AgentIDForConv("conv-a")
	require.NoError(t, err)
	require.NoError(t, db.SetUsageBudgetExtension(agentID, usageProfileRuleKey(p.ID, "budget"), 20))
	f.alive["tmux-conv-a"] = struct{}{}
	pollUsageRules(now)
	require.Len(t, f.stopped, 2, "an extended budget re-arms the ladder below the stop")
}

func TestUsageRules_LayerResolution(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 6)
	f.agent("conv-b", 6)
	c := f.agent("conv-c", 6)
	d := f.agent("conv-d", 6)
	notifyAt := func(name string, at float64) usagerules.Rule {
		return usagerules.Rule{Name: name, Period: usagerules.PeriodDay, Budget: 10,
			Steps: []usagerules.Step{{At: at, Notify: []usagerules.Target{usagerules.TargetAgent}}}}
	}
	global := profileWith(t, "global", notifyAt("global", 10))
	g1p := profileWith(t, "g1", notifyAt("g1", 20))
	g2p := profileWith(t, "g2", notifyAt("g2", 30))
	own := profileWith(t, "own", notifyAt("own", 40))
	linked := profileWith(t, "linked", notifyAt("linked", 50))
	g1, err := db.CreateAgentGroup("g1", "")
	require.NoError(t, err)
	g2, err := db.CreateAgentGroup("g2", "")
	require.NoError(t, err)
	for _, m := range []struct {
		group int64
		conv  string
	}{{g1, "conv-b"}, {g2, "conv-b"}, {g1, "conv-c"}, {g1, "conv-d"}} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: m.group, ConvID: m.conv}))
	}
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{global.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGroup, db.UsageGroupScopeID(g1), []int64{g1p.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGroup, db.UsageGroupScopeID(g2), []int64{g2p.ID, g1p.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeAgent, c, []int64{own.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeAgent, d, []int64{linked.ID}, usageLinkedSourcePrefix+"reviewer"))

	pollUsageRules(time.Now())
	// a: no group, no own → global. b: both groups apply, g1 once.
	// c: its own assignment replaces its group's.
	require.Equal(t, []string{`Usage budget: 60% of "global"`}, f.agentSubjects("conv-a"))
	require.Equal(t, []string{`Usage budget: 60% of "g1"`, `Usage budget: 60% of "g2"`}, f.agentSubjects("conv-b"))
	require.Equal(t, []string{`Usage budget: 60% of "own"`}, f.agentSubjects("conv-c"))
	// d: a spawn profile's linked profile adds to its group's, never sheds it.
	require.Equal(t, []string{`Usage budget: 60% of "g1"`, `Usage budget: 60% of "linked"`}, f.agentSubjects("conv-d"))
}

func TestUsageRules_GlobalAndGroupLimits(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 300)
	f.agent("conv-b", 40)
	f.agent("conv-idle", 0)
	delete(f.alive, "tmux-conv-idle")
	g, err := db.CreateAgentGroup("team", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: g, ConvID: "conv-b"}))

	monthly := usagerules.Rule{Name: "monthly", Period: usagerules.PeriodMonth, Budget: 500, Steps: []usagerules.Step{
		{At: 50, Notify: []usagerules.Target{usagerules.TargetOperator}},
		{At: 90, Action: usagerules.ActionWrapUp},
	}}
	monthly.Normalize()
	require.NoError(t, db.CreateUsageLimit(&db.UsageLimit{Scope: db.UsageScopeGlobal, Rule: monthly, Enabled: true}))
	team := usagerules.Rule{Name: "team", Period: usagerules.PeriodWeek, Budget: 50, Steps: []usagerules.Step{
		{At: 80, Notify: []usagerules.Target{usagerules.TargetAgent, usagerules.TargetOperator}},
		{At: 100, Action: usagerules.ActionHardStop},
	}}
	team.Normalize()
	require.NoError(t, db.CreateUsageLimit(&db.UsageLimit{Scope: db.UsageScopeGroup, GroupID: g, Rule: team, Enabled: true}))
	disabled := monthly
	disabled.Name = "off"
	disabled.Budget = 1
	require.NoError(t, db.CreateUsageLimit(&db.UsageLimit{Scope: db.UsageScopeGlobal, Rule: disabled}))

	pollUsageRules(time.Now())
	require.Equal(t, []string{
		`All agents reached 68% of "monthly"`,
		`Group team reached 80% of "team"`,
	}, f.operatorSubjects())
	require.Equal(t, []string{`Usage budget: 80% of "team"`}, f.agentSubjects("conv-b"))
	require.Empty(t, f.agentSubjects("conv-a"), "the group notice reaches members only")

	f.setCost("conv-a", 410)
	f.setCost("conv-b", 50)
	pollUsageRules(time.Now())
	// The global wrap-up reaches every running agent, not the stopped one.
	require.Contains(t, f.agentSubjects("conv-a"), `Usage budget reached: please wrap up ("monthly")`)
	require.Contains(t, f.agentSubjects("conv-b"), `Usage budget reached: please wrap up ("monthly")`)
	require.Empty(t, f.agentSubjects("conv-idle"))
	require.Equal(t, []string{"conv-b"}, f.stopped)
}

func TestUsageRules_DryRunOnlyTellsOperator(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 20)
	r := ladderRule()
	r.DryRun = true
	p := profileWith(t, "dry", r)
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	pollUsageRules(time.Now())
	require.Empty(t, f.agentSubjects("conv-a"))
	require.Empty(t, f.stopped)
	subjects := f.operatorSubjects()
	require.Len(t, subjects, 4)
	for _, s := range subjects {
		require.Contains(t, s, "[dry run]")
	}
}

func TestUsageRules_BudgetChangeRearms(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 6)
	p := profileWith(t, "default", ladderRule())
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	pollUsageRules(time.Now())
	require.Len(t, f.agentSubjects("conv-a"), 1)
	p.Rules[0].Budget = 12
	_, err := db.UpdateUsageProfile(p)
	require.NoError(t, err)
	pollUsageRules(time.Now())
	require.Equal(t, []string{`Usage budget: 50% of "budget"`, `Usage budget: 60% of "budget"`}, f.agentSubjects("conv-a"))
}

func TestUsageRules_OfflineAgentStepsWaitForResume(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 13)
	delete(f.alive, "tmux-conv-a")
	p := profileWith(t, "default", ladderRule())
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	pollUsageRules(time.Now())
	require.Empty(t, f.agentSubjects("conv-a"))
	require.Empty(t, f.operatorSubjects())

	f.alive["tmux-conv-a"] = struct{}{}
	pollUsageRules(time.Now())
	require.Equal(t, []string{"conv-a"}, f.stopped, "a hard stop crossed while offline applies on resume")
}

func TestUsageRules_LivenessErrorSkipsTick(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 13)
	p := profileWith(t, "default", ladderRule())
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	usageRulesLiveSessions = func() (map[string]struct{}, error) { return nil, errors.New("tmux down") }
	pollUsageRules(time.Now())
	firings, err := db.ListUsageRuleFiringsSince(time.Time{})
	require.NoError(t, err)
	require.Empty(t, firings, "nothing is used up while liveness is unknown")
	usageRulesLiveSessions = func() (map[string]struct{}, error) { return f.alive, nil }
	pollUsageRules(time.Now())
	require.Equal(t, []string{"conv-a"}, f.stopped)
}

func TestUsageRules_JumpSendsOneMessagePerStep(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 9.5)
	r := usagerules.Rule{Name: "nag", Period: usagerules.PeriodDay, Budget: 10, Steps: []usagerules.Step{
		{At: 50, Notify: []usagerules.Target{usagerules.TargetAgent}, RepeatEveryPct: 10},
	}}
	p := profileWith(t, "default", r)
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	pollUsageRules(time.Now())
	require.Len(t, f.agentSubjects("conv-a"), 1)
	f.setCost("conv-a", 10.1)
	pollUsageRules(time.Now())
	require.Len(t, f.agentSubjects("conv-a"), 2, "the next repeat mark sends again")
}

func TestUsageRules_DryRunOffRearms(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 13)
	r := ladderRule()
	r.DryRun = true
	p := profileWith(t, "default", r)
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	pollUsageRules(time.Now())
	require.Empty(t, f.stopped)
	p.Rules[0].DryRun = false
	_, err := db.UpdateUsageProfile(p)
	require.NoError(t, err)
	pollUsageRules(time.Now())
	require.Equal(t, []string{"conv-a"}, f.stopped)
}

func TestUsageRules_WindowResetJitterKeepsOneLadder(t *testing.T) {
	setupTestDB(t)
	st := &usageRulesState{now: time.Now()}
	target := usageRuleTarget{ruleKey: "limit:1", subject: "global"}
	first := time.Date(2026, 10, 28, 17, 59, 59, 900_000_000, time.UTC)
	key := st.windowPeriodKey(target, first)
	ok, err := db.InsertUsageRuleFiring(db.UsageRuleFiring{RuleKey: "limit:1", Subject: "global", PeriodKey: key + "|b=100", StepKey: "notify@80", Action: "notify"}, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, key, st.windowPeriodKey(target, first.Add(600*time.Millisecond)))
	require.NotEqual(t, key, st.windowPeriodKey(target, first.Add(5*time.Hour)))
}

func TestUsageRules_AgentAdjustments(t *testing.T) {
	f := newUsageRulesFixture(t)
	a := f.agent("conv-a", 11)
	b := f.agent("conv-b", 11)
	c := f.agent("conv-c", 11)
	p := profileWith(t, "default", ladderRule())
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{p.ID}, ""))
	require.NoError(t, db.SetUsageAgentOverride(a, true, time.Time{}))
	require.NoError(t, db.SetUsageAgentOverride(b, false, time.Now().Add(time.Hour)))
	require.NoError(t, db.SetUsageBudgetExtension(c, usageProfileRuleKey(p.ID, "budget"), 10))

	pollUsageRules(time.Now())
	require.Empty(t, f.agentSubjects("conv-a"), "exempt")
	require.Empty(t, f.agentSubjects("conv-b"), "snoozed")
	require.Equal(t, []string{`Usage budget: 55% of "budget"`}, f.agentSubjects("conv-c"), "11 of 10+10")
	require.Empty(t, f.stopped)

	statuses, _ := latestUsageRuleStatus()
	paused := map[string]string{}
	for _, s := range statuses {
		paused[s.AgentID] = s.Paused
	}
	require.Equal(t, map[string]string{a: "exempt", b: "snoozed", c: ""}, paused)

	// The snooze ends: the steps it held fire now.
	require.NoError(t, db.SetUsageAgentOverride(b, false, time.Time{}))
	pollUsageRules(time.Now())
	require.Contains(t, f.agentSubjects("conv-b"), `Usage budget reached: please wrap up ("budget")`)
}

// A day, week or month rule reads inside the limit and gets the full ledger's
// figure from it. A lifetime rule is the one period that needs the older rows.
func TestUsageRules_SpendReadStaysInsideTheLimitUnlessLifetime(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 10)
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now()
	old := now.AddDate(0, -3, 0).Format(costDayKey)
	_, err = d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd) VALUES ('old-sess', ?, 'old-conv', 7), ('long-sess', ?, 'long-conv', 20), ('long-sess', ?, 'long-conv', 23)`,
		old, old, now.Format(costDayKey))
	require.NoError(t, err)
	require.NoError(t, db.RefreshCostOpening(now))

	spent := func(period usagerules.Period) float64 {
		st := &usageRulesState{now: now, limits: []*db.UsageLimit{{Rule: usagerules.Rule{Metric: usagerules.MetricTotalUSD, Period: period}}}}
		rows, err := st.spendRows()
		require.NoError(t, err)
		return sumUsageSlices(newUsageSpend(mixedCostDeltasFromRows(rows), nil).all, usagerules.MetricTotalUSD, usagerules.PeriodStartDay(period, now))
	}
	full, _, _, err := countedCostDailyRows()
	require.NoError(t, err)
	wantMonth := sumUsageSlices(newUsageSpend(mixedCostDeltasFromRows(full), nil).all, usagerules.MetricTotalUSD, usagerules.PeriodStartDay(usagerules.PeriodMonth, now))
	require.InDelta(t, wantMonth, spent(usagerules.PeriodMonth), 1e-9)
	lifetime := spent(usagerules.PeriodLifetime)

	for _, trigger := range []string{"insert", "delete", "update"} {
		_, err = d.Exec(`DROP TRIGGER cost_opening_stale_` + trigger)
		require.NoError(t, err)
	}
	_, err = d.Exec(`UPDATE session_cost_daily SET cost_usd = 9999 WHERE day = ?`, old)
	require.NoError(t, err)
	require.InDelta(t, wantMonth, spent(usagerules.PeriodMonth), 1e-9, "a month rule reads no older row")
	require.Greater(t, spent(usagerules.PeriodLifetime), lifetime, "a lifetime rule does")
}
