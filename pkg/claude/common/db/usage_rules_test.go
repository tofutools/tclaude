package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

func testUsageRule(name string) usagerules.Rule {
	r := usagerules.Rule{Name: name, Period: usagerules.PeriodDay, Budget: 10,
		Steps: []usagerules.Step{{At: 50, Notify: []usagerules.Target{usagerules.TargetOperator}}}}
	r.Normalize()
	return r
}

func TestUsageProfilesCRUDAndAssignments(t *testing.T) {
	setupTestDB(t)
	p := &UsageProfile{Name: "Default", Rules: []usagerules.Rule{testUsageRule("budget")}}
	require.NoError(t, CreateUsageProfile(p))
	require.ErrorIs(t, CreateUsageProfile(&UsageProfile{Name: "default"}), ErrUsageProfileNameTaken)

	got, err := GetUsageProfileByName("DEFAULT")
	require.NoError(t, err)
	require.Equal(t, p.ID, got.ID)
	require.Equal(t, "budget", got.Rules[0].Name)

	got.Descr = "for everyone"
	ok, err := UpdateUsageProfile(got)
	require.NoError(t, err)
	require.True(t, ok)

	groupID, err := CreateAgentGroup("g1", "")
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("conv-1", "spawn")
	require.NoError(t, err)

	require.NoError(t, SetUsageProfileAssignments(UsageScopeGlobal, "ignored", []int64{p.ID}, ""))
	require.NoError(t, SetUsageProfileAssignments(UsageScopeGroup, UsageGroupScopeID(groupID), []int64{p.ID}, ""))
	require.NoError(t, SetUsageProfileAssignments(UsageScopeAgent, agentID, []int64{p.ID, p.ID}, "spawn profile x"))
	require.Error(t, SetUsageProfileAssignments(UsageScopeAgent, "", []int64{p.ID}, ""))

	as, err := ListUsageProfileAssignments()
	require.NoError(t, err)
	require.Equal(t, []UsageProfileAssignment{
		{Scope: "agent", ScopeID: agentID, ProfileID: p.ID, Source: "spawn profile x"},
		{Scope: "global", ScopeID: "", ProfileID: p.ID},
		{Scope: "group", ScopeID: UsageGroupScopeID(groupID), ProfileID: p.ID},
	}, as)

	// Group deletion drops its assignment; profile deletion cascades the rest.
	require.NoError(t, DeleteAgentGroup("g1"))
	as, err = ListUsageProfileAssignments()
	require.NoError(t, err)
	require.Len(t, as, 2)
	ok, err = DeleteUsageProfile(p.ID)
	require.NoError(t, err)
	require.True(t, ok)
	as, err = ListUsageProfileAssignments()
	require.NoError(t, err)
	require.Empty(t, as)
}

func TestUsageLimitsCRUD(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("g1", "")
	require.NoError(t, err)
	global := &UsageLimit{Scope: UsageScopeGlobal, Rule: testUsageRule("monthly"), Enabled: true}
	require.NoError(t, CreateUsageLimit(global))
	group := &UsageLimit{Scope: UsageScopeGroup, GroupID: groupID, Rule: testUsageRule("team"), Enabled: true}
	require.NoError(t, CreateUsageLimit(group))
	require.Error(t, CreateUsageLimit(&UsageLimit{Scope: UsageScopeGroup, Rule: testUsageRule("x")}))

	group.Enabled = false
	ok, err := UpdateUsageLimit(group)
	require.NoError(t, err)
	require.True(t, ok)

	limits, err := ListUsageLimits()
	require.NoError(t, err)
	require.Len(t, limits, 2)
	require.Equal(t, "global", limits[0].Scope)
	require.False(t, limits[1].Enabled)
	require.Equal(t, groupID, limits[1].GroupID)

	require.NoError(t, DeleteAgentGroup("g1"))
	limits, err = ListUsageLimits()
	require.NoError(t, err)
	require.Len(t, limits, 1)
}

func TestUsageRuleFiringsDedupeAndPrune(t *testing.T) {
	setupTestDB(t)
	old := time.Now().Add(-200 * 24 * time.Hour)
	f := UsageRuleFiring{RuleKey: "limit:1", Subject: "global", PeriodKey: "2026-10|b=500", StepKey: "notify@50", Action: "notify", Value: 250, Budget: 500, FiredAt: old}
	ok, err := InsertUsageRuleFiring(f, &HumanMessage{FromTitle: "Usage limits", Subject: "s", Body: "b"})
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = InsertUsageRuleFiring(f, &HumanMessage{FromTitle: "Usage limits", Subject: "s", Body: "b"})
	require.NoError(t, err)
	require.False(t, ok)
	messages, err := ListHumanMessagesPage(HumanMessageFilter{}, 0, MaxHumanMessagesPage)
	require.NoError(t, err)
	require.Len(t, messages, 1)

	life := f
	life.PeriodKey, life.Subject = "lifetime|b=20", "agt_x"
	ok, err = InsertUsageRuleFiring(life, nil)
	require.NoError(t, err)
	require.True(t, ok)

	times, err := UsageRuleFiringTimes("limit:1", "global", "2026-10|b=500")
	require.NoError(t, err)
	require.WithinDuration(t, old, times["notify@50"], time.Millisecond)

	n, err := PruneUsageRuleFirings(time.Now().Add(-120 * 24 * time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	rest, err := ListUsageRuleFiringsSince(time.Time{})
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.Equal(t, "agt_x", rest[0].Subject)
}
