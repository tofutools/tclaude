package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// operatorLoginCookie hunts the dashboard session cookie out of a
// response, or returns nil. Same wire-contract cookie the rest of the
// dashboard rides on (see dashboard_auth_flow_test.go).
func operatorLoginCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == dashboardCookieName {
			return c
		}
	}
	return nil
}

// loginPOST builds a same-origin POST to /dashboard/login carrying the
// given token in the form body, with the Origin header a real browser
// form submit would send. baseURL must match the test popupBaseURL.
func loginPOST(baseURL, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/dashboard/login",
		strings.NewReader("token="+token))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", baseURL)
	return req
}

// Scenario: an unauthenticated GET / no longer dead-ends on a plain
// 403 — it serves the sign-in page (status stays 403 so the existing
// auth contract holds) with both the `tclaude agent dashboard` hint and
// the operator-token field.
func TestDashboardLogin_UnauthedServesSignInPage(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec := serveLogin(dash, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusForbidden, rec.Code,
		"unauthed GET / keeps the 403 contract; body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "tclaude agent dashboard", "page must show the CLI re-auth path")
	assert.Contains(t, body, `name="token"`, "page must offer the operator-token field")
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/html",
		"sign-in page must be served as HTML, not plain text")
	assert.Contains(t, body, `action="/dashboard/login"`,
		"plain sign-in page posts to the bare login path")

	// The 🎰 slop theme survives the re-auth round-trip: the form action
	// carries ?slop=1 so the post-login redirect lands back on it.
	rec = serveLogin(dash, httptest.NewRequest(http.MethodGet, "/?slop=1", nil))
	assert.Contains(t, rec.Body.String(), `action="/dashboard/login?slop=1"`,
		"slop theme must thread into the login form action")

	// The 🧙 wizard theme threads through the same way (?wizard=1).
	rec = serveLogin(dash, httptest.NewRequest(http.MethodGet, "/?wizard=1", nil))
	assert.Contains(t, rec.Body.String(), `action="/dashboard/login?wizard=1"`,
		"wizard theme must thread into the login form action")
}

// Scenario: the operator pastes a valid operator token. The login
// endpoint verifies it, 303-redirects, and sets the session cookie;
// that cookie then serves the dashboard page — same surface a fresh
// init-token exchange reaches.
func TestDashboardLogin_ValidOperatorTokenSignsIn(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_test_valid_token"))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec := serveLogin(dash, loginPOST(base, "tclo_test_valid_token"))
	require.Equal(t, http.StatusSeeOther, rec.Code,
		"valid operator token must 303-redirect; body=%s", rec.Body.String())
	cookie := operatorLoginCookie(rec)
	require.NotNil(t, cookie, "valid login must set the dashboard session cookie")

	// The minted cookie serves the real dashboard page.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(cookie)
	rec = serveLogin(dash, req)
	require.Equal(t, http.StatusOK, rec.Code, "cookie GET / body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<!DOCTYPE html",
		"the login-minted cookie must serve the dashboard HTML")
}

// Scenario: a wrong operator token is refused — no cookie, sign-in page
// re-rendered with an error. This is the load-bearing gate: the token
// is the only thing a same-machine agent lacks, so a bad one must never
// mint the cookie.
func TestDashboardLogin_WrongOperatorTokenRefused(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_the_real_one"))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec := serveLogin(dash, loginPOST(base, "tclo_wrong_guess"))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"wrong operator token must be refused; body=%s", rec.Body.String())
	assert.Nil(t, operatorLoginCookie(rec), "a refused login must not set the cookie")
}

// Scenario: fail closed when no operator token was ever minted (e.g.
// agentd started backgrounded). An empty stored token must never match
// — not even an empty submission.
func TestDashboardLogin_NoOperatorTokenFailsClosed(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))
	t.Cleanup(agentd.SetOperatorTokenForTest("")) // none minted

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	for _, submitted := range []string{"", "tclo_anything"} {
		rec := serveLogin(dash, loginPOST(base, submitted))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"no minted token must fail closed for %q; body=%s", submitted, rec.Body.String())
		assert.Nil(t, operatorLoginCookie(rec), "fail-closed login must not set the cookie")
	}
}

// Scenario: a cross-origin POST (CSRF probe) carrying a token is
// rejected on the Origin pin before the token is even compared.
func TestDashboardLogin_ForeignOriginRejected(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_the_real_one"))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	req := loginPOST(base, "tclo_the_real_one")
	req.Header.Set("Origin", "http://evil.example") // not our loopback origin
	rec := serveLogin(dash, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"foreign-origin login must be refused; body=%s", rec.Body.String())
	assert.Nil(t, operatorLoginCookie(rec), "foreign-origin login must not set the cookie")
}

// Scenario: a hostile same-user process binds a loopback port whose
// number is a string-superstring of the daemon's port (e.g. 6553 vs
// 655) and serves a page that POSTs a guessed token. The origin pin
// must reject it — a bare strings.HasPrefix would not. Even with the
// *correct* token in the body, the wrong origin is refused before the
// cookie is minted.
func TestDashboardLogin_PortSuperstringOriginRejected(t *testing.T) {
	const base = "http://127.0.0.1:655"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_the_real_one"))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	req := loginPOST(base, "tclo_the_real_one")
	req.Header.Set("Origin", "http://127.0.0.1:6553") // superstring port, different origin
	rec := serveLogin(dash, req)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"port-superstring origin must be refused; body=%s", rec.Body.String())
	assert.Nil(t, operatorLoginCookie(rec), "port-superstring origin must not set the cookie")
}

// Scenario: GET /dashboard/login is method-not-allowed (it is a POST
// target only).
func TestDashboardLogin_GetNotAllowed(t *testing.T) {
	const base = "http://127.0.0.1:0"
	t.Cleanup(agentd.SetPopupBaseURLForTest(base))

	dash := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(dash)

	rec := serveLogin(dash, httptest.NewRequest(http.MethodGet, "/dashboard/login", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
		"GET /dashboard/login must be 405; body=%s", rec.Body.String())
}

// serveLogin runs one request through the dashboard mux and returns the
// recorder. Thin wrapper so the cases above read declaratively.
func serveLogin(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}
