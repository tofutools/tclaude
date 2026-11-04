package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scribe and Steward summons resolve their launch like every other daemon
// composed launch (tcl-a2e791 slice 3, operator Q8): the group's and the
// workspace's sandbox profiles and environment apply. Nothing configured
// leaves them exactly as before: no sandbox snapshot at all.

func summonNamedScribe(t *testing.T, f *testharness.Flow, name string, scope map[string]any) *httptestResult {
	t.Helper()
	stubScribeTerminal(t)
	body := map[string]any{"name": name, "slugs": []string{agentd.PermTemplatesManage}, "brief": "Edit things."}
	if scope != nil {
		body = map[string]any{"name": name, "scope": scope, "brief": "Help."}
	}
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/scribe", body)))
	res := &httptestResult{Code: rec.Code, Body: rec.Body.String()}
	if rec.Code == http.StatusOK {
		res.ConvID = decodeScribeResp(t, rec).ConvID
	}
	return res
}

type httptestResult struct {
	Code   int
	Body   string
	ConvID string
}

func scribeLaunchSnapshotPresent(t *testing.T, f *testharness.Flow, conv string) bool {
	t.Helper()
	snapshot, ok := f.World.SpawnSandboxPolicy(conv)
	require.Truef(t, ok, "no launch recorded for %s", conv)
	return snapshot != nil
}

// Review check 1: nothing configured leaves the scribe and the Steward
// without a sandbox snapshot, as they always launched.
func TestScribeLaunch_NothingConfiguredHasNoSnapshot(t *testing.T) {
	f := newFlow(t)
	scribe := summonNamedScribe(t, f, "plain-scribe", nil)
	require.Equalf(t, http.StatusOK, scribe.Code, "summon body=%s", scribe.Body)
	assert.False(t, scribeLaunchSnapshotPresent(t, f, scribe.ConvID))

	steward := summonNamedScribe(t, f, "steward", map[string]any{"kind": "steward"})
	require.Equalf(t, http.StatusOK, steward.Code, "summon body=%s", steward.Body)
	assert.False(t, scribeLaunchSnapshotPresent(t, f, steward.ConvID))
}

// Review check 2: with a workspace sandbox profile and a group environment,
// the scribe gets both, and still launches in its fixed workdir with that
// workdir trusted.
func TestScribeLaunch_GetsWorkspaceSandboxProfileAndGroupEnv(t *testing.T) {
	f := newFlow(t)
	first := summonNamedScribe(t, f, "ruled-scribe", nil)
	require.Equalf(t, http.StatusOK, first.Code, "summon body=%s", first.Body)

	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "workspace-rules",
		Environment: []db.SandboxEnvironmentEntry{{Name: "FROM_PROFILE", Value: "yes"}}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("workspace-rules"))
	_, err = db.SetAgentGroupEnvironment("ruled-scribe", []db.SandboxEnvironmentEntry{{Name: "FROM_GROUP", Value: "yes"}})
	require.NoError(t, err)

	scribe := summonNamedScribe(t, f, "ruled-scribe", nil)
	require.Equalf(t, http.StatusOK, scribe.Code, "summon body=%s", scribe.Body)
	snapshot, ok := f.World.SpawnSandboxPolicy(scribe.ConvID)
	require.True(t, ok)
	require.NotNil(t, snapshot, "the workspace sandbox profile applies to a scribe")
	var applied []string
	for _, a := range snapshot.Applied {
		applied = append(applied, a.Name)
	}
	assert.Contains(t, applied, "workspace-rules")
	assert.Contains(t, snapshot.Effective.Environment, db.SandboxEnvironmentEntry{Name: "FROM_PROFILE", Value: "yes"})
	assert.Contains(t, snapshot.LaunchEnvironment, db.SandboxEnvironmentEntry{Name: "FROM_GROUP", Value: "yes"})

	workdir := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	sessions, err := db.FindSessionsByConvID(scribe.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	assert.Equal(t, workdir, sessions[0].Cwd, "a scribe still works in its fixed workdir")
	assert.True(t, claudeDirTrusted(t, filepath.Join(f.World.HomeDir, ".claude", ".claude.json"), workdir), "the scribe workdir is pre-trusted")
}

// Review check 3: a sandbox profile that denies the scribe's fixed workdir
// refuses the summon with an error naming the profile and the path, and
// leaves no workdir behind.
func TestScribeLaunch_DenyingProfileGivesClearError(t *testing.T) {
	f := newFlow(t)
	workdir := filepath.Join(f.World.HomeDir, ".tclaude", "scribe")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "no-tclaude",
		Filesystem: []db.SandboxFilesystemGrant{{Path: filepath.Join(f.World.HomeDir, ".tclaude"), Access: "deny"}}})
	require.NoError(t, err)
	require.NoError(t, db.SetGlobalSandboxProfile("no-tclaude"))

	res := summonNamedScribe(t, f, "denied-scribe", nil)
	require.Equalf(t, http.StatusUnprocessableEntity, res.Code, "summon body=%s", res.Body)
	assert.Contains(t, res.Body, "sandbox_denies_workdir")
	assert.Contains(t, res.Body, `\"no-tclaude\"`)
	assert.Contains(t, res.Body, workdir)
	_, statErr := os.Stat(workdir)
	assert.True(t, os.IsNotExist(statErr), "a refused summon creates no workdir")
}
