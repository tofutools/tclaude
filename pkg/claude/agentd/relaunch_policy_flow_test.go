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

// Launch slice 6d (tcl-4qj8c6): the harness_config relaunch row decides how an
// explicit --harness-config posture weighs against the sandbox profiles' on
// every relaunch. Nothing configured is explicit-wins.

func setRelaunchPolicy(t *testing.T, f *testharness.Flow, field, mode string) {
	t.Helper()
	rec := humanReq(t, f, http.MethodPut, "/v1/relaunch-policy", map[string]any{"field": field, "mode": mode})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func resumedHarnessConfig(t *testing.T, f *testharness.Flow, conv string) sandboxpolicy.HarnessConfigAccess {
	t.Helper()
	for _, session := range f.World.Tmux.Sessions() {
		f.MarkOffline(session)
	}
	f.AssertResumeSpawned(f.AsHuman().Resume(conv))
	policy, ok := f.World.SpawnSandboxPolicy(conv)
	require.True(t, ok)
	return policy.Effective.HarnessConfig
}

func TestRelaunchPolicy_HarnessConfigModesOnResume(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "floored", HarnessConfig: sandboxpolicy.HarnessConfigAccessDefault})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("floored"))
	conv, _ := spawnPinned(t, f, "writer", "write")

	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, resumedHarnessConfig(t, f, conv), "explicit-wins by default")

	setRelaunchPolicy(t, f, "harness_config", "tighten-only")
	assert.True(t, sandboxpolicy.HarnessConfigFloorApplies(resumedHarnessConfig(t, f, conv)),
		"tighten-only: the profiles keep the floor, so an explicit write gets it back")

	setRelaunchPolicy(t, f, "harness_config", "explicit-wins")
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, resumedHarnessConfig(t, f, conv),
		"switching back restores the explicit posture")
}

func TestRelaunchPolicy_ChainWinsAndTheRepairFinder(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "chain", HarnessConfig: sandboxpolicy.HarnessConfigAccessWrite})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("chain"))
	conv, agentID := spawnPinned(t, f, "pinned", "read")

	setRelaunchPolicy(t, f, "harness_config", "chain-wins")
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, resumedHarnessConfig(t, f, conv), "chain-wins takes the profiles' posture")
	assert.NotContains(t, listHarnessConfigRepairs(t, f), agentID, "a posture the policy gave up is not a lost pin")

	setRelaunchPolicy(t, f, "harness_config", "explicit-wins")
	assert.Contains(t, listHarnessConfigRepairs(t, f), agentID, "under explicit-wins the recorded launch is missing its pin")
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, resumedHarnessConfig(t, f, conv), "and the next relaunch restores it")
}

func TestRelaunchPolicy_ReadOpenSetGated(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	const caller = "rlp0-1111-2222-3333-4444"
	f.HaveMember("crew", caller)

	rec := agentReq(t, f, caller, http.MethodGet, "/v1/relaunch-policy", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view struct {
		Rows []struct {
			Field    string `json:"field"`
			Mode     string `json:"mode"`
			Default  string `json:"default"`
			Set      bool   `json:"set"`
			Settable bool   `json:"settable"`
			Why      string `json:"why"`
		} `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	rows := map[string]int{}
	for i, row := range view.Rows {
		rows[row.Field] = i
	}
	require.Contains(t, rows, "harness_config")
	hc := view.Rows[rows["harness_config"]]
	assert.Equal(t, "explicit-wins", hc.Mode)
	assert.Equal(t, "explicit-wins", hc.Default)
	assert.True(t, hc.Settable)
	assert.False(t, hc.Set)
	assert.NotEmpty(t, hc.Why)
	require.Contains(t, rows, "group_env")
	assert.False(t, view.Rows[rows["group_env"]].Settable, "a row no relaunch reads yet")

	body := map[string]any{"field": "harness_config", "mode": "chain-wins"}
	assert.Equal(t, http.StatusForbidden, agentReq(t, f, caller, http.MethodPut, "/v1/relaunch-policy", body).Code)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermRelaunchPolicyManage, "test"))
	assert.Equal(t, http.StatusOK, agentReq(t, f, caller, http.MethodPut, "/v1/relaunch-policy", body).Code)

	refused := humanReq(t, f, http.MethodPut, "/v1/relaunch-policy", map[string]any{"field": "group_env", "mode": "reuse"})
	assert.Equal(t, http.StatusBadRequest, refused.Code)
	assert.Contains(t, refused.Body.String(), "not_settable")
	assert.Equal(t, http.StatusBadRequest, humanReq(t, f, http.MethodPut, "/v1/relaunch-policy",
		map[string]any{"field": "harness_config", "mode": "reuse"}).Code, "a mode the row does not offer")

	// Setting the default clears the stored row.
	setRelaunchPolicy(t, f, "harness_config", "explicit-wins")
	stored, err := db.ListRelaunchPolicyRows()
	require.NoError(t, err)
	assert.Empty(t, stored)
}
