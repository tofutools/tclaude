package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestGroupDefaultSpawnSelectionIsExclusive(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	dir := t.TempDir()
	for _, group := range []string{"alpha", "beta"} {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/groups/"+group, map[string]any{"default_cwd": dir})))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	for _, group := range []string{"alpha", "beta"} {
		rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/groups/"+group, map[string]any{"default_spawn_group": true})))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	alpha, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	beta, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	assert.False(t, alpha.DefaultSpawnGroup)
	assert.True(t, beta.DefaultSpawnGroup)

	_, err = db.SetAgentGroupDefaultCwd("beta", "")
	require.NoError(t, err)
	beta, err = db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	assert.False(t, beta.DefaultSpawnGroup, "clearing the directory also clears its default selection")
}

func TestGroupDefaultSpawnRequiresDirectory(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha", map[string]any{"default_spawn_group": true})))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "default spawn directory")
}
