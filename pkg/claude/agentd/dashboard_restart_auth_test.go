package agentd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func withDashboardRestartAuthTest(t *testing.T, now time.Time) {
	t.Helper()
	prevToken := dashboardSessionToken
	prevGrace := dashboardGraceSessionHashes
	prevNow := dashboardSessionNow
	prevURL := popupBaseURL
	t.Cleanup(func() {
		dashboardSessionToken = prevToken
		dashboardGraceSessionHashes = prevGrace
		dashboardSessionNow = prevNow
		popupBaseURL = prevURL
	})
	dashboardSessionNow = func() time.Time { return now }
	dashboardGraceSessionHashes = map[string]time.Time{}
	popupBaseURL = "http://127.0.0.1:4567"
}

func TestDashboardSessionSurvivesCleanRestartAndRotatesCookie(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	withDashboardRestartAuthTest(t, now)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	// Model daemon A's clean shutdown, then daemon B's startup with a fresh
	// in-memory session token.
	dashboardSessionToken = "daemon-a-cookie"
	require.NoError(t, preserveDashboardSessionForRestart())
	dashboardSessionToken = "daemon-b-cookie"
	require.NoError(t, restoreDashboardGraceSessions())

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Origin", popupBaseURL)
	req.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: "daemon-a-cookie"})
	rec := httptest.NewRecorder()
	require.True(t, checkDashboardAuth(rec, req), "unexpired pre-restart cookie should authenticate")
	rotated := rec.Result().Cookies()
	require.Len(t, rotated, 1, "grace authentication must rotate the browser cookie")
	assert.Equal(t, dashboardCookieName, rotated[0].Name)
	assert.Equal(t, "daemon-b-cookie", rotated[0].Value)
	assert.True(t, rotated[0].HttpOnly)

	// A top-level reload takes the same handoff path and serves the SPA rather
	// than the login screen, while also issuing daemon B's cookie.
	rootReq := httptest.NewRequest(http.MethodGet, "/", nil)
	rootReq.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: "daemon-a-cookie"})
	rootRec := httptest.NewRecorder()
	handleDashboardRoot(rootRec, rootReq)
	assert.Equal(t, http.StatusOK, rootRec.Code)
	assert.Contains(t, rootRec.Body.String(), "<!DOCTYPE html")
	require.Len(t, rootRec.Result().Cookies(), 1)
	assert.Equal(t, "daemon-b-cookie", rootRec.Result().Cookies()[0].Value)
}

func TestDashboardExpiredCookieSignalsBrowserLogin(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	withDashboardRestartAuthTest(t, now)
	dashboardSessionToken = "current-cookie"
	dashboardGraceSessionHashes[dashboardTokenHash("expired-cookie")] = now.Add(-time.Second)

	req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
	req.Header.Set("Origin", popupBaseURL)
	req.AddCookie(&http.Cookie{Name: dashboardCookieName, Value: "expired-cookie"})
	rec := httptest.NewRecorder()
	assert.False(t, checkDashboardAuth(rec, req))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("X-Tclaude-Login-Required"),
		"the fetch wrapper needs an unambiguous re-auth signal")
	assert.Empty(t, rec.Result().Cookies(), "expired credentials must never be rotated")
}
