package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardGuideViewAndCues(t *testing.T) {
	f := newFlow(t)
	agentd.ResetDashboardGuideForTest()
	t.Cleanup(agentd.ResetDashboardGuideForTest)
	f.HaveConvWithTitle("steward-conv", "steward")
	dash := agentd.BuildDashboardHandlerForTest()
	postView := func(body map[string]any) int {
		req := testharness.JSONRequest(t, http.MethodPost, "/api/companion/view", body)
		req.Header.Set("Origin", "http://127.0.0.1:0")
		return testharness.Serve(dash, req).Code
	}

	// Nothing is readable or postable without the slugs.
	require.Equal(t, http.StatusForbidden, accountQuery(t, f, "steward-conv", "/v1/dashboard/view").Code)
	cue := func(body map[string]any) int {
		r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/dashboard/cue", body), "steward-conv")
		return testharness.Serve(f.Mux, r).Code
	}
	require.Equal(t, http.StatusForbidden, cue(map[string]any{"tab": "groups"}))
	require.NoError(t, db.GrantAgentPermission("steward-conv", agentd.PermDashboardViewRead, "test"))
	require.NoError(t, db.GrantAgentPermission("steward-conv", agentd.PermDashboardGuide, "test"))

	// Before the dashboard reports anything the view is unknown.
	r := accountQuery(t, f, "steward-conv", "/v1/dashboard/view")
	require.Equal(t, http.StatusOK, r.Code)
	require.Contains(t, r.Body.String(), `"known":false`)

	// The dashboard reports the operator's view; the agent reads it back.
	require.Equal(t, http.StatusNoContent, postView(map[string]any{"tab": "processes", "path": "/processes/templates/deploy", "title": "tclaude"}))
	r = accountQuery(t, f, "steward-conv", "/v1/dashboard/view")
	var view struct {
		Known bool   `json:"known"`
		Tab   string `json:"tab"`
		Path  string `json:"path"`
	}
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &view))
	require.True(t, view.Known)
	require.Equal(t, "processes", view.Tab)
	require.Equal(t, "/processes/templates/deploy", view.Path)

	// A view path must stay on the dashboard.
	require.Equal(t, http.StatusBadRequest, postView(map[string]any{"tab": "x", "path": "https://evil.example/"}))
	require.Equal(t, http.StatusNoContent, postView(map[string]any{"tab": "messages", "path": "/messages?q=http://example.com"}), "a search query may hold a URL")

	// Cues are validated, then ride the snapshot with the agent's identity.
	require.Equal(t, http.StatusBadRequest, cue(map[string]any{}))
	require.Equal(t, http.StatusBadRequest, cue(map[string]any{"tab": "nope"}))
	require.Equal(t, http.StatusBadRequest, cue(map[string]any{"path": "//evil.example/x"}))
	require.Equal(t, http.StatusBadRequest, cue(map[string]any{"path": "/terminals/agt_0123"}), "a cue may not attach a terminal")
	require.Equal(t, http.StatusOK, cue(map[string]any{"path": "/terminals"}))
	agentd.ResetDashboardGuideForTest()
	require.Equal(t, http.StatusOK, cue(map[string]any{"tab": "Processes", "target": "Save draft", "note": "Save once\nthe diff\x07 looks right"}))

	snapReq := testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil)
	snapReq.Header.Set("Origin", "http://127.0.0.1:0")
	rec := testharness.Serve(dash, snapReq)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var snap struct {
		Cues []struct {
			ID      int64  `json:"id"`
			AgentID string `json:"agent_id"`
			Tab     string `json:"tab"`
			Target  string `json:"target"`
			Note    string `json:"note"`
		} `json:"companion_cues"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	require.Len(t, snap.Cues, 1)
	require.Equal(t, "processes", snap.Cues[0].Tab)
	require.Equal(t, "Save draft", snap.Cues[0].Target)
	require.Equal(t, "Save once the diff looks right", snap.Cues[0].Note, "control bytes are dropped and newlines flattened")
	require.NotEmpty(t, snap.Cues[0].AgentID)
}
