package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func (s *externalStewardFlow) dashboard(method, path string, body any) *httptest.ResponseRecorder {
	s.t.Helper()
	return testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(s.t, method, path, body))
}

func (s *externalStewardFlow) dashboardIssue(path string, body any) externalStewardIssued {
	s.t.Helper()
	rec := s.dashboard(http.MethodPost, path, body)
	require.Equal(s.t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(s.t, "no-store", rec.Header().Get("Cache-Control"))
	var issued externalStewardIssued
	require.NoError(s.t, json.Unmarshal(rec.Body.Bytes(), &issued))
	require.NotEmpty(s.t, issued.Credential)
	return issued
}

func TestExternalStewardDashboard_ManageFromConfig(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.dashboardIssue("/api/external-stewards", map[string]any{"name": "assistant", "expires_days": 7})
	code, body := s.call(issued.Credential, http.MethodGet, "/v1/whoami", nil)
	require.Equal(t, http.StatusOK, code, body)

	listed := s.dashboard(http.MethodGet, "/api/external-stewards", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.Body.String())
	assert.Contains(t, listed.Body.String(), `"assistant"`)
	assert.NotContains(t, listed.Body.String(), "tcls_")

	renewed := s.dashboardIssue("/api/external-stewards/assistant/renew", map[string]any{})
	assert.NotEqual(t, issued.Credential, renewed.Credential)

	rec := s.dashboard(http.MethodPost, "/api/external-stewards/assistant/revoke", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, credential := range []string{issued.Credential, renewed.Credential} {
		code, body = s.call(credential, http.MethodGet, "/v1/whoami", nil)
		assert.Equal(t, http.StatusUnauthorized, code, body)
	}

	rows, err := db.ListAuditLog(db.AuditLogFilter{})
	require.NoError(t, err)
	verbs := map[string]bool{}
	for _, row := range rows {
		assert.NotContains(t, fmt.Sprintf("%+v", row), "tcls_")
		verbs[row.Verb] = true
	}
	assert.True(t, verbs["external-steward.create"], "audited: %v", verbs)
	assert.True(t, verbs["external-steward.revoke"], "audited: %v", verbs)
	d, err := db.Open()
	require.NoError(t, err)
	var kept int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM agentd_idempotency WHERE response_body LIKE '%tcls_%'`).Scan(&kept))
	assert.Zero(t, kept)
}

func TestExternalStewardDashboard_OffAndCollabOff(t *testing.T) {
	s := newExternalStewardFlow(t, false)
	rec := s.dashboard(http.MethodPost, "/api/external-stewards", map[string]any{"name": "assistant"})
	assert.NotEqual(t, http.StatusOK, rec.Code, "the switch is off")
	assert.NotContains(t, rec.Body.String(), "tcls_")

	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features.Collab = false
	require.NoError(t, config.Save(cfg))
	assert.Equal(t, http.StatusNotFound, s.dashboard(http.MethodGet, "/api/external-stewards", nil).Code)
}

// No credential may leave for another instance or another site's page.
func TestExternalStewardDashboard_NeverReachesPeersOrOtherOrigins(t *testing.T) {
	fh := newFedHarness(t)
	enableCollab(t)
	setExternalStewards(t, true)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/external-stewards"},
		{http.MethodPost, "/api/external-stewards"},
		{http.MethodPost, "/api/external-stewards/assistant/renew"},
		{http.MethodPost, "/api/external-stewards/assistant/revoke"},
	}
	for _, route := range routes {
		rec := testharness.Serve(agentd.PeerViewHandler(fh.peer.id.ID()),
			testharness.JSONRequest(t, route.method, route.path, map[string]any{"name": "assistant"}))
		assert.Equal(t, http.StatusForbidden, rec.Code, route.path)
		assert.NotContains(t, rec.Body.String(), "tcls_")
	}
	req := testharness.JSONRequest(t, http.MethodPost, "/api/external-stewards", map[string]any{"name": "assistant"})
	req.Header.Set("Origin", "https://evil.invalid")
	assert.Equal(t, http.StatusForbidden, testharness.Serve(agentd.BuildDashboardHandlerForTest(), req).Code)
}

// A principal has no session or group, and the roster shows it even with every
// grant taken away.
func TestExternalStewardDashboard_OnTheRoster(t *testing.T) {
	s := newExternalStewardFlow(t, true)
	issued := s.create("assistant")
	steward, err := db.ListExternalStewards()
	require.NoError(t, err)
	require.Len(t, steward, 1)
	for _, slug := range issued.Grants {
		_, err := db.RevokeAgentPermission(steward[0].ConvID, slug)
		require.NoError(t, err)
	}
	rec := s.dashboard(http.MethodGet, "/api/snapshot", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var snapshot struct {
		Ungrouped []struct {
			AgentID         string `json:"agent_id"`
			Title           string `json:"title"`
			Online          bool   `json:"online"`
			ExternalSteward bool   `json:"external_steward"`
		} `json:"ungrouped"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	for _, agent := range snapshot.Ungrouped {
		if agent.AgentID == issued.AgentID {
			assert.Equal(t, "assistant", agent.Title)
			assert.True(t, agent.ExternalSteward)
			assert.False(t, agent.Online)
			return
		}
	}
	t.Fatalf("not on the roster: %s", rec.Body.String())
}
