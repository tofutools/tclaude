package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-317 makes the dashboard path-routed: deep dashboard paths (/jobs,
// /access/sudo, …) must serve the SPA index HTML so the client router can
// restore the view on reload / a bookmarked deep link, while genuinely unknown
// paths still 404. This exercises the real auth path (cookie required) so it
// also proves the SPA fallback did not open a hole around the init-token gate.
func TestDashboardAppPaths_SPAFallback(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	// Mint + exchange an init token to obtain the session cookie, mirroring
	// TestDashboardAuth_TokenExchangeFlow.
	rec := testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/dashboard/open", nil)))
	require.Equal(t, http.StatusOK, rec.Code, "open body=%s", rec.Body.String())
	var resp struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	u, err := url.Parse(resp.URL)
	require.NoError(t, err)
	tok := u.Query().Get("init_token")
	require.NotEmpty(t, tok, "minted URL must carry init_token")

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/?init_token="+tok, nil))
	require.Equal(t, http.StatusSeeOther, rec.Code, "exchange body=%s", rec.Body.String())
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashboardCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "exchange must set the dashboard session cookie")

	authedGet := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		return testharness.Serve(dash, req)
	}

	// Deep dashboard paths — top-level and nested — serve the SPA HTML.
	for _, path := range []string{
		"/", "/dashboard", "/jobs", "/processes", "/access", "/access/sudo",
		"/processes/runs/run-42", "/costs", "/audit", "/logs", "/plugins", "/messages",
	} {
		rec := authedGet(path)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s should serve the SPA; body=%s", path, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "<!DOCTYPE html", "GET %s must serve the dashboard page", path)
	}

	// Unknown / junk paths still 404 — the SPA fallback did not become a
	// catch-all that renders HTML for anything. /vegas and /terminals-typo
	// confirm the deliberately non-routed tabs are not app paths.
	for _, path := range []string{
		"/favicon.ico", "/nope", "/robots.txt", "/vegas", "/terminals-typo", "/access-extra",
	} {
		rec := authedGet(path)
		assert.Equal(t, http.StatusNotFound, rec.Code, "GET %s must 404 (not an app path); body=%s", path, rec.Body.String())
	}

	// A deep app path with NO cookie is still refused — auth is enforced on the
	// fallback exactly as on the root.
	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/access/sudo", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code, "unauthenticated deep path must be refused; body=%s", rec.Body.String())
}
