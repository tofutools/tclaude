package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-ezvrww: the spawn preview resolves a spawn request through the spawn
// handler itself and answers with each field's value and source, without
// spawning anything.

func spawnPreview(t *testing.T, h http.Handler, group string, body map[string]any) (int, agent.ResolvedLaunch, string) {
	t.Helper()
	req := testharness.JSONRequest(t, http.MethodPost, "/api/groups/"+group+"/spawn-preview", body)
	req.Header.Set("Origin", "http://127.0.0.1:0")
	rec := testharness.Serve(h, agentd.AsHumanPeer(req))
	var out struct {
		Resolved agent.ResolvedLaunch `json:"resolved"`
	}
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out.Resolved, rec.Body.String()
}

func TestSpawnPreview_ResolvesWithoutSpawning(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	f.HaveGroup("crew")
	on := true
	_, err := db.SetAgentGroupRemoteControl("crew", &on)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createProfile(t, f, profileBody("worker", map[string]any{
		"auto_memory": true, "model": "opus",
	})).Code)
	h := agentd.BuildDashboardHandlerForTest()

	code, got, raw := spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "worker"})
	require.Equal(t, http.StatusOK, code, raw)
	require.NotNil(t, got.RemoteControl)
	assert.Equal(t, agent.ResolvedField{Value: "true", Source: "group remote-control policy"}, *got.RemoteControl)
	require.NotNil(t, got.AutoMemory)
	assert.Equal(t, "true", got.AutoMemory.Value)
	assert.Contains(t, got.AutoMemory.Source, "worker")
	require.NotNil(t, got.PeerMessaging)
	assert.Equal(t, agent.ResolvedField{Value: "false", Source: agent.ProvHarnessDefault}, *got.PeerMessaging)
	assert.Equal(t, "opus", got.Model.Value)
	assert.Equal(t, "claude", got.Harness.Value)

	code, got, raw = spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "worker",
		"remote_control": false, "peer_messaging": true})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, agent.ResolvedField{Value: "false", Source: agent.ProvExplicit}, *got.RemoteControl)
	assert.Equal(t, agent.ResolvedField{Value: "true", Source: agent.ProvExplicit}, *got.PeerMessaging)

	code, _, raw = spawnPreview(t, h, "crew", map[string]any{"name": "w", "profile": "nope"})
	assert.Equal(t, http.StatusBadRequest, code, "the preview refuses what the spawn would: %s", raw)

	g, err := db.GetAgentGroupByName("crew")
	require.NoError(t, err)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	assert.Empty(t, members, "a preview spawns nothing")
}
