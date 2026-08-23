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
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func expectedManagedCodexAgentDirectories(base ...string) []string {
	got := append([]string(nil), base...)
	if runtime.GOOS == "linux" {
		got = append(got, "TCL_CODEX_SSH_CONFIG_DIR")
	}
	return got
}

func expectedManagedCodexEnvironmentCount(baseAgentDirectories int) int {
	if runtime.GOOS == "linux" {
		return baseAgentDirectories + 2 // config directory plus GIT_SSH_COMMAND
	}
	return baseAgentDirectories
}

func TestCodexSpawnSSHWorkaroundDefaultsOnAndCanBeDisabled(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Codex managed-sandbox SSH workaround is Linux-only")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "default-on", "harness": "codex", "sandbox": "tclaude-agent",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "body=%s", spawn.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Contains(t, snapshot.Effective.AgentDirectories, "TCL_CODEX_SSH_CONFIG_DIR")
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", true)
	firstConfigDir := sshSnapshotEnvironment(snapshot.Effective.Environment, "TCL_CODEX_SSH_CONFIG_DIR")
	require.NotEmpty(t, firstConfigDir)

	defaultSuccessor := selfReincarnate(t, f, spawn.ConvID)
	snapshot, ok = f.World.SpawnSandboxPolicy(defaultSuccessor)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	successorConfigDir := sshSnapshotEnvironment(snapshot.Effective.Environment, "TCL_CODEX_SSH_CONFIG_DIR")
	require.NotEmpty(t, successorConfigDir)
	assert.NotEqual(t, firstConfigDir, successorConfigDir,
		"a live predecessor must never share the successor's regenerated SSH directory")
	assert.Contains(t, successorConfigDir, "agent-dirs/spwn-",
		"relaunches use a generation-keyed root instead of the stable agent ID")

	require.NoError(t, db.GrantAgentPermission(defaultSuccessor, agentd.PermSelfClone, "test"))
	enabledClone := agentReq(t, f, defaultSuccessor, http.MethodPost, "/v1/whoami/clone",
		map[string]any{"no_copy_conv": true})
	require.Equalf(t, http.StatusOK, enabledClone.Code, "body=%s", enabledClone.Body.String())
	var enabledCloneResponse struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, enabledClone, &enabledCloneResponse)
	snapshot, ok = f.World.SpawnSandboxPolicy(enabledCloneResponse.NewConv)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	cloneConfigDir := sshSnapshotEnvironment(snapshot.Effective.Environment, "TCL_CODEX_SSH_CONFIG_DIR")
	require.NotEmpty(t, cloneConfigDir)
	assert.NotEqual(t, successorConfigDir, cloneConfigDir,
		"an enabled clone must not share its source agent's SSH directory")

	raw := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "raw-codex", "harness": "codex", "sandbox": "workspace-write",
		"ssh_workaround": true,
	})
	require.Equalf(t, http.StatusOK, raw.Code, "body=%s", raw.Raw)
	rawDurable, err := db.AgentRelaunchProfileForConv(raw.ConvID)
	require.NoError(t, err)
	require.NotNil(t, rawDurable)
	require.NotNil(t, rawDurable.SSHWorkaround)
	assert.False(t, *rawDurable.SSHWorkaround,
		"raw Codex containment cannot persist the managed-sandbox workaround as active")

	optedOut := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "explicit-off", "harness": "codex", "sandbox": "tclaude-agent",
		"ssh_workaround": false,
	})
	require.Equalf(t, http.StatusOK, optedOut.Code, "body=%s", optedOut.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(optedOut.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.NotContains(t, snapshot.Effective.AgentDirectories, "TCL_CODEX_SSH_CONFIG_DIR")
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)
	durable, err := db.AgentRelaunchProfileForConv(optedOut.ConvID)
	require.NoError(t, err)
	require.NotNil(t, durable)
	require.NotNil(t, durable.SSHWorkaround)
	assert.False(t, *durable.SSHWorkaround, "the explicit opt-out is frozen on the stable agent")

	require.NoError(t, db.GrantAgentPermission(optedOut.ConvID, agentd.PermSelfClone, "test"))
	clone := agentReq(t, f, optedOut.ConvID, http.MethodPost, "/v1/whoami/clone",
		map[string]any{"no_copy_conv": true})
	require.Equalf(t, http.StatusOK, clone.Code, "body=%s", clone.Body.String())
	var cloneResponse struct {
		NewConv string `json:"new_conv"`
	}
	testharness.DecodeJSON(t, clone, &cloneResponse)
	snapshot, ok = f.World.SpawnSandboxPolicy(cloneResponse.NewConv)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)

	successor := selfReincarnate(t, f, optedOut.ConvID)
	snapshot, ok = f.World.SpawnSandboxPolicy(successor)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)

	off := false
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "codex-no-ssh", Harness: "codex", SSHWorkaround: &off,
	})
	require.NoError(t, err)
	fromProfile := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "profile-off", "profile": "codex-no-ssh",
	})
	require.Equalf(t, http.StatusOK, fromProfile.Code, "body=%s", fromProfile.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(fromProfile.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)

	f.MarkOffline(fromProfile.TmuxSession)
	resumed := f.AsHuman().Resume(fromProfile.ConvID)
	require.Equalf(t, http.StatusOK, resumed.Code, "body=%s", resumed.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(fromProfile.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)

}

func TestTclaudeLayerSSHWorkaroundCoversNonCodexAndBothPacketIdentities(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the SSH workaround is Linux-only")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Cleanup(session.SetCodexEffectiveConfigProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry, string) (json.RawMessage, error) {
			return json.RawMessage(`{
				"config": {
					"model_provider": "test",
					"model_providers": {"test": {"base_url": "https://example.test/v1"}}
				},
				"origins": {}
			}`), nil
		}))
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
	t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{Detected: true, Detail: "test packet gateway"}
	}))
	verdict := func(
		_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
		_ sandboxpolicy.NetworkEngine,
	) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{
			State:           "on",
			Source:          "test tclaude-layer",
			FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
		}, nil
	}
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(verdict))
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictWithIdentityForTest(func(
		name string, posture sandboxpolicy.NetworkPosture, root sandboxpolicy.RootPosture,
		engine sandboxpolicy.NetworkEngine, _ bool,
	) (harness.LaunchOSSandbox, error) {
		return verdict(name, posture, root, engine)
	}))
	for _, profile := range []*db.SandboxProfile{
		{
			Name: "caller-id-packet",
			Network: &sandboxpolicy.NetworkRules{
				Baseline:               sandboxpolicy.NetworkBaselineDeny,
				Engine:                 sandboxpolicy.NetworkEnginePacket,
				PreserveCallerIdentity: true,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "example.test"},
					{Domain: "api.anthropic.com", Ports: []int{443}},
					{Domain: "chatgpt.com", Ports: []int{443}},
					{Domain: "auth.openai.com", Ports: []int{443}},
				},
			},
		},
		{
			Name: "namespace-root-packet",
			Network: &sandboxpolicy.NetworkRules{
				Baseline: sandboxpolicy.NetworkBaselineDeny,
				Engine:   sandboxpolicy.NetworkEnginePacket,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "example.test"},
					{Domain: "api.anthropic.com", Ports: []int{443}},
					{Domain: "chatgpt.com", Ports: []int{443}},
					{Domain: "auth.openai.com", Ports: []int{443}},
				},
			},
		},
		{
			Name: "caller-id-proxy",
			Network: &sandboxpolicy.NetworkRules{
				Baseline:               sandboxpolicy.NetworkBaselineDeny,
				Engine:                 sandboxpolicy.NetworkEngineProxy,
				PreserveCallerIdentity: true,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "example.test"},
					{Domain: "api.anthropic.com", Ports: []int{443}},
					{Domain: "chatgpt.com", Ports: []int{443}},
					{Domain: "auth.openai.com", Ports: []int{443}},
				},
			},
		},
	} {
		_, err := db.CreateSandboxProfile(profile)
		require.NoError(t, err)
	}

	launchCwd := t.TempDir()
	spawn := func(name, harnessName, profile string) testharness.SpawnResp {
		return f.AsHuman().SpawnWith("crew", map[string]any{
			"name": name, "harness": harnessName,
			"cwd":                    launchCwd,
			"sandbox_implementation": "tclaude-layer",
			"sandbox_profile":        profile,
		})
	}

	caller := spawn("caller-id", harness.CodexName, "caller-id-packet")
	require.Equalf(t, http.StatusOK, caller.Code, "body=%s", caller.Raw)
	snapshot, ok := f.World.SpawnSandboxPolicy(caller.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Contains(t, snapshot.Effective.AgentDirectories, "TCL_CODEX_SSH_CONFIG_DIR")
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", true)

	namespaceRoot := spawn("namespace-root", harness.DefaultName, "namespace-root-packet")
	require.Equalf(t, http.StatusOK, namespaceRoot.Code, "body=%s", namespaceRoot.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(namespaceRoot.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.Contains(t, snapshot.Effective.AgentDirectories, "TCL_CODEX_SSH_CONFIG_DIR")
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", true)

	proxy := spawn("proxy", harness.DefaultName, "caller-id-proxy")
	require.Equalf(t, http.StatusOK, proxy.Code, "body=%s", proxy.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(proxy.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assert.NotContains(t, snapshot.Effective.AgentDirectories, "TCL_CODEX_SSH_CONFIG_DIR")
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)

	f.MarkOffline(caller.TmuxSession)
	resumed := f.AsHuman().Resume(caller.ConvID)
	require.Equalf(t, http.StatusOK, resumed.Code, "body=%s", resumed.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(caller.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", true)

	changed := spawn("packet-profile-will-change", harness.CodexName, "caller-id-packet")
	require.Equalf(t, http.StatusOK, changed.Code, "body=%s", changed.Raw)
	durable, err := db.AgentRelaunchProfileForConv(changed.ConvID)
	require.NoError(t, err)
	require.NotNil(t, durable)
	require.NotNil(t, durable.SSHWorkaround)
	assert.True(t, *durable.SSHWorkaround, "birth freezes the opt-in, not current activation")
	profile, err := db.GetSandboxProfile("caller-id-packet")
	require.NoError(t, err)
	profile.Network.Engine = sandboxpolicy.NetworkEngineProxy
	require.NoError(t, db.UpdateSandboxProfile(profile))
	f.MarkOffline(changed.TmuxSession)
	resumed = f.AsHuman().Resume(changed.ConvID)
	require.Equalf(t, http.StatusOK, resumed.Code, "body=%s", resumed.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(changed.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", false)
	durable, err = db.AgentRelaunchProfileForConv(changed.ConvID)
	require.NoError(t, err)
	require.NotNil(t, durable)
	require.NotNil(t, durable.SSHWorkaround)
	assert.True(t, *durable.SSHWorkaround, "proxy activation must not erase the durable opt-in")

	profile, err = db.GetSandboxProfile("caller-id-packet")
	require.NoError(t, err)
	profile.Network.Engine = sandboxpolicy.NetworkEnginePacket
	require.NoError(t, db.UpdateSandboxProfile(profile))
	currentSession, err := db.FindSessionByConvID(changed.ConvID)
	require.NoError(t, err)
	require.NotNil(t, currentSession)
	f.MarkOffline(currentSession.TmuxSession)
	resumed = f.AsHuman().Resume(changed.ConvID)
	require.Equalf(t, http.StatusOK, resumed.Code, "body=%s", resumed.Raw)
	snapshot, ok = f.World.SpawnSandboxPolicy(changed.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot)
	assertSnapshotHasEnvironment(t, snapshot.Effective.Environment, "GIT_SSH_COMMAND", true)
}

func assertSnapshotHasEnvironment(t *testing.T, entries []sandboxpolicy.EnvironmentEntry, name string, want bool) {
	t.Helper()
	assert.Equal(t, want, sshSnapshotEnvironment(entries, name) != "")
}

func sshSnapshotEnvironment(entries []sandboxpolicy.EnvironmentEntry, name string) string {
	for _, entry := range entries {
		if entry.Name == name {
			return entry.Value
		}
	}
	return ""
}
