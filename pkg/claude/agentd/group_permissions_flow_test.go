package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardGroupPermissionScopesRoundTrip(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("trusted")
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	mux := agentd.BuildDashboardHandlerForTest()

	r := testharness.JSONRequest(t, http.MethodPatch, "/api/groups/trusted", map[string]any{
		"permissions": []any{map[string]any{
			"slug":  agentd.PermRoutesPublish,
			"scope": map[string][]string{"group": {"trusted"}},
		}},
	})
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	rows, err := db.ListAgentGroupPermissionRows(g.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, agentd.PermRoutesPublish, rows[0].Slug)
	assert.JSONEq(t, `{"group":["trusted"]}`, rows[0].ScopeJSON)

	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var snapshot struct {
		Groups []struct {
			Permissions      []string                       `json:"permissions"`
			PermissionScopes map[string]map[string][]string `json:"permission_scopes"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	require.Len(t, snapshot.Groups, 1)
	assert.Equal(t, []string{agentd.PermRoutesPublish}, snapshot.Groups[0].Permissions,
		"the legacy slug list remains backward compatible")
	assert.Equal(t, map[string][]string{"group": {"trusted"}},
		snapshot.Groups[0].PermissionScopes[agentd.PermRoutesPublish])

	// Posting the bare-string arm deliberately clears the scope. This is how
	// the shared editor represents a grant whose last scope chip was removed.
	r = testharness.JSONRequest(t, http.MethodPatch, "/api/groups/trusted",
		map[string]any{"permissions": []string{agentd.PermRoutesPublish}})
	rec = testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	rows, err = db.ListAgentGroupPermissionRows(g.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Empty(t, rows[0].ScopeJSON)
}

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
