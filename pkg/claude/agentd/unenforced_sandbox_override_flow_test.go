package agentd_test

import (
	"encoding/json"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const closedNetworkOverrideRefusal = "Codex builtin sandbox (tools-only scope) cannot enforce closed network access; " +
	"choose a sandbox implementation that can enforce closed network access, use network open, " +
	"or enable “Allow launch without enforcement” in the dashboard spawn dialog"

const dashboardOverrideTestOrigin = "http://127.0.0.1:0"

func dashboardOverrideRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	request := testharness.JSONRequest(t, method, path, body)
	request.Header.Set("Origin", dashboardOverrideTestOrigin)
	return request
}

func TestDashboardClosedNetworkOverrideIsFreshSpawnOnlyAndDisclosed(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Codex builtin closed-network unenforceability is the Linux capability cell")
	}
	restoreURL := agentd.SetPopupBaseURLForTest(dashboardOverrideTestOrigin)
	t.Cleanup(restoreURL)
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "closed-network",
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeClosed,
		},
	})
	require.NoError(t, err)
	body := map[string]any{
		"name":                   "worker",
		"harness":                harness.CodexName,
		"sandbox":                harness.SandboxManagedProfile,
		"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
		"sandbox_profile":        "closed-network",
	}

	without := f.AsHuman().SpawnWith("crew", body)
	require.Equal(t, http.StatusUnprocessableEntity, without.Code)
	var failure struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(without.Raw, &failure))
	assert.Equal(t, harness.SandboxCapabilityNetworkAllowlist, failure.Code)
	assert.Equal(t, closedNetworkOverrideRefusal, failure.Error,
		"the absent field keeps the exact capability refusal and named remedies")

	dashboardBody := make(map[string]any, len(body)+1)
	for key, value := range body {
		dashboardBody[key] = value
	}
	dashboardBody["allow_unenforced_sandbox"] = true
	dashboard := agentd.BuildDashboardHandlerForTest()
	spawn := testharness.Serve(dashboard, dashboardOverrideRequest(
		t, http.MethodPost, "/api/groups/crew/spawn", dashboardBody))
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Body.String())
	var launched struct {
		ConvID      string `json:"conv_id"`
		TmuxSession string `json:"tmux_session"`
	}
	testharness.DecodeJSON(t, spawn, &launched)
	require.NotEmpty(t, launched.ConvID)
	require.NotEmpty(t, launched.TmuxSession)

	snapshot, err := db.AgentEffectiveSandboxConfigForConv(launched.ConvID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.Len(t, snapshot.Effective.AccessNotices, 1)
	notice := snapshot.Effective.AccessNotices[0]
	assert.Equal(t, sandboxpolicy.AccessNoticeClassDegradation, notice.Class)
	assert.Equal(t, "network", notice.Axis)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		notice.Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notice.Effect)
	assert.Contains(t, notice.Detail, "outbound network access remains open")

	spawnedSnapshot, ok := f.World.SpawnSandboxPolicy(launched.ConvID)
	require.True(t, ok)
	require.NotNil(t, spawnedSnapshot)
	require.Len(t, spawnedSnapshot.Effective.AccessNotices, 1,
		"the persisted notice must reach the production spawner boundary")

	// The flow simulator swaps out `tclaude session new`, so it does not perform
	// that command's production write-back of the private snapshot onto the
	// session row. Complete that external-boundary effect with the exact
	// snapshot observed by the simulated spawner, then exercise the production
	// dashboard read path below.
	sessionRows, err := db.FindSessionsByConvID(launched.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessionRows)
	sessionRows[0].EffectiveSandbox = spawnedSnapshot
	require.NoError(t, db.SaveSession(sessionRows[0]))

	var rendered struct {
		Groups []struct {
			Name    string `json:"name"`
			Members []struct {
				ConvID string `json:"conv_id"`
				State  struct {
					Notices []sandboxpolicy.AccessNotice `json:"sandbox_access_notices"`
				} `json:"state"`
			} `json:"members"`
		} `json:"groups"`
	}
	snapshotResponse := testharness.Serve(dashboard,
		dashboardOverrideRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, snapshotResponse.Code)
	testharness.DecodeJSON(t, snapshotResponse, &rendered)
	require.Len(t, rendered.Groups, 1)
	require.Len(t, rendered.Groups[0].Members, 1)
	require.Lenf(t, rendered.Groups[0].Members[0].State.Notices, 1,
		"snapshot body=%s", snapshotResponse.Body.String())
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		rendered.Groups[0].Members[0].State.Notices[0].Reason)

	clone := f.AsHuman().CloneWith(launched.ConvID, map[string]any{
		"no_copy_conv": true,
	})
	require.Equal(t, http.StatusUnprocessableEntity, clone.Code)
	assert.Contains(t, string(clone.Raw), closedNetworkOverrideRefusal,
		"clone must not treat the predecessor's disclosure notice as authorization")

	reincarnate := f.AsHuman().ReincarnateWith(launched.ConvID, map[string]any{
		"follow_up": "continue",
	})
	require.Equal(t, http.StatusUnprocessableEntity, reincarnate.Code)
	assert.Contains(t, string(reincarnate.Raw), closedNetworkOverrideRefusal,
		"reincarnation must require a new dashboard authorization")

	f.MarkOffline(launched.TmuxSession)
	resume := f.AsHuman().Resume(launched.ConvID)
	assert.Equal(t, "error", resume.Action)
	assert.Contains(t, resume.Detail, closedNetworkOverrideRefusal,
		"resume must require a new dashboard authorization")
}

