package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const whoamiBrief = "Group context: use worktrees.\n\nYour task: ship the getter."

// haveIntrospectedAgent is a spawned agent: a member of alpha (its spawn
// group, with a role) and beta, the owner of gamma, with a task link, a
// spawn profile, a startup briefing and a link into alpha from beta.
func haveIntrospectedAgent(t *testing.T, f *testharness.Flow) (conv, agentID string) {
	t.Helper()
	conv = "whoami-full-aaaa-1111"
	f.HaveConvWithTitle(conv, "getter")
	f.HaveEnrolledAgent(conv)
	alpha := f.HaveGroup("alpha")
	beta := f.HaveGroup("beta")
	gamma := f.HaveGroup("gamma")
	f.HaveMemberWithRole("alpha", conv, "dev")
	f.HaveMember("beta", conv)
	require.NoError(t, db.AddAgentGroupOwner(gamma.ID, conv, "test"))
	_, err := db.SetAgentGroupDefaultContext("alpha", "Use worktrees.")
	require.NoError(t, err)
	_, err = db.InsertAgentGroupLink(beta.ID, alpha.ID, db.LinkModeMembersToMembers, "")
	require.NoError(t, err)

	agentID, err = db.AgentIDForConv(conv)
	require.NoError(t, err)
	_, err = db.SetAgentTaskRef(agentID, "https://example.test/t/1", "T-1")
	require.NoError(t, err)
	require.NoError(t, db.SetAgentInitialSpawnConfig(agentID, `{"profile":"opus-dev","sandbox_profile":"tight"}`))
	briefID, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: alpha.ID, ToConv: conv, Subject: db.StartupContextSubject, Body: whoamiBrief, ToRecipients: []string{conv}})
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentStartupSnapshot(db.AgentStartupSnapshot{
		AgentID: agentID, SpawnGroupID: alpha.ID, IncludeGroupContext: true, BriefMessageID: briefID,
	}))
	return conv, agentID
}

func whoamiFull(t *testing.T, f *testharness.Flow, conv string) (agent.WhoamiFull, int, string) {
	t.Helper()
	bridgeAgentClientToMuxAsAgent(t, f.Mux, conv)
	var stdout, stderr bytes.Buffer
	rc := agent.RunWhoamiFull(true, &stdout, &stderr)
	var out agent.WhoamiFull
	if rc == 0 {
		require.NoError(t, json.Unmarshal(stdout.Bytes(), &out), stdout.String())
	}
	return out, rc, stderr.String()
}

func TestWhoamiFull_NeedsSelfIntrospect(t *testing.T) {
	f := newFlow(t)
	conv, _ := haveIntrospectedAgent(t, f)
	_, rc, stderr := whoamiFull(t, f, conv)
	assert.NotZero(t, rc)
	assert.Contains(t, stderr, agentd.PermSelfIntrospect)

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(httptest.NewRequest(http.MethodGet, "/v1/whoami", nil), conv))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "getter", "plain whoami stays ungated")
}

func TestWhoamiFull_ReportsTheAgentsWholeSetup(t *testing.T) {
	f := newFlow(t)
	conv, agentID := haveIntrospectedAgent(t, f)
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermSelfIntrospect, "test"))
	me, rc, stderr := whoamiFull(t, f, conv)
	require.Zero(t, rc, stderr)

	assert.Equal(t, agentID, me.AgentID)
	require.NotNil(t, me.TaskRef)
	assert.Equal(t, "https://example.test/t/1", me.TaskRef.URL)
	assert.Equal(t, "opus-dev", me.Spawn.Profile)
	assert.Equal(t, "tight", me.Spawn.SandboxProfile)

	byName := map[string]int{}
	for i, g := range me.Groups {
		byName[g.Name] = i
	}
	require.Contains(t, byName, "alpha")
	alpha := me.Groups[byName["alpha"]]
	assert.True(t, alpha.Member)
	assert.Equal(t, "dev", alpha.Role)
	require.Len(t, alpha.LinksIn, 1)
	assert.Equal(t, "beta", alpha.LinksIn[0].From)
	require.Contains(t, byName, "gamma")
	assert.True(t, me.Groups[byName["gamma"]].Owner)
	assert.False(t, me.Groups[byName["gamma"]].Member)

	assert.True(t, me.Startup.Recorded)
	assert.Equal(t, "alpha", me.Startup.SpawnGroup)
	assert.True(t, me.Startup.IncludeGroupContext)
	assert.Equal(t, whoamiBrief, me.Startup.Brief)
	require.NotNil(t, me.Reinject)
	assert.Equal(t, "alpha", me.Reinject.Group)
	assert.Equal(t, db.ReinjectContexts, me.Reinject.Policy)
	assert.Equal(t, "Use worktrees.", me.Reinject.GroupContext)

	assert.Contains(t, me.Permissions.Effective, agentd.PermSelfIntrospect)
	assert.Contains(t, me.Permissions.Effective, agentd.PermGroupsContextRead)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, me.Permissions.MemberGroups[agentd.PermGroupsContextRead],
		"membership confers groups.context.read in each group")
	assert.Contains(t, me.Permissions.Provenance[agentd.PermGroupsSettingsRead], "owner", "ownership confers groups.settings.read")
	assert.Contains(t, me.Permissions.OwnedGroups, "gamma")

	bridgeAgentClientToMuxAsAgent(t, f.Mux, conv)
	var stdout, errOut bytes.Buffer
	require.Zero(t, agent.RunWhoamiFull(false, &stdout, &errOut), errOut.String())
	text := stdout.String()
	assert.Contains(t, text, "reached from beta")
	assert.Contains(t, text, "Your task: ship the getter.")
	assert.Contains(t, text, "group context:       included")
	assert.Contains(t, text, "groups.context.read                      owner [group=gamma] + member:alpha,beta")
}

func TestWhoamiFull_OptedOutGroupContextIsNotReinjected(t *testing.T) {
	f := newFlow(t)
	conv, agentID := haveIntrospectedAgent(t, f)
	snap, err := db.GetAgentStartupSnapshot(agentID)
	require.NoError(t, err)
	snap.IncludeGroupContext = false
	require.NoError(t, db.UpsertAgentStartupSnapshot(*snap))
	require.NoError(t, db.GrantAgentPermission(conv, agentd.PermSelfIntrospect, "test"))
	me, rc, stderr := whoamiFull(t, f, conv)
	require.Zero(t, rc, stderr)
	assert.False(t, me.Startup.IncludeGroupContext)
	require.NotNil(t, me.Reinject)
	assert.Empty(t, me.Reinject.GroupContext)
}

func TestWhoamiFull_HumanIsTheHuman(t *testing.T) {
	f := newFlow(t)
	bridgeAgentClientToMux(t, f.Mux)
	var stdout, stderr bytes.Buffer
	require.Zero(t, agent.RunWhoamiFull(false, &stdout, &stderr), stderr.String())
	assert.Equal(t, agent.HumanIdentity+"\n", stdout.String())
}
