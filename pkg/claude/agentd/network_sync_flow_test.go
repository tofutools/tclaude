package agentd_test

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"net/http"
	"testing"
)

func TestNetworkSyncSpawnPreferenceAndOneOffSave(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{"name": "sync", "harness": "claude", "network_auto_sync": true})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	for _, automatic := range []bool{true, false} {
		body := map[string]any{"name": "worker", "profile": "sync"}
		if !automatic {
			body["name"] = "manual"
			body["network_auto_sync"] = false
		}
		spawn := f.AsHuman().SpawnWith("crew", body)
		require.Equal(t, http.StatusOK, spawn.Code, string(spawn.Raw))
		var wire agent.SpawnResponse
		require.NoError(t, json.Unmarshal(spawn.Raw, &wire))
		snapshot, err := db.AgentEffectiveSandboxConfigForConv(wire.ConvID)
		require.NoError(t, err)
		require.NotNil(t, snapshot)
		require.Equal(t, automatic, snapshot.NetworkAutoSync)
	}
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "base", "network": map[string]any{"mode": "list"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = profileReq(t, f, http.MethodPost, "/v1/sandbox-profiles", map[string]any{"name": "outer", "includes": []string{"base"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, "outer")
	require.NoError(t, err)
	require.NoError(t, db.RegisterNetworkSyncLaunch("one-off", "session", snapshot))
	require.NoError(t, db.BeginNetworkSyncLaunch("one-off"))
	rec = profileReq(t, f, http.MethodPatch, "/v1/sandbox-profiles/base?sync_running=1", map[string]any{"name": "base", "network": map[string]any{"mode": "list", "allow": []map[string]any{{"host": "example.com"}}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	row, err := db.ReadNetworkSyncLaunch("one-off")
	require.NoError(t, err)
	require.False(t, row.Automatic)
	require.Equal(t, "pending", row.Status)
	require.NotNil(t, row.Requested)
	require.Equal(t, "example.com", row.Requested.Effective.Network.Allow[0].Host)
}
