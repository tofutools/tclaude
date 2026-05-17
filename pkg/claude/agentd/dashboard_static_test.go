package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withDashboardAuthForTest pins the dashboard session token and popup
// base URL to fixed test values (restored on cleanup) and returns a
// cookie + Origin that satisfy checkDashboardAuth — so the tests below
// can drive the real, auth-gated handleDashboardStatic handler.
func withDashboardAuthForTest(t *testing.T) (cookie *http.Cookie, origin string) {
	t.Helper()
	prevToken, prevURL := dashboardSessionToken, popupBaseURL
	t.Cleanup(func() { dashboardSessionToken, popupBaseURL = prevToken, prevURL })
	dashboardSessionToken = "static-test-session-token"
	popupBaseURL = "http://127.0.0.1:0"
	return &http.Cookie{Name: dashboardCookieName, Value: dashboardSessionToken}, popupBaseURL
}

// authedStaticRequest builds a GET for the /static/ route carrying the
// session cookie and a matching Origin.
func authedStaticRequest(path string, cookie *http.Cookie, origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.AddCookie(cookie)
	r.Header.Set("Origin", origin)
	return r
}

// TestDashboardStatic_RequiresAuth: the /static/ asset route is gated
// by the dashboard session cookie — an unauthenticated GET is refused,
// the same gate /api/* sits behind.
func TestDashboardStatic_RequiresAuth(t *testing.T) {
	withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/js/dashboard.js", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated /static/ GET: status %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardStatic_ServesAssets: with the session cookie the
// /static/ route serves the asset with Cache-Control: no-store and a
// type the browser will honour — a JavaScript type for .js (a module
// served as text/plain is refused) and text/css for .css.
func TestDashboardStatic_ServesAssets(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)

	cases := []struct{ path, wantType string }{
		{"/static/js/dashboard.js", "javascript"},
		{"/static/dashboard.css", "text/css"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		handleDashboardStatic().ServeHTTP(rec, authedStaticRequest(tc.path, cookie, origin))

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200; body=%s", tc.path, rec.Code, rec.Body.String())
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantType) {
			t.Errorf("GET %s: Content-Type %q, want it to contain %q", tc.path, ct, tc.wantType)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control %q, want %q", tc.path, cc, "no-store")
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s: empty body", tc.path)
		}
	}
}

// TestDashboardStatic_NoDirectoryListing: a directory path under
// /static/ is 404'd — the route serves named asset files only and never
// emits a directory listing.
func TestDashboardStatic_NoDirectoryListing(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, authedStaticRequest("/static/js/", cookie, origin))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /static/js/ (directory): status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestDashboardStatic_ContainsTraversal: a ../-style path cannot escape
// the embedded dashboard/ tree — handleDashboardStatic serves a rooted
// sub-FS, so /static/../dashboard.go resolves to nothing and 404s
// rather than leaking a sibling source file.
func TestDashboardStatic_ContainsTraversal(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, authedStaticRequest("/static/../dashboard.go", cookie, origin))

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /static/../dashboard.go: status %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "package agentd") {
		t.Error("traversal path leaked Go source from outside the embedded dashboard/ tree")
	}
}
