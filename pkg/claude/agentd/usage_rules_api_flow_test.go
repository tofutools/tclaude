package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func usageCall(t *testing.T, f *testharness.Flow, asConv, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := testharness.JSONRequest(t, method, path, body)
	if asConv == "" {
		req = agentd.AsHumanPeer(req)
	} else {
		req = agentd.AsAgentPeer(req, asConv)
	}
	rec := httptest.NewRecorder()
	f.Mux.ServeHTTP(rec, req)
	return rec
}

var budgetRule = map[string]any{
	"name": "budget", "period": "lifetime", "budget": 20,
	"steps": []map[string]any{
		{"at": 80, "action": "notify", "notify": []string{"agent", "operator"}},
		{"at": 100, "action": "wrap_up"},
	},
}

// Scenario: the operator builds a usage profile and a limit through the API,
// assigns the profile as the global default and adjusts one agent; the
// overview reflects each change. Agents can read but not change anything.
func TestUsageRulesAPI_ManageAndRead(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetHumanMessageNotifierForTest(func(_, _, _, _, _ string) {}))
	f.HaveGroup("alpha")
	const worker = "work-aaaa-bbbb-cccc-111111111111"
	f.HaveMember("alpha", worker)
	workerAgent, _, err := db.EnsureAgentForConv(worker, "spawn")
	require.NoError(t, err)

	rec := usageCall(t, f, "", http.MethodPost, "/v1/usage/profiles", map[string]any{"name": "default", "rules": []any{budgetRule}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/profiles", map[string]any{"name": "Default", "rules": []any{budgetRule}})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/profiles", map[string]any{"name": "bad", "rules": []any{map[string]any{"name": "x", "period": "lifetime", "budget": 5, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}}})
	require.Equal(t, http.StatusBadRequest, rec.Code, "block_spawn is limit-only: %s", rec.Body.String())

	rec = usageCall(t, f, "", http.MethodPut, "/v1/usage/assignments", map[string]any{"scope": "global", "profiles": []string{"default"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/limits", map[string]any{"scope": "group", "group": "alpha", "rule": map[string]any{
		"name": "team", "period": "week", "budget": 100, "steps": []any{map[string]any{"at": 100, "action": "block_spawn"}}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var limit db.UsageLimit
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &limit))

	for _, bad := range []string{"profile:1:nope", "profile:9:budget", "budget", "profile:default"} {
		rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/agents/"+worker+"/adjust", map[string]any{"rule_key": bad, "extra_budget": 5})
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", bad, rec.Body.String())
	}
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/agents/"+worker+"/adjust", map[string]any{"exempt": true, "rule_key": "budget", "extra_budget": 5})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	overrides, err := db.ListUsageAgentOverrides()
	require.NoError(t, err)
	require.Empty(t, overrides, "a rejected adjustment writes nothing, not even its valid half")
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/agents/"+worker+"/adjust", map[string]any{"snooze_minutes": 30, "rule_key": "profile:default:BUDGET", "extra_budget": 5})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = usageCall(t, f, "", http.MethodPost, "/v1/usage/profiles", map[string]any{"name": "42", "rules": []any{budgetRule}})
	require.Equal(t, http.StatusBadRequest, rec.Code, "all-digit names are ambiguous with ids: %s", rec.Body.String())

	// An agent may read, even its own view, but not change anything.
	require.NoError(t, db.GrantAgentPermission(worker, agentd.PermUsageRead, "test"))
	rec = usageCall(t, f, worker, http.MethodGet, "/v1/usage/rules", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var agentView struct {
		Limits    []json.RawMessage `json:"limits"`
		Overrides []json.RawMessage `json:"overrides"`
		Agents    []json.RawMessage `json:"agents"`
		Statuses  []json.RawMessage `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &agentView))
	assert.Len(t, agentView.Limits, 1, "an agent sees the rules")
	assert.Empty(t, agentView.Overrides, "but not other agents' adjustments")
	assert.Empty(t, agentView.Agents)
	assert.Empty(t, agentView.Statuses)

	rec = usageCall(t, f, "", http.MethodGet, "/v1/usage/rules", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var overview struct {
		Profiles []struct {
			Name string `json:"name"`
		} `json:"profiles"`
		Limits []struct {
			Group string `json:"group"`
		} `json:"limits"`
		Assignments []struct {
			Scope       string `json:"scope"`
			ProfileName string `json:"profile_name"`
		} `json:"assignments"`
		Overrides []db.UsageAgentOverride `json:"overrides"`
		Groups    []string                `json:"groups"`
		Agents    []struct {
			ID string `json:"id"`
		} `json:"agents"`
		Windows []string `json:"windows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &overview))
	assert.Equal(t, []string{"alpha"}, overview.Groups, "the editor offers the groups to assign to")
	require.NotEmpty(t, overview.Agents)
	assert.Equal(t, workerAgent, overview.Agents[0].ID)
	assert.NotNil(t, overview.Windows)
	require.Len(t, overview.Profiles, 1)
	require.Len(t, overview.Limits, 1)
	assert.Equal(t, "alpha", overview.Limits[0].Group)
	require.Len(t, overview.Assignments, 1)
	assert.Equal(t, "default", overview.Assignments[0].ProfileName)
	require.Len(t, overview.Overrides, 1)
	assert.Equal(t, 5.0, overview.Overrides[0].Extensions["profile:1:budget"])
	assert.False(t, overview.Overrides[0].SnoozeUntil.IsZero())

	rec = usageCall(t, f, worker, http.MethodGet, "/v1/usage/self", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var self struct {
		Layer    string `json:"layer"`
		Profiles []struct {
			Name string `json:"name"`
		} `json:"profiles"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &self))
	assert.Equal(t, "global", self.Layer)
	require.Len(t, self.Profiles, 1)

	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/v1/usage/profiles"},
		{http.MethodPut, "/v1/usage/assignments"},
		{http.MethodDelete, "/v1/usage/limits/" + strconv.FormatInt(limit.ID, 10)},
		{http.MethodPost, "/v1/usage/agents/" + worker + "/adjust"},
	} {
		rec = usageCall(t, f, worker, call.method, call.path, map[string]any{})
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s %s: %s", call.method, call.path, rec.Body.String())
	}

	rec = usageCall(t, f, "", http.MethodDelete, "/v1/usage/profiles/default", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assignments, err := db.ListUsageProfileAssignments()
	require.NoError(t, err)
	assert.Empty(t, assignments, "deleting a profile drops its assignments")
}

// Scenario: a spawn names usage profiles explicitly (human only), or picks
// up the ones linked to its spawn profile; either becomes the new agent's
// own assignment, which outranks the group and global layers.
func TestUsageRulesAPI_SpawnAssignsUsageProfiles(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	strict := &db.UsageProfile{Name: "strict"}
	require.NoError(t, db.CreateUsageProfile(strict))
	cheap := &db.UsageProfile{Name: "cheap"}
	require.NoError(t, db.CreateUsageProfile(cheap))

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w1", "usage_profiles": []string{"strict"}})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Raw)
	assignments, err := db.ListUsageProfileAssignments()
	require.NoError(t, err)
	require.Len(t, assignments, 1)
	assert.Equal(t, db.UsageScopeAgent, assignments[0].Scope)
	assert.Equal(t, strict.ID, assignments[0].ProfileID)
	assert.Equal(t, "spawn", assignments[0].Source)

	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w2", "usage_profiles": []string{"nope"}})
	require.Equal(t, http.StatusBadRequest, resp.Code, "body=%s", resp.Raw)

	sp := &db.SpawnProfile{Name: "reviewer"}
	_, err = db.CreateSpawnProfile(sp)
	require.NoError(t, err)
	rec := usageCall(t, f, "", http.MethodPut, "/v1/usage/assignments", map[string]any{"scope": "spawn_profile", "target": "reviewer", "profiles": []string{"cheap"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resp = f.AsHuman().SpawnWith("alpha", map[string]any{"alias": "w3", "profile": "reviewer"})
	require.Equal(t, http.StatusOK, resp.Code, "body=%s", resp.Raw)
	assignments, err = db.ListUsageProfileAssignments()
	require.NoError(t, err)
	var linked []db.UsageProfileAssignment
	for _, a := range assignments {
		if a.Scope == db.UsageScopeAgent && a.ProfileID == cheap.ID {
			linked = append(linked, a)
		}
	}
	require.Len(t, linked, 1)
	assert.Equal(t, "spawn profile reviewer", linked[0].Source)
}
