package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Launch slice 8 (tcl-64cr0a): `tclaude conv resume` and the conversation
// watch TUI launch their pane themselves. Before they do, agentd prepares the
// agent's sandbox as an ordinary resume does, and after the launch it is told
// whether the pane started.

// standaloneFixture is a stopped agent whose group's sandbox profile has been
// replaced since it launched.
func standaloneFixture(t *testing.T) (*testharness.Flow, testharness.SpawnResp) {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("crew")
	for _, v := range []string{"v1", "v2"} {
		_, err := db.CreateSandboxProfile(&db.SandboxProfile{
			Name: "group-" + v, Environment: []db.SandboxEnvironmentEntry{{Name: "GROUP_VALUE", Value: v}},
		})
		require.NoError(t, err)
	}
	_, err := db.SetAgentGroupSandboxProfile("crew", "group-v1")
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.MarkOffline(spawn.TmuxSession)
	_, err = db.SetAgentGroupSandboxProfile("crew", "group-v2")
	require.NoError(t, err)
	return f, spawn
}

func storedGroupValue(t *testing.T, conv string) string {
	t.Helper()
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(conv)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	for _, e := range snapshot.Effective.Environment {
		if e.Name == "GROUP_VALUE" {
			return e.Value
		}
	}
	return ""
}

func standaloneCall(t *testing.T, f *testharness.Flow, route string, body map[string]any) map[string]any {
	t.Helper()
	rec := humanReq(t, f, http.MethodPost, "/v1/relaunch-sandbox/"+route, body)
	require.Equalf(t, http.StatusOK, rec.Code, "%s body=%s", route, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// Prepare re-reads the profiles as a resume does and records the result; a
// launch that started keeps it.
func TestStandaloneRelaunch_PrepareReReadsTheProfiles(t *testing.T) {
	f, spawn := standaloneFixture(t)
	require.Equal(t, "v1", storedGroupValue(t, spawn.ConvID))

	standaloneCall(t, f, "prepare", map[string]any{"conv_id": spawn.ConvID})
	assert.Equal(t, "v2", storedGroupValue(t, spawn.ConvID), "the group's current profile is what the pane renders")
	standaloneCall(t, f, "settle", map[string]any{"conv_id": spawn.ConvID, "launched": true})
	assert.Equal(t, "v2", storedGroupValue(t, spawn.ConvID))
}

// A launch that did not start puts the previous snapshot back, as a failed
// resume does.
func TestStandaloneRelaunch_AFailedLaunchRestoresThePreviousSnapshot(t *testing.T) {
	f, spawn := standaloneFixture(t)
	standaloneCall(t, f, "prepare", map[string]any{"conv_id": spawn.ConvID})
	standaloneCall(t, f, "settle", map[string]any{"conv_id": spawn.ConvID, "launched": false})
	assert.Equal(t, "v1", storedGroupValue(t, spawn.ConvID))
}

// Both routes are the human's: an agent gets a clear 403 and changes nothing.
func TestStandaloneRelaunch_IsHumanOnly(t *testing.T) {
	f, spawn := standaloneFixture(t)
	const caller = "srl0-1111-2222-3333-4444"
	f.HaveMember("crew", caller)
	for _, route := range []string{"prepare", "settle"} {
		rec := agentReq(t, f, caller, http.MethodPost, "/v1/relaunch-sandbox/"+route, map[string]any{"conv_id": spawn.ConvID})
		assert.Equalf(t, http.StatusForbidden, rec.Code, "%s body=%s", route, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "only the human operator")
	}
	assert.Equal(t, "v1", storedGroupValue(t, spawn.ConvID))
}

// A running conversation is not prepared: its policy must not change under it.
func TestStandaloneRelaunch_RefusesARunningConversation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "cwd": t.TempDir()})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	rec := humanReq(t, f, http.MethodPost, "/v1/relaunch-sandbox/prepare", map[string]any{"conv_id": spawn.ConvID})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent_live")
}

// A snapshot recorded by another launch after prepare (agentd's own resume)
// is not this relaunch's to restore.
func TestStandaloneRelaunch_SettleLeavesALaterSnapshotAlone(t *testing.T) {
	f, spawn := standaloneFixture(t)
	standaloneCall(t, f, "prepare", map[string]any{"conv_id": spawn.ConvID})
	later, err := db.AgentEffectiveSandboxConfigForConv(spawn.ConvID)
	require.NoError(t, err)
	later.LaunchEnvironmentOverrides = append(later.LaunchEnvironmentOverrides, db.SandboxEnvironmentEntry{Name: "LATER", Value: "1"})
	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, later))

	standaloneCall(t, f, "settle", map[string]any{"conv_id": spawn.ConvID, "launched": false})
	assert.Equal(t, "v2", storedGroupValue(t, spawn.ConvID), "the later snapshot stands")
}

// Prepare also answers whether the pane arms Remote Access, by the relaunch
// policy's group_remote_control row, as a resume decides it (tcl-7oauwa).
func TestStandaloneRelaunch_PrepareDecidesRemoteControl(t *testing.T) {
	f, spawn := standaloneFixture(t)
	on := true
	setGroupRemoteControl(t, &on)

	setRelaunchPolicy(t, f, "group_remote_control", "reuse")
	out := standaloneCall(t, f, "prepare", map[string]any{"conv_id": spawn.ConvID})
	assert.Equal(t, false, out["remote_control"], "reuse keeps the state of the last launch")
	standaloneCall(t, f, "settle", map[string]any{"conv_id": spawn.ConvID, "launched": false})

	setRelaunchPolicy(t, f, "group_remote_control", "re-resolve")
	out = standaloneCall(t, f, "prepare", map[string]any{"conv_id": spawn.ConvID})
	assert.Equal(t, true, out["remote_control"], "re-resolve applies the group's current policy")
	assert.Contains(t, out["changes"], "remote control: off → on (group policy)")
}