func TestUnenforcedSandboxOverrideRejectsEveryRawV1Caller(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	body := map[string]any{
		"name":                     "worker",
		"allow_unenforced_sandbox": true,
		"sandbox_implementation":   string(sandboxpolicy.ImplementationHarnessBuiltin),
		"omit_sandbox_profiles":    true,
		"include_group_context":    false,
	}

	human := f.AsHuman().SpawnWith("crew", body)
	require.Equal(t, http.StatusForbidden, human.Code)
	assert.JSONEq(t, `{
		"code": "unenforced_sandbox_override_restricted",
		"error": "only the human operator may allow an unenforced sandbox through the dashboard spawn dialog"
	}`, string(human.Raw))

	const parent = "raw-agent-1111-2222-3333-444444444444"
	f.HaveMember("crew", parent)
	require.NoError(t, db.GrantAgentPermission(parent, agentd.PermGroupsSpawn, "test"))
	agent := f.AsAgent(parent).SpawnWith("crew", body)
	require.Equal(t, http.StatusForbidden, agent.Code)
	assert.JSONEq(t, `{
		"code": "unenforced_sandbox_override_restricted",
		"error": "only the human operator may allow an unenforced sandbox through the dashboard spawn dialog"
	}`, string(agent.Raw))
}

func TestDashboardOverrideDoesNotBypassFilteredModelTransport(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("filtered OpenCode transport launch seam is Linux-only")
	}
	restoreURL := agentd.SetPopupBaseURLForTest(dashboardOverrideTestOrigin)
	t.Cleanup(restoreURL)
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name: "opencode-local",
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Loopback: true},
			},
		},
	})
	require.NoError(t, err)

	dashboard := agentd.BuildDashboardHandlerForTest()
	resp := testharness.Serve(dashboard, dashboardOverrideRequest(
		t, http.MethodPost, "/api/groups/crew/spawn", map[string]any{
			"name":                     "opencode-worker",
			"harness":                  harness.OpenCodeName,
			"sandbox":                  harness.OpenCodeSandboxTclaudeLayer,
			"sandbox_implementation":   string(sandboxpolicy.ImplementationTclaudeLayer),
			"sandbox_profile":          "opencode-local",
			"allow_unenforced_sandbox": true,
		}))
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code)
	var failure struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	testharness.DecodeJSON(t, resp, &failure)
	assert.Equal(t, harness.SandboxCapabilityModelTransport, failure.Code)
	assert.Contains(t, failure.Error, "TCL-826")
	assert.Contains(t, failure.Error, "network open")
}
