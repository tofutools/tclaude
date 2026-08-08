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

func postPermissionScope(t *testing.T, f *testharness.Flow, verb string, body map[string]any) *httpResult {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/permissions/"+verb, body)))
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

func TestPermissionScopeGrantWireRoundTrip(t *testing.T) {
	f := newFlow(t)
	const target = "scope-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(target, "scoped-agent")

	grant := postPermissionScope(t, f, "grant", map[string]any{
		"target": target,
		"slug":   agentd.PermGroupsSpawn,
		"scope": map[string]any{
			"group":         []string{"dev"},
			"spawn_profile": []string{"locked", "reviewer"},
		},
	})
	require.Equal(t, http.StatusOK, grant.Code, grant.Body)
	var mutate struct {
		Scope map[string][]string `json:"scope"`
	}
	require.NoError(t, json.Unmarshal([]byte(grant.Body), &mutate))
	assert.Equal(t, []string{"dev"}, mutate.Scope["group"])
	assert.Equal(t, []string{"locked", "reviewer"}, mutate.Scope["spawn_profile"])

	agentID, err := db.AgentIDForConv(target)
	require.NoError(t, err)
	var stored string
	d, err := db.Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT scope_json FROM agent_permissions WHERE agent_id = ? AND slug = ?`,
		agentID, agentd.PermGroupsSpawn).Scan(&stored))
	assert.JSONEq(t, `{"group":["dev"],"spawn_profile":["locked","reviewer"]}`, stored)

	view := getPermissionsTarget(t, f, target, target)
	require.Equal(t, http.StatusOK, view.Code, view.Body)
	var effective struct {
		Provenance map[string]string `json:"provenance"`
	}
	require.NoError(t, json.Unmarshal([]byte(view.Body), &effective))
	assert.Equal(t, "override [group=dev spawn_profile=locked,reviewer]",
		effective.Provenance[agentd.PermGroupsSpawn])

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/permissions", nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var state struct {
		Scopes map[string]map[string]map[string][]string `json:"scopes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state))
	assert.Equal(t, []string{"dev"}, state.Scopes[target][agentd.PermGroupsSpawn]["group"])
}

func TestPermissionScopeGrantPathRejections(t *testing.T) {
	f := newFlow(t)
	const target = "scope-bbbb-cccc-dddd-0002"
	f.HaveConvWithTitle(target, "scope-rejections")

	for _, tc := range []struct {
		name string
		verb string
		body map[string]any
		want string
	}{
		{"undeclared dimension", "grant", map[string]any{"target": target, "slug": agentd.PermProcessRunsManage, "scope": map[string]any{"group": []string{"dev"}}}, "does not declare"},
		{"unknown selector", "grant", map[string]any{"target": target, "slug": agentd.PermAgentRetire, "scope": map[string]any{"target_agent": []string{"@parent"}}}, "unknown selector"},
		{"selector on wrong dimension", "grant", map[string]any{"target": target, "slug": agentd.PermGroupsSpawn, "scope": map[string]any{"group": []string{"@descendants"}}}, "unknown selector"},
		{"scoped default", "grant", map[string]any{"target": "default", "slug": agentd.PermGroupsSpawn, "scope": map[string]any{"group": []string{"dev"}}}, "not supported"},
		{"scoped deny", "deny", map[string]any{"target": target, "slug": agentd.PermGroupsSpawn, "scope": map[string]any{"group": []string{"dev"}}}, "cannot carry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := postPermissionScope(t, f, tc.verb, tc.body)
			assert.Equal(t, http.StatusBadRequest, res.Code, res.Body)
			assert.Contains(t, res.Body, tc.want)
		})
	}
}

func TestPermissionSlugsAdvertiseScopeDimensions(t *testing.T) {
	f := newFlow(t)
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodGet, "/v1/permissions/slugs", nil)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var slugs []struct {
		Slug      string   `json:"slug"`
		ScopeDims []string `json:"scope_dims"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &slugs))
	for _, slug := range slugs {
		if slug.Slug == agentd.PermGroupsSpawn {
			assert.Equal(t, []string{"group", "spawn_profile"}, slug.ScopeDims)
			return
		}
	}
	t.Fatal("groups.spawn missing from registry response")
}
