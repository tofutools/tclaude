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

// dashboardCookieName is the wire-contract cookie the dashboard
// session rides on. Duplicated here (the const is unexported) so the
// test asserts the real header an agent would have to forge.
const dashboardCookieName = "tclaude_dashboard_session"

// Scenario: an agent (a caller with a Claude Code ancestor) is refused
// the dashboard init token. This is the load-bearing gate — the
// dashboard's /api/* surface bypasses the per-agent permission system,
// so an agent must never be able to mint a token and exchange it for
// the session cookie.
func TestDashboardOpen_RefusesAgents(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodGet, "/v1/dashboard/open", nil),
			"dboa-aaaa-bbbb-cccc-1111"))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent must be refused the dashboard init token; body=%s", rec.Body.String())
}

// Scenario: the human (no Claude Code ancestor) gets a one-shot URL
// with an init token embedded.
func TestDashboardOpen_HumanGetsTokenURL(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	rec := testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/dashboard/open", nil)))
	require.Equal(t, http.StatusOK, rec.Code, "human body=%s", rec.Body.String())

	var resp struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	u, err := url.Parse(resp.URL)
	require.NoError(t, err, "returned URL must parse")
	assert.NotEmpty(t, u.Query().Get("init_token"), "URL must embed an init token")
}

// Scenario: the full authorization-code-style exchange. A bare GET /
// is refused (the cookie is never handed out for free); a valid init
// token is swapped for the session cookie via a 303 redirect; the
// token is single-use; and a request carrying the cookie serves the
// page.
func TestDashboardAuth_TokenExchangeFlow(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	// Human mints an init token through the gated /v1 endpoint.
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

	// Dashboard mux without the test cookie-injection wrapper, so the
	// real auth path is exercised.
	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	// Bare GET / — no token, no cookie — refused.
	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"bare GET / must be refused; body=%s", rec.Body.String())

	// Bogus token — refused.
	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/?init_token=deadbeefdeadbeef", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"bogus init token must be refused; body=%s", rec.Body.String())

	// Exchange: GET /?init_token=<tok> → 303 redirect that sets the cookie.
	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/?init_token="+tok, nil))
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"exchange must 303-redirect; body=%s", rec.Body.String())
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashboardCookieName {
			cookie = c
		}
	}
	require.NotNil(t, cookie, "exchange must set the dashboard session cookie")

	// The same token a second time — refused (single-use).
	rec = testharness.Serve(dash, httptest.NewRequest(http.MethodGet, "/?init_token="+tok, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"init token must be single-use; body=%s", rec.Body.String())

	// Carrying the cookie, GET / serves the dashboard HTML.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = testharness.Serve(dash, req)
	require.Equal(t, http.StatusOK, rec.Code, "cookie GET / body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<!DOCTYPE html",
		"a cookie-authenticated GET / must serve the dashboard page")
}
