package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardAgentRestartReResolvesSandboxProfile(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "restart-rules",
		Environment: []db.SandboxEnvironmentEntry{{
			Name: "RESTART_VALUE", Value: "before",
		}},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "worker", "sandbox_profile": "restart-rules",
		"environment": []map[string]string{{"name": "BIRTH_VALUE", "value": "frozen"}},
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	before, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, before)
	require.Len(t, before.Effective.Environment, 1)
	assert.Equal(t, "before", before.Effective.Environment[0].Value)
	require.Equal(t, []db.SandboxEnvironmentEntry{{Name: "BIRTH_VALUE", Value: "frozen"}}, before.LaunchEnvironment)

	profile, err := db.GetSandboxProfile("restart-rules")
	require.NoError(t, err)
	profile.Environment[0].Value = "after"
	require.NoError(t, db.UpdateSandboxProfile(profile))
	f.SetSessionStatus(spawn.ConvID, session.StatusIdle)
	const attachedTTY = "/dev/pts/71"
	f.World.Tmux.AttachClient(attachedTTY, spawn.TmuxSession)

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(
		t, http.MethodPost, "/api/agents/"+spawn.AgentID+"/restart", nil,
	))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var response struct {
		Restart         string `json:"restart"`
		SwitchedClients int    `json:"switched_clients"`
	}
	testharness.DecodeJSON(t, rec, &response)
	assert.Equal(t, "resumed", response.Restart)
	assert.Equal(t, 1, response.SwitchedClients,
		"the attached terminal should follow the smooth restart")
	clientSession := f.World.Tmux.ClientSession(attachedTTY)
	assert.NotEmpty(t, clientSession)
	assert.NotContains(t, clientSession, "restart-",
		"the attached terminal must not remain parked on the bridge")
	assert.True(t, f.World.Tmux.IsAlive(clientSession),
		"the attached terminal should target the resumed live pane")

	after, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, after)
	require.Len(t, after.Effective.Environment, 1)
	assert.Equal(t, "after", after.Effective.Environment[0].Value,
		"restart should resolve the profile's current rules")
	assert.Equal(t, []db.SandboxEnvironmentEntry{{Name: "BIRTH_VALUE", Value: "frozen"}}, after.LaunchEnvironment,
		"restart must preserve birth-time group/profile/per-spawn environment")

	snapshot := fetchDashSnapshot(t, mux)
	group := groupInSnap(snapshot, "crew")
	require.NotNil(t, group)
	require.Len(t, group.Members, 1)
	assert.True(t, group.Members[0].Online, "the same agent should be online again")
	assert.Equal(t, spawn.ConvID, group.Members[0].ConvID)
	assert.Equal(t, spawn.AgentID, group.Members[0].AgentID)
}

func TestDashboardAgentRestartRefusesBusyAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "busy-worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(
		t, http.MethodPost, "/api/agents/"+spawn.AgentID+"/restart", nil,
	))
	require.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"agent_not_idle"`)
	assert.True(t, f.World.Tmux.IsAlive(spawn.TmuxSession),
		"an ineligible restart must not stop the running agent")
}
