package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The collab surfaces are opt-in: with features.collab off every one answers
// with the same stable 404, for agents and the dashboard alike, and the
// snapshot says so. Turning the flag on takes effect without a restart.
func TestCollabSurfacesAreAbsentUntilOptedIn(t *testing.T) {
	f := newFlow(t)
	const agent = "coll-aaaa-bbbb-cccc-000000000001"
	f.HaveGroup("design")
	f.HaveMember("design", agent)
	f.HaveAliveSession(agent, "coll", "tclaude-coll", f.TestCwd("work"))
	require.NoError(t, db.GrantAgentPermission(agent, agentd.PermHumanNotify, "test"))
	dash := agentd.BuildDashboardHandlerForTest()

	disabled := func(t *testing.T, code int, body string, name string) {
		t.Helper()
		require.Equal(t, http.StatusNotFound, code, "%s: %s", name, body)
		var out struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &out), name)
		assert.Contains(t, body, "collab_disabled", name)
		assert.Contains(t, body, config.CollabDisabledMessage, name)
	}
	agentRoutes := []struct{ method, path string }{
		{http.MethodGet, "/v1/fleet"},
		{http.MethodGet, "/v1/dashboard/view"},
		{http.MethodPost, "/v1/dashboard/cue"},
		{http.MethodGet, "/v1/boards"},
		{http.MethodPost, "/v1/boards"},
		{http.MethodGet, "/v1/boards/map"},
		{http.MethodPatch, "/v1/boards/map"},
		{http.MethodGet, "/v1/boards/map/snapshot"},
	}
	for _, route := range agentRoutes {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, route.method, route.path, map[string]any{}), agent)
		rec := testharness.Serve(f.Mux, r)
		disabled(t, rec.Code, rec.Body.String(), route.method+" "+route.path)
	}
	dashRoutes := []struct{ method, path string }{
		{http.MethodGet, "/api/boards"},
		{http.MethodPost, "/api/boards"},
		{http.MethodGet, "/api/boards/map"},
		{http.MethodPatch, "/api/boards/map"},
		{http.MethodDelete, "/api/boards/map"},
		{http.MethodPost, "/api/companion/view"},
		{http.MethodGet, "/api/companion/agt_x/peek"},
		{http.MethodGet, "/api/companion/agt_x/speech"},
	}
	for _, route := range dashRoutes {
		rec := testharness.Serve(dash, boardDashReq(t, route.method, route.path, map[string]any{}))
		disabled(t, rec.Code, rec.Body.String(), route.method+" "+route.path)
	}

	// A decision is refused, a plain notification still goes through.
	notify := func(body map[string]any) (int, string) {
		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", body), agent))
		return rec.Code, rec.Body.String()
	}
	code, body := notify(map[string]any{"body": "pick", "options": []string{"a", "b"}})
	disabled(t, code, body, "decision")
	code, body = notify(map[string]any{"body": "fyi"})
	require.Equal(t, http.StatusOK, code, body)

	snapshot := func() bool {
		rec := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		var s struct {
			Enabled bool `json:"collab_enabled"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
		return s.Enabled
	}
	assert.False(t, snapshot())

	enableCollab(t)
	assert.True(t, snapshot())
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/boards", nil), agent))
	assert.NotEqual(t, http.StatusNotFound, rec.Code, rec.Body.String())
	code, body = notify(map[string]any{"body": "pick", "options": []string{"a", "b"}})
	assert.Equal(t, http.StatusOK, code, body)
}
