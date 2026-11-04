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
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A resume and a reincarnation say, field by field, what their sandbox
// differs in from the one stored at the last launch, naming what changed and
// never a value (tcl-a2e791 slice 6b).

func TestRelaunchEcho_ResumeNamesAnEditedProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	id, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "worker-policy",
		Environment: []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"}, {Name: "ROTATE", Value: "old-secret"}}})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "worker-policy"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	profile, err := db.GetSandboxProfileByID(id)
	require.NoError(t, err)
	profile.Environment = []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"},
		{Name: "ROTATE", Value: "new-secret"}, {Name: "ADDED", Value: "x"}}
	require.NoError(t, db.UpdateSandboxProfile(profile))
	f.MarkOffline(spawn.TmuxSession)

	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	joined := strings.Join(resume.Changes, "\n")
	assert.Contains(t, joined, `sandbox profile edited since the last launch: "worker-policy"`)
	assert.Contains(t, joined, "environment: added ADDED; changed ROTATE")
	assert.NotContains(t, joined, "secret", "values are never echoed")
}

func TestRelaunchEcho_ResumeNamesAGroupEnvironmentChange(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.SetAgentGroupEnvironment("crew", []sandboxpolicy.EnvironmentEntry{{Name: "TEAM", Value: "a"}})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	_, err = db.SetAgentGroupEnvironment("crew", []sandboxpolicy.EnvironmentEntry{{Name: "TOKEN", Value: "t"}})
	require.NoError(t, err)
	f.MarkOffline(spawn.TmuxSession)

	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	assert.Contains(t, resume.Changes, "group environment: added TOKEN; removed TEAM")
}

func TestRelaunchEcho_ReincarnateNamesAnEditedProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	id, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "worker-policy",
		Environment: []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"}}})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker", "sandbox_profile": "worker-policy"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	profile, err := db.GetSandboxProfileByID(id)
	require.NoError(t, err)
	profile.Environment = nil
	require.NoError(t, db.UpdateSandboxProfile(profile))

	rec := agentReq(t, f, spawn.ConvID, http.MethodPost, "/v1/whoami/reincarnate", map[string]any{"follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, rec.Code, "reincarnate body=%s", rec.Body.String())
	var response testharness.ReincarnateResp
	testharness.DecodeJSON(t, rec, &response)
	assert.Contains(t, response.Changes, "environment: removed KEEP")
}

// Pinned: a relaunch where nothing changed echoes nothing, on both paths and
// with an agent directory and a sandbox profile in play.
func TestRelaunchEcho_NothingChangedEchoesNothing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "steady",
		Environment: []db.SandboxEnvironmentEntry{{Name: "KEEP", Value: "1"}}, AgentDirectories: []string{"GOCACHE"}})
	require.NoError(t, err)
	for _, body := range []map[string]any{
		{"name": "plain"},
		{"name": "profiled", "sandbox_profile": "steady"},
		{"name": "codex", "sandbox_profile": "steady", "harness": "codex", "sandbox": "tclaude-agent"},
	} {
		spawn := f.AsHuman().SpawnWith("crew", body)
		require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
		f.MarkOffline(spawn.TmuxSession)
		resume := f.AsHuman().Resume(spawn.ConvID)
		f.AssertResumeSpawned(resume)
		assert.Emptyf(t, resume.Changes, "%v", body)

		rec := agentReq(t, f, spawn.ConvID, http.MethodPost, "/v1/whoami/reincarnate", map[string]any{"follow_up": "carry on"})
		require.Equalf(t, http.StatusOK, rec.Code, "reincarnate body=%s", rec.Body.String())
		var response testharness.ReincarnateResp
		testharness.DecodeJSON(t, rec, &response)
		assert.Emptyf(t, response.Changes, "%v", body)
	}
}

// A filtered-network profile under the tclaude layer regenerates the Codex SSH
// workaround (its config directory and command) on every launch; none of that
// is a change.
func TestRelaunchEcho_FilteredNetworkRelaunchEchoesNothing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Cleanup(session.SetCodexEffectiveConfigProbeForTest(
		func(string, []sandboxpolicy.EnvironmentEntry, string) (json.RawMessage, error) {
			return json.RawMessage(`{"config": {"model_provider": "test",
				"model_providers": {"test": {"base_url": "https://example.test/v1"}}}, "origins": {}}`), nil
		}))
	f := newFlow(t)
	f.HaveGroup("crew")
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
	t.Cleanup(agentd.SetFilteredNetworkPrerequisiteForTest(func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{Detected: true, Detail: "test packet gateway"}
	}))
	verdict := func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture,
		_ sandboxpolicy.NetworkEngine) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{State: "on", Source: "test tclaude-layer",
			FilteredNetwork: posture == sandboxpolicy.NetworkFiltered}, nil
	}
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictForTest(verdict))
	t.Cleanup(agentd.SetTclaudeLayerAccessVerdictWithIdentityForTest(func(name string,
		posture sandboxpolicy.NetworkPosture, root sandboxpolicy.RootPosture,
		engine sandboxpolicy.NetworkEngine, _ bool) (harness.LaunchOSSandbox, error) {
		return verdict(name, posture, root, engine)
	}))
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "filtered",
		Network: &sandboxpolicy.NetworkRules{Baseline: sandboxpolicy.NetworkBaselineDeny,
			Engine: sandboxpolicy.NetworkEnginePacket,
			Allow: []sandboxpolicy.NetworkAllowEntry{{Domain: "example.test"},
				{Domain: "chatgpt.com", Ports: []int{443}}, {Domain: "auth.openai.com", Ports: []int{443}}}}})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "filtered", "harness": harness.CodexName,
		"cwd": t.TempDir(), "sandbox_implementation": "tclaude-layer", "sandbox_profile": "filtered"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	f.AssertResumeSpawned(resume)
	assert.Empty(t, resume.Changes)

	rec := agentReq(t, f, spawn.ConvID, http.MethodPost, "/v1/whoami/reincarnate", map[string]any{"follow_up": "carry on"})
	require.Equalf(t, http.StatusOK, rec.Code, "reincarnate body=%s", rec.Body.String())
	var response testharness.ReincarnateResp
	testharness.DecodeJSON(t, rec, &response)
	assert.Empty(t, response.Changes)
}
