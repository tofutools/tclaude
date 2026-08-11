package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	params := &session.NewParams{Dir: alias, AutoJoinGroup: true}
	got, err := automaticGroupForDir(params)
	require.NoError(t, err)
	assert.Equal(t, "project-team", got)
	assert.Equal(t, alias, params.Dir, "discovery must preserve logical cwd until a daemon spawn is confirmed")
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
	assert.Contains(t, err.Error(), "⚙")

	_, err = db.SetAgentGroupDefaultSpawn("beta", true)
	require.NoError(t, err)
	got, err = automaticGroupForDir(&session.NewParams{Dir: dir, AutoJoinGroup: true})
	require.NoError(t, err)
	assert.Equal(t, "beta", got)
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

func TestSpawnParamsForJoinedSessionPreservesOwner(t *testing.T) {
	spawn := spawnParamsForJoinedSession(&session.NewParams{Owner: true}, "team")
	assert.True(t, spawn.Owner)
}

func TestAvailableDirectoryGroupName(t *testing.T) {
	groups := []*db.AgentGroup{{Name: "repo"}, {Name: "repo-2"}, {Name: "other"}}
	assert.Equal(t, "repo-3", availableDirectoryGroupName("/work/repo", groups))
	assert.Equal(t, "group", availableDirectoryGroupName("/", groups))
}

func TestConfirmSoloWithoutDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{name: "yes short", in: "y\n", want: true},
		{name: "yes long case insensitive", in: " YES \n", want: true},
		{name: "default no", in: "\n", want: false},
		{name: "eof defaults no", want: false},
		{name: "other defaults no", in: "sure\n", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			assert.Equal(t, tc.want, confirmSoloWithoutDaemon(strings.NewReader(tc.in), &out))
			assert.Contains(t, out.String(), "--no-daemon")
			assert.Contains(t, out.String(), "agentd serve --help")
		})
	}
}

func TestAutomaticDaemonFallback(t *testing.T) {
	var out bytes.Buffer
	err := automaticDaemonFallback(strings.NewReader("y\n"), &out, false)
	require.Error(t, err)
	assert.NotErrorIs(t, err, session.ErrNoAutomaticGroupMatch)
	assert.Contains(t, err.Error(), "non-interactively")
	assert.Empty(t, out.String(), "a non-interactive launch must not emit a prompt")

	out.Reset()
	err = automaticDaemonFallback(strings.NewReader("n\n"), &out, true)
	require.Error(t, err)
	assert.NotErrorIs(t, err, session.ErrNoAutomaticGroupMatch)
	assert.Contains(t, err.Error(), "canceled")

	out.Reset()
	err = automaticDaemonFallback(strings.NewReader("yes\n"), &out, true)
	assert.ErrorIs(t, err, session.ErrNoAutomaticGroupMatch)
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
