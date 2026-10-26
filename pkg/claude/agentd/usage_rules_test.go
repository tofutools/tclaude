package agentd

import (
	"sort"
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
}

func TestUsageRules_LayerResolution(t *testing.T) {
	f := newUsageRulesFixture(t)
	f.agent("conv-a", 6)
	f.agent("conv-b", 6)
	c := f.agent("conv-c", 6)
	notifyAt := func(name string, at float64) usagerules.Rule {
		return usagerules.Rule{Name: name, Period: usagerules.PeriodDay, Budget: 10,
			Steps: []usagerules.Step{{At: at, Notify: []usagerules.Target{usagerules.TargetAgent}}}}
	}
	global := profileWith(t, "global", notifyAt("global", 10))
	g1p := profileWith(t, "g1", notifyAt("g1", 20))
	g2p := profileWith(t, "g2", notifyAt("g2", 30))
	own := profileWith(t, "own", notifyAt("own", 40))
	g1, err := db.CreateAgentGroup("g1", "")
	require.NoError(t, err)
	g2, err := db.CreateAgentGroup("g2", "")
	require.NoError(t, err)
	for _, m := range []struct {
		group int64
		conv  string
	}{{g1, "conv-b"}, {g2, "conv-b"}, {g1, "conv-c"}} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: m.group, ConvID: m.conv}))
	}
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGlobal, "", []int64{global.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGroup, db.UsageGroupScopeID(g1), []int64{g1p.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeGroup, db.UsageGroupScopeID(g2), []int64{g2p.ID, g1p.ID}, ""))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeAgent, c, []int64{own.ID}, ""))

	pollUsageRules(time.Now())
	// a: no group, no own → global. b: both groups apply, g1 once.
	// c: its own assignment replaces its group's.
	require.Equal(t, []string{`Usage budget: 60% of "global"`}, f.agentSubjects("conv-a"))
	require.Equal(t, []string{`Usage budget: 60% of "g1"`, `Usage budget: 60% of "g2"`}, f.agentSubjects("conv-b"))
	require.Equal(t, []string{`Usage budget: 60% of "own"`}, f.agentSubjects("conv-c"))
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
