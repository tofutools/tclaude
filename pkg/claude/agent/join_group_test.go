package agent

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Ensures the agent package's init() hooks runJoinGroup into session,
// so `tclaude --join-group …` is reachable from runNew without a
// session→agent import cycle.
func TestJoinGroupHandlerWired(t *testing.T) {
	require.NotNil(t, session.JoinGroupHandler, "session.JoinGroupHandler is nil; agent package init() did not run")
}

func TestAutomaticGroupForDirCanonicalMatch(t *testing.T) {
	setupTestDB(t)
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-link")
	require.NoError(t, os.Symlink(real, alias))
	_, err := db.CreateAgentGroup("project-team", "")
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("project-team", alias+string(os.PathSeparator)+".")
	require.NoError(t, err)

	got, err := automaticGroupForDir(&session.NewParams{Dir: real, AutoJoinGroup: true})
	require.NoError(t, err)
	assert.Equal(t, "project-team", got)
}

func TestAutomaticGroupForDirNoMatchAndAmbiguity(t *testing.T) {
	setupTestDB(t)
	dir := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(dir, 0o755))
	got, err := automaticGroupForDir(&session.NewParams{Dir: dir, AutoJoinGroup: true})
	require.NoError(t, err)
	assert.Empty(t, got)

	for _, name := range []string{"alpha", "beta"} {
		_, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		_, err = db.SetAgentGroupDefaultCwd(name, dir)
		require.NoError(t, err)
	}
	_, err = automaticGroupForDir(&session.NewParams{Dir: dir, AutoJoinGroup: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple groups")
	assert.Contains(t, err.Error(), "--join-group")
}

func TestAutomaticGroupForDirNoMatchPreservesLogicalDir(t *testing.T) {
	setupTestDB(t)
	real := t.TempDir()
	alias := filepath.Join(t.TempDir(), "repo-link")
	require.NoError(t, os.Symlink(real, alias))
	params := &session.NewParams{Dir: alias, AutoJoinGroup: true}

	got, err := automaticGroupForDir(params)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, alias, params.Dir)
}

func TestSpawnParamsForJoinedSessionPreservesExplicitCodexAppServerFalse(t *testing.T) {
	params := &session.NewParams{
		CodexAppServer:          false,
		CodexAppServerSpecified: true,
	}
	spawn := spawnParamsForJoinedSession(params, "team")
	assert.False(t, spawn.CodexAppServer)
	assert.True(t, spawn.codexAppServerSpecified)
}

func TestAvailableDirectoryGroupName(t *testing.T) {
	groups := []*db.AgentGroup{{Name: "repo"}, {Name: "repo-2"}, {Name: "other"}}
	assert.Equal(t, "repo-3", availableDirectoryGroupName("/work/repo", groups))
	assert.Equal(t, "group", availableDirectoryGroupName("/", groups))
}

func TestAutomaticGroupForDirCreatesMissingGroup(t *testing.T) {
	setupTestDB(t)
	dir := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.Mkdir(dir, 0o755))
	previousAvailable := DaemonAvailableImpl
	previousRequest := DaemonRequestImpl
	DaemonAvailableImpl = func() bool { return true }
	t.Cleanup(func() {
		DaemonAvailableImpl = previousAvailable
		DaemonRequestImpl = previousRequest
	})

	DaemonRequestImpl = func(method, path string, in, out any, _ DaemonOpts) error {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/groups", path)
		body, ok := in.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "repo", body["name"])
		canonical, err := canonicalGroupDir(dir)
		require.NoError(t, err)
		assert.Equal(t, canonical, body["default_cwd"])
		data, err := json.Marshal(map[string]any{"name": "repo"})
		require.NoError(t, err)
		return json.Unmarshal(data, out)
	}

	got, err := automaticGroupForDir(&session.NewParams{
		Dir: dir + string(os.PathSeparator) + ".", AutoJoinOrCreateGroup: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "repo", got)
}

// Mutually-exclusive flags should fail before any daemon contact.
// Covers the two "we own the spawn label" guarantees: --resume conflicts
// (we always create a fresh conv, never resume), --label conflicts
// (the daemon picks `spwn-XXXXXX`).
func TestJoinGroupRejectsConflictingFlags(t *testing.T) {
	cases := []struct {
		name   string
		params session.NewParams
		want   string
	}{
		{
			name:   "resume incompatible",
			params: session.NewParams{JoinGroup: "team", Resume: "abc123"},
			want:   "--resume",
		},
		{
			name:   "label incompatible",
			params: session.NewParams{JoinGroup: "team", Label: "my-label"},
			want:   "--label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := RunJoinGroup(&tc.params)
			require.Error(t, err, "expected error, got nil")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
