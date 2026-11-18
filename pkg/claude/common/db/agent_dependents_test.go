package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cascadeFixture is an agent with one of everything that goes with it, plus a
// bystander's rows that must survive.
type cascadeFixture struct {
	agentID, bystanderID                    string
	ownedCron, targetCron, bystanderCron    int64
	ownedOrder, targetOrder, bystanderOrder int64
	ownedRule, bystanderRule                int64
}

func haveCascadeFixture(t *testing.T) cascadeFixture {
	t.Helper()
	const conv, other = "cascade-agent", "cascade-bystander"
	var f cascadeFixture
	var err error
	f.agentID, _, err = EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	f.bystanderID, _, err = EnsureAgentForConv(other, "test")
	require.NoError(t, err)

	cron := func(name, owner, target string) int64 {
		id, err := InsertAgentCronJob(&AgentCronJob{Name: name, OwnerConv: owner, TargetConv: target,
			TargetKind: CronTargetConv, IntervalSeconds: 60, Body: name, Enabled: true})
		require.NoError(t, err)
		return id
	}
	f.ownedCron = cron("owned", conv, other)
	f.targetCron = cron("addressed", other, conv)
	f.bystanderCron = cron("bystander", other, other)

	order := func(name, owner, target string) int64 {
		o := sampleOrder(name)
		o.OwnerAgent = owner
		if target != "" {
			o.TargetKind, o.TargetAgent, o.GroupID = StandingTargetConv, target, 0
		}
		id, err := InsertStandingOrder(o)
		require.NoError(t, err)
		return id
	}
	f.ownedOrder = order("owned-order", f.agentID, "")
	f.targetOrder = order("addressed-order", f.bystanderID, f.agentID)
	f.bystanderOrder = order("bystander-order", f.bystanderID, "")

	rule := func(name, owner string) int64 {
		id, err := InsertTriggerRule(&TriggerRule{Name: name, Enabled: true, OwnerAgent: owner,
			ScopeKind: TriggerScopeGlobal, Source: TriggerSourceAgentIdle, DraftFilter: TriggerDraftInclude,
			ForSeconds: 60, Actions: []TriggerAction{{Type: TriggerActionMessage,
				Message: &TriggerMessageAction{Target: "agent", BodyTemplate: "wake"}}}})
		require.NoError(t, err)
		return id
	}
	f.ownedRule = rule("owned-rule", f.agentID)
	f.bystanderRule = rule("bystander-rule", f.bystanderID)
	return f
}

// Retiring with the cascade removes what exists only for the agent, reports
// it, and leaves a bystander's rows alone.
func TestRetireCascade_RemovesWhatExistsOnlyForTheAgent(t *testing.T) {
	setupTestDB(t)
	f := haveCascadeFixture(t)

	preview, err := ListAgentDependents(f.agentID)
	require.NoError(t, err)
	out, err := RetireAgentAuthorizationWithOptions("cascade-agent", "human", "test", false, RetireOptions{Cascade: true})
	require.NoError(t, err)
	require.True(t, out.Retired)
	assert.Equal(t, preview, out.Removed, "the retirement removes exactly what the preview listed")

	type key struct {
		kind string
		id   int64
		via  string
	}
	var removed []key
	for _, dep := range out.Removed {
		removed = append(removed, key{dep.Kind, dep.ID, dep.Via})
	}
	assert.ElementsMatch(t, []key{
		{AgentDependentCron, f.ownedCron, AgentDependentOwner},
		{AgentDependentCron, f.targetCron, AgentDependentTarget},
		{AgentDependentStandingOrder, f.ownedOrder, AgentDependentOwner},
		{AgentDependentStandingOrder, f.targetOrder, AgentDependentTarget},
		{AgentDependentTriggerRule, f.ownedRule, AgentDependentOwner},
	}, removed)
	assert.Zero(t, out.CronDisabled, "nothing of the agent's is left to disable")
	assert.Zero(t, out.StandingOrdersDisabled)

	for _, id := range []int64{f.ownedCron, f.targetCron} {
		job, err := GetAgentCronJob(id)
		require.NoError(t, err)
		assert.Nil(t, job)
	}
	for _, id := range []int64{f.ownedOrder, f.targetOrder} {
		order, err := GetStandingOrder(id)
		require.NoError(t, err)
		assert.Nil(t, order)
	}
	rule, err := GetTriggerRule(f.ownedRule)
	require.NoError(t, err)
	assert.Nil(t, rule)

	job, err := GetAgentCronJob(f.bystanderCron)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.True(t, job.Enabled)
	order, err := GetStandingOrder(f.bystanderOrder)
	require.NoError(t, err)
	require.NotNil(t, order)
	assert.True(t, order.Enabled)
	kept, err := GetTriggerRule(f.bystanderRule)
	require.NoError(t, err)
	require.NotNil(t, kept)
}

