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

func TestDashboardGroupPermissionsReplaceAndValidate(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("trusted")
	const memberConv = "gped-1111-2222-3333-4444"
	f.HaveMember("trusted", memberConv)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	mux := agentd.BuildDashboardHandlerForTest()

	r := testharness.JSONRequest(t, http.MethodPatch, "/api/groups/trusted",
		map[string]any{"permissions": []string{agentd.PermHumanNotify, agentd.PermGroupsSpawn}})
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got, err := db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{agentd.PermGroupsSpawn, agentd.PermHumanNotify}, got)
	snapshot := fetchSnapshotOnly(t, mux)
	require.Len(t, snapshot.Groups, 1)
	assert.Equal(t, got, snapshot.Groups[0].Permissions, "group policy surfaces in the dashboard snapshot")
	agentRow := findAgent(snapshot.Agents, memberConv)
	require.NotNil(t, agentRow)
	assert.Contains(t, agentRow.Effective, agentd.PermHumanNotify, "effective readback includes group grants")

	// Typos fail without replacing the already-persisted group policy.
	r = testharness.JSONRequest(t, http.MethodPatch, "/api/groups/trusted",
		map[string]any{"descr": "must not partially apply", "permissions": []string{"human.notfiy"}})
	rec = testharness.Serve(mux, r)
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	got, err = db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{agentd.PermGroupsSpawn, agentd.PermHumanNotify}, got)
	unchanged, err := db.GetAgentGroupByName("trusted")
	require.NoError(t, err)
	assert.Empty(t, unchanged.Descr, "invalid mixed PATCH applies no earlier field")

	// An explicit empty list is distinct from omission and clears the policy.
	r = testharness.JSONRequest(t, http.MethodPatch, "/api/groups/trusted",
		map[string]any{"permissions": []string{}})
	rec = testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	got, err = db.ListAgentGroupPermissions(g.ID)
	require.NoError(t, err)
	assert.Empty(t, got)
}
