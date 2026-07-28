package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func withDashboardSessionForTUITest(t *testing.T, token string) {
	t.Helper()
	oldToken := dashboardSessionToken
	oldCookie := dashboardCookieName
	dashboardSessionToken = token
	dashboardCookieName = "tclaude_dashboard_session_tui_test"
	t.Cleanup(func() {
		dashboardSessionToken = oldToken
		dashboardCookieName = oldCookie
	})
}

func TestTUIHTTPAuthBootstrapsTheRestartSurvivingDashboardSession(t *testing.T) {
	withOperatorToken(t, "tclo_http-test")
	withDashboardSessionForTUITest(t, "dashboard-session")

	var class callerClass
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class = classify(peerFromContext(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticateTUIHTTPRequest(w, r) {
			next.ServeHTTP(w, asDashboardHumanPeer(r))
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	req.Header.Set(agent.HumanTokenHeader, "tclo_http-test")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, classHuman, class)
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	require.Len(t, resp.Cookies(), 1)
	assert.Equal(t, dashboardCookieName, resp.Cookies()[0].Name)
	assert.Equal(t, "dashboard-session", resp.Cookies()[0].Value)
}

func TestTUIHTTPAuthRejectsAnInvalidOperatorToken(t *testing.T) {
	withOperatorToken(t, "tclo_http-test")
	withDashboardSessionForTUITest(t, "dashboard-session")

	req := httptest.NewRequest(http.MethodGet, "/v1/peers", nil)
	req.Header.Set(agent.HumanTokenHeader, "wrong")
	rec := httptest.NewRecorder()
	assert.False(t, authenticateTUIHTTPRequest(rec, req))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestTUIHTTPHandlerExposesOnlyTheTerminalDashboardSurface(t *testing.T) {
	withOperatorToken(t, "tclo_http-test")
	withDashboardSessionForTUITest(t, "dashboard-session")

	req := httptest.NewRequest(http.MethodGet, "/v1/permissions", nil)
	req.Header.Set(agent.HumanTokenHeader, "tclo_http-test")
	rec := httptest.NewRecorder()
	buildTUIHTTPHandler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