// Kept, a retirement does what it always did: it disables what the agent
// owns and leaves the rest.
func TestRetireCascade_KeptDisablesOwnedRows(t *testing.T) {
	setupTestDB(t)
	f := haveCascadeFixture(t)

	out, err := RetireAgentAuthorizationWithOptions("cascade-agent", "human", "test", false, RetireOptions{})
	require.NoError(t, err)
	assert.Empty(t, out.Removed)
	assert.Equal(t, int64(1), out.CronDisabled)
	assert.Equal(t, int64(1), out.StandingOrdersDisabled)

	owned, err := GetAgentCronJob(f.ownedCron)
	require.NoError(t, err)
	require.NotNil(t, owned)
	assert.False(t, owned.Enabled)
	assert.Equal(t, CronDisabledReasonAgentRetired, owned.DisabledReason)
	addressed, err := GetAgentCronJob(f.targetCron)
	require.NoError(t, err)
	require.NotNil(t, addressed, "a kept retirement leaves a job addressed to the agent")
	rule, err := GetTriggerRule(f.ownedRule)
	require.NoError(t, err)
	require.NotNil(t, rule, "a kept retirement leaves the agent's automations")
}

// Deleting an agent purges its agent-keyed rows that have no FK, and closes
// the trigger worker it was, so the worker stops holding a live slot.
func TestDeleteAgent_PurgesRowsWithoutForeignKeys(t *testing.T) {
	setupTestDB(t)
	const conv = "purge-agent"
	agentID, _, err := EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	parentID, _, err := EnsureAgentForConv("purge-parent", "test")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	for _, stmt := range []string{
		`INSERT INTO agent_lineage (child_agent_id, parent_agent_id, spawned_at) VALUES ('` + agentID + `', '` + parentID + `', 1)`,
		`INSERT INTO steward_roster (agent_id, seat, since, set_by) VALUES ('` + agentID + `', 'standby', 1, 'test')`,
		`INSERT INTO trigger_workers (action_index, agent_id, conv_id, state, created_at) VALUES (0, '` + agentID + `', '` + conv + `', 'live', 1)`,
	} {
		_, err := d.Exec(stmt)
		require.NoError(t, err, stmt)
	}

	_, err = DeleteAgentByConvID(conv)
	require.NoError(t, err)

	count := func(query string) int {
		var n int
		require.NoError(t, d.QueryRow(query, agentID).Scan(&n))
		return n
	}
	assert.Zero(t, count(`SELECT COUNT(*) FROM agent_lineage WHERE child_agent_id = ?`))
	assert.Zero(t, count(`SELECT COUNT(*) FROM steward_roster WHERE agent_id = ?`))
	var state, detail string
	require.NoError(t, d.QueryRow(`SELECT state, detail FROM trigger_workers WHERE agent_id = ?`, agentID).Scan(&state, &detail))
	assert.Equal(t, "exited", state)
	assert.Equal(t, "the agent was deleted", detail)
}
