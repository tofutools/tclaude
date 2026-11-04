package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
	for _, body := range []map[string]any{{"name": "plain"}, {"name": "profiled", "sandbox_profile": "steady"}} {
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
