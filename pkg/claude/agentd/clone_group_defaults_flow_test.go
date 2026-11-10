package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// haveGroupWithSandboxDefaults creates a group whose sandbox profile and group
// environment each name the group, so a launch shows which one it took.
func haveGroupWithSandboxDefaults(t *testing.T, f *testharness.Flow, name string) *db.AgentGroup {
	t.Helper()
	group := f.HaveGroup(name)
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:        name + "-profile",
		Environment: []db.SandboxEnvironmentEntry{{Name: "PROFILE_OWNER", Value: name}},
	})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile(name, name+"-profile")
	require.NoError(t, err)
	_, err = db.SetAgentGroupEnvironment(name, []sandboxpolicy.EnvironmentEntry{{Name: "GROUP_OWNER", Value: name}})
	require.NoError(t, err)
	return group
}

func launchEnv(snapshot *sandboxpolicy.Snapshot) map[string]string {
	out := map[string]string{}
	for _, entries := range [][]sandboxpolicy.EnvironmentEntry{snapshot.Effective.Environment, snapshot.LaunchEnvironment} {
		for _, entry := range entries {
			out[entry.Name] = entry.Value
		}
	}
	return out
}

// Scenario: the operator clones an agent from group alpha into group beta
// (a Ctrl-drop, or `tclaude agent clone --group beta`). By default the clone
// launches under beta's sandbox profile and environment straight away, not
// only after its first resume; each setting can instead be kept on the
// source's group. The clone's handoff says where its sandbox comes from.
func TestCloneIntoGroup_LaunchesUnderChosenGroupDefaults(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		keepSandbox, keepEnv      bool
		wantProfile, wantGroupEnv string
		wantResolution            string
	}{
		{name: "group defaults", wantProfile: "beta", wantGroupEnv: "beta", wantResolution: "beta"},
		{name: "keep sandbox", keepSandbox: true, wantProfile: "alpha", wantGroupEnv: "beta", wantResolution: "alpha"},
		{name: "keep environment", keepEnv: true, wantProfile: "beta", wantGroupEnv: "alpha", wantResolution: "beta"},
		{name: "keep both", keepSandbox: true, keepEnv: true, wantProfile: "alpha", wantGroupEnv: "alpha", wantResolution: "alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
			t.Cleanup(restoreURL)
			f := newFlow(t)
			const source = "clone-defaults-source-1111"
			f.HaveConvWithTitle(source, "worker")
			f.HaveAliveSession(source, "spwn-clone-defaults", "tmux-clone-defaults", f.TestCwd("clone-defaults"))
			alpha := haveGroupWithSandboxDefaults(t, f, "alpha")
			beta := haveGroupWithSandboxDefaults(t, f, "beta")
			f.HaveMember("alpha", source)
			launched, err := db.ResolveEffectiveSandboxSnapshot(alpha.ID, "")
			require.NoError(t, err)
			agentID, err := db.AgentIDForConv(source)
			require.NoError(t, err)
			require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &launched))

			body, err := json.Marshal(map[string]any{"no_copy_conv": true, "group": "beta",
				"keep_sandbox": tc.keepSandbox, "keep_environment": tc.keepEnv})
			require.NoError(t, err)
			req, _ := http.NewRequest(http.MethodPost, "/api/agents/"+source+"/clone", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
			var resp struct {
				NewConv string `json:"new_conv"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.NewConv)

			spawned, ok := f.World.SpawnSandboxPolicy(resp.NewConv)
			require.True(t, ok)
			require.NotNil(t, spawned)
			env := launchEnv(spawned)
			assert.Equal(t, tc.wantProfile, env["PROFILE_OWNER"], "profile environment at first launch")
			assert.Equal(t, tc.wantGroupEnv, env["GROUP_OWNER"], "group environment at first launch")
			groups := map[string]int64{"alpha": alpha.ID, "beta": beta.ID}
			assert.Equal(t, groups[tc.wantResolution], spawned.ResolutionGroupID)

			persisted, err := db.AgentEffectiveSandboxConfigForConv(resp.NewConv)
			require.NoError(t, err)
			require.NotNil(t, persisted)
			assert.Equal(t, groups[tc.wantResolution], persisted.ResolutionGroupID, "resume keeps the chosen profile group")
			envGroup, _ := persisted.EnvironmentGroupID()
			assert.Equal(t, groups[tc.wantGroupEnv], envGroup, "resume keeps the chosen environment group")

			sourceSnapshot, err := db.AgentEffectiveSandboxConfigForConv(source)
			require.NoError(t, err)
			assert.Equal(t, alpha.ID, sourceSnapshot.ResolutionGroupID, "the source is untouched")

			inbox, err := db.ListAgentMessagesForConv(resp.NewConv, 10)
			require.NoError(t, err)
			var briefed bool
			for _, msg := range inbox {
				briefed = briefed || strings.Contains(msg.Body, `sandbox profile from group "`+tc.wantResolution+`"`)
			}
			assert.True(t, briefed, "the clone is told where its sandbox comes from: %+v", inbox)
		})
	}
}
