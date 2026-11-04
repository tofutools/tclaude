package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-pa5589: agents relaunched before #3817 lost their explicit
// --harness-config. The finder lists them from their recorded spawn request,
// and a re-pin restores the posture so the next relaunch keeps it.

type harnessConfigRepairRow struct {
	AgentID   string `json:"agent_id"`
	ConvID    string `json:"conv_id"`
	Requested string `json:"requested"`
	Current   string `json:"current"`
	Source    string `json:"source"`
	Change    string `json:"change"`
}

func listHarnessConfigRepairs(t *testing.T, f *testharness.Flow) map[string]harnessConfigRepairRow {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/harness-config-repairs", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Agents []harnessConfigRepairRow `json:"agents"`
		Limits []string                 `json:"limits"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.NotEmpty(t, body.Limits, "the finder names what it cannot see")
	out := map[string]harnessConfigRepairRow{}
	for _, row := range body.Agents {
		out[row.AgentID] = row
	}
	return out
}

// spawnPinned spawns an agent with an explicit harness_config and returns its
// conv and agent IDs.
func spawnPinned(t *testing.T, f *testharness.Flow, name, harnessConfig string) (string, string) {
	t.Helper()
	body := map[string]any{"name": name, "cwd": t.TempDir()}
	if harnessConfig != "" {
		body["harness_config"] = harnessConfig
	}
	spawn := f.AsHuman().SpawnWith("crew", body)
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	return spawn.ConvID, agentID
}

// loseThePin rewrites an agent's recorded launch the way a relaunch before
// #3817 left it: the chain's posture, with the chain's provenance.
func loseThePin(t *testing.T, conv string, chain sandboxpolicy.HarnessConfigAccess) {
	t.Helper()
	snap, err := db.AgentEffectiveSandboxConfigForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, snap)
	lost := *snap
	lost.Effective.HarnessConfig = chain
	lost.Effective.Provenance.HarnessConfig = &sandboxpolicy.ProfileSource{Scope: sandboxpolicy.ScopeGlobal, Profile: "chain"}
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &lost))
}

func TestHarnessConfigRepair_FindAndRepin(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "chain", HarnessConfig: sandboxpolicy.HarnessConfigAccessWrite})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("chain"))

	widenedConv, widened := spawnPinned(t, f, "widened", "read")
	_, unpinned := spawnPinned(t, f, "unpinned", "write")
	_, intact := spawnPinned(t, f, "intact", "read")
	_, unasked := spawnPinned(t, f, "unasked", "")
	loseThePin(t, widenedConv, sandboxpolicy.HarnessConfigAccessWrite)
	unpinnedConv, err := db.CurrentConvForAgent(unpinned)
	require.NoError(t, err)
	loseThePin(t, unpinnedConv, sandboxpolicy.HarnessConfigAccessWrite)

	found := listHarnessConfigRepairs(t, f)
	assert.Equal(t, harnessConfigRepairRow{AgentID: widened, ConvID: widenedConv, Requested: "read",
		Current: "write", Source: "chain", Change: "widened"}, found[widened])
	assert.Equal(t, "unpinned", found[unpinned].Change, "same posture today, but the pin is gone")
	assert.NotContains(t, found, intact, "a launch that kept its pin")
	assert.NotContains(t, found, unasked, "a spawn that named no posture")

	// Only the selected agent is re-pinned.
	rec := humanReq(t, f, http.MethodPost, "/v1/harness-config-repairs",
		map[string]any{"agent_ids": []string{widened, intact}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Results []struct {
			AgentID string `json:"agent_id"`
			Repined bool   `json:"repinned"`
			Error   string `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Results, 2)
	assert.True(t, out.Results[0].Repined, out.Results[0].Error)
	assert.False(t, out.Results[1].Repined, "an agent that needs no repair is left alone")
	assert.NotEmpty(t, out.Results[1].Error)

	stored, err := db.AgentEffectiveSandboxConfigForConv(widenedConv)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, stored.Effective.HarnessConfig)
	assert.Nil(t, stored.Effective.Provenance.HarnessConfig, "recorded as the launch contract, as a spawn records it")
	found = listHarnessConfigRepairs(t, f)
	assert.NotContains(t, found, widened)
	assert.Contains(t, found, unpinned, "a deselected agent is untouched")

	// The next relaunch keeps the restored posture over the write chain.
	for _, session := range f.World.Tmux.Sessions() {
		f.MarkOffline(session)
	}
	f.AssertResumeSpawned(f.AsHuman().Resume(widenedConv))
	resumed, ok := f.World.SpawnSandboxPolicy(widenedConv)
	require.True(t, ok)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, resumed.Effective.HarnessConfig)
}

func TestHarnessConfigRepair_NeedsSandboxProfilesManage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const caller = "hcfg-1111-2222-3333-4444"
	f.HaveMember("crew", caller)
	assert.Equal(t, http.StatusForbidden, agentReq(t, f, caller, http.MethodGet, "/v1/harness-config-repairs", nil).Code)
	assert.Equal(t, http.StatusForbidden, agentReq(t, f, caller, http.MethodPost, "/v1/harness-config-repairs",
		map[string]any{"agent_ids": []string{"x"}}).Code)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermSandboxProfilesManage, "test"))
	assert.Equal(t, http.StatusOK, agentReq(t, f, caller, http.MethodGet, "/v1/harness-config-repairs", nil).Code)
}
