package agentd_test

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"net/http"
	"testing"
	"time"
)

func TestFederationAwaySurvivesLifecycleSweepsAndReturns(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	repoFixture(t, f)
	const source = "wagt-1111-2222-3333-4444"
	id, e := db.AgentIDForConv(source)
	require.NoError(t, e)
	group, e := db.GetAgentGroupByName("squad")
	require.NoError(t, e)
	require.NoError(t, db.AddAgentGroupOwner(group.ID, source, "human"))
	require.NoError(t, db.GrantAgentPermission(source, "human.notify", "human"))
	job := createCronAsHuman(t, f, map[string]any{"owner": source, "target": source, "interval": "1h", "name": "away-job", "body": "keep", "queue_when_offline": true})
	message := queueInternalNudge(t, source, "home inbox survives travel")
	agentd.WaitForBackgroundForTest()
	identity := db.FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "proof"}}
	require.NoError(t, db.DepartFederationIdentity(source, "home", "peer", "out", identity))
	writeRetiredCleanupConfig(t, true, 1)
	future := time.Now().AddDate(0, 0, 400)
	agentd.RunRetiredAgentCleanupForTest(future)
	agentd.RunReaperTickForTest(future)
	agentd.RunCronTickForTest(future)
	agentd.RunStandingOrderDebounceTickForTest(future)
	agentd.RunPendingSpawnSweepForTest()
	require.True(t, agentd.RunAwayLifecycleObserversForTest(source))
	agentd.WaitForBackgroundForTest()
	tree := byPath(discoverAllWorktrees(t, agentd.BuildDashboardHandlerForTest()).Worktrees)
	require.Equal(t, "agent", tree["/repo-wt-agent"].Category)
	require.False(t, tree["/repo-wt-agent"].Agents[0].Retired)
	a, e := db.GetAgent(id)
	require.NoError(t, e)
	require.NotNil(t, a)
	require.True(t, db.AgentAway(id))
	m, e := db.GetAgentMessage(message)
	require.NoError(t, e)
	require.True(t, m.NudgeCancelledAt.IsZero())
	require.Equal(t, "home inbox survives travel", m.Body)
	j, e := db.GetAgentCronJob(job.ID)
	require.NoError(t, e)
	require.True(t, j.Enabled)
	retired := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, "GET", "/api/retired", nil))
	require.NotContains(t, retired.Body.String(), id)
	_, e = db.ReserveFederationIdentity(identity, "home", "third-hop", "return")
	require.NoError(t, e)
	_, _, e = db.EnsureAgentForConvWithID("returned-home", id, "federation")
	require.NoError(t, e)
	perms, e := db.ListAgentPermissionOverridesForConv("returned-home")
	require.NoError(t, e)
	require.Contains(t, perms, "human.notify")
	owners, e := db.ListAgentGroupOwners(group.ID)
	require.NoError(t, e)
	require.Len(t, owners, 1)
	require.Equal(t, "returned-home", owners[0].ConvID)
	groups, e := db.ListGroupsForConv("returned-home")
	require.NoError(t, e)
	require.Len(t, groups, 1)
	m, e = db.GetAgentMessage(message)
	require.NoError(t, e)
	require.Equal(t, id, m.ToAgent)
}

func TestFederationAwayExplicitRetireDeleteAndRecall(t *testing.T) {
	for _, action := range []string{"retire", "delete"} {
		t.Run(action, func(t *testing.T) {
			f := newFlow(t)
			const source = "11111111-2222-3333-4444-555555555555"
			f.HaveConvWithTitle(source, "traveller")
			f.HaveEnrolledAgent(source)
			f.HaveGroup("home")
			f.HaveMember("home", source)
			id, e := db.AgentIDForConv(source)
			require.NoError(t, e)
			identity := db.FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "proof"}}
			require.NoError(t, db.DepartFederationIdentity(source, "home", "peer", "out", identity))
			method, path := "POST", "/v1/agent/"+source+"/retire?shutdown=0&delete_worktree=0"
			if action == "delete" {
				method, path = "DELETE", "/v1/agent/"+source+"/delete"
			}
			rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, nil)))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			_, e = db.ReserveFederationIdentity(identity, "home", "peer", "late-return")
			require.ErrorContains(t, e, "explicitly retired")
			rec = testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, "POST", "/v1/federation/agents/"+id+"/recall", map[string]any{})))
			require.Equal(t, 409, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "agent_terminal")
		})
	}
}
