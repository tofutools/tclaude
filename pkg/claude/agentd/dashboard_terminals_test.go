package agentd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDashboardTerminals_RequiresAuth: the standalone /terminals page is
// gated by the dashboard session cookie. An unauthenticated GET is bounced
// to / (where the human re-authenticates) rather than dead-ending — so the
// page is never served without a valid cookie.
func TestDashboardTerminals_RequiresAuth(t *testing.T) {
	withDashboardAuthForTest(t) // pins a session token, but we send no cookie

	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, httptest.NewRequest(http.MethodGet, "/terminals", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated /terminals GET: status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("redirect target = %q, want %q", loc, "/")
	}
	if strings.Contains(rec.Body.String(), "mux-tabs") {
		t.Error("unauthenticated /terminals must not leak the page body")
	}
}

// TestDashboardTerminals_ServesPage: with the session cookie the /terminals
// route serves the multiplexer page — no-store, and referencing the assets it
// needs (the page JS, its stylesheet, and the vendored xterm scripts) so the
// browser can actually render terminals.
func TestDashboardTerminals_ServesPage(t *testing.T) {
	cookie, _ := withDashboardAuthForTest(t)

	r := httptest.NewRequest(http.MethodGet, "/terminals", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /terminals GET: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	for _, needle := range []string{
		`id="mux-tabs"`,
		`/static/js/terminals.js`,
		`/static/terminals.css`,
		`/static/mux.css`,
		`/static/vendor/xterm/xterm.min.js`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("terminals page missing %q", needle)
		}
	}
}

// TestDashboardTerminals_GETOnly: the page route is read-only; a non-GET is
// refused before any auth/serving work.
func TestDashboardTerminals_GETOnly(t *testing.T) {
	withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, httptest.NewRequest(http.MethodPost, "/terminals", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /terminals: status %d, want 405", rec.Code)
	}
}
