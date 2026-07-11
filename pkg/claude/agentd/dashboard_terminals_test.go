package agentd

import (
	"context"
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

// TestDashboardTerminals_SoloServesPopout: with the session cookie,
// /terminals?solo=1 serves the standalone multiplexer popout page — no-store,
// and referencing the assets it needs (the page JS, its stylesheet, and the
// vendored xterm scripts) so the browser can render a popped-out terminal. The
// ?solo=1 query is what the "⧉ tab" pop-out opens (js/terminals.js).
func TestDashboardTerminals_SoloServesPopout(t *testing.T) {
	cookie, _ := withDashboardAuthForTest(t)

	r := httptest.NewRequest(http.MethodGet, "/terminals?solo=1", nil)
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated /terminals?solo=1 GET: status %d, want 200; body=%s", rec.Code, rec.Body.String())
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
		`/static/vendor/xterm/addon-web-links.min.js`,
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("terminals popout page missing %q", needle)
		}
	}
	// It's the popout, NOT the dashboard SPA.
	if strings.Contains(body, `/static/js/dashboard.js`) {
		t.Error("solo popout must not serve the dashboard SPA entry")
	}
}

// TestDashboardTerminals_PlainServesSPA: a plain /terminals (no ?solo) is the
// dashboard's own Terminals TAB under path routing (TCL-317) — it serves the
// SPA index so the URL and the visible tab agree, NOT the standalone popout.
func TestDashboardTerminals_PlainServesSPA(t *testing.T) {
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
	if !strings.Contains(body, `/static/js/dashboard.js`) || !strings.Contains(body, `id="nav-back"`) {
		t.Error("plain /terminals must serve the dashboard SPA (so the Terminals tab restores)")
	}
	if strings.Contains(body, `/static/js/terminals.js`) {
		t.Error("plain /terminals must not serve the standalone popout page")
	}
}

// TestDashboardTerminals_RemotePreAuthed: a request already authenticated at
// the remote (mTLS + passphrase) listener boundary — tagged via
// remoteAuthedCtxKey by remoteAuthMiddleware — is served the page directly,
// with NO dashboard cookie. Guards the separate pre-auth branch so the remote
// path doesn't regress silently.
func TestDashboardTerminals_RemotePreAuthed(t *testing.T) {
	withDashboardAuthForTest(t) // pins a session token we deliberately don't send

	r := httptest.NewRequest(http.MethodGet, "/terminals?solo=1", nil)
	r = r.WithContext(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("remote pre-authed /terminals?solo=1 GET: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	if !strings.Contains(rec.Body.String(), `/static/js/terminals.js`) {
		t.Error("remote pre-authed /terminals?solo=1 must serve the popout page")
	}
}

// TestDashboardTerminals_SoloRequiresAuth: the ?solo popout is behind the same
// gate as the plain route — an unauthenticated GET is bounced to /, so ?solo
// can't bypass auth to reach the popout body.
func TestDashboardTerminals_SoloRequiresAuth(t *testing.T) {
	withDashboardAuthForTest(t) // pins a session token, but we send no cookie

	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, httptest.NewRequest(http.MethodGet, "/terminals?solo=1", nil))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated /terminals?solo=1 GET: status %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "mux-tabs") {
		t.Error("unauthenticated /terminals?solo=1 must not leak the popout body")
	}
}

// TestDashboardTerminals_RemotePreAuthedPlainServesSPA: a remote-preauthed
// client hitting the PLAIN /terminals gets the dashboard SPA (the new default
// branch for remote clients), not the popout.
func TestDashboardTerminals_RemotePreAuthedPlainServesSPA(t *testing.T) {
	withDashboardAuthForTest(t) // pins a token we deliberately don't send

	r := httptest.NewRequest(http.MethodGet, "/terminals", nil)
	r = r.WithContext(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	rec := httptest.NewRecorder()
	handleDashboardTerminals(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("remote pre-authed plain /terminals GET: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `/static/js/dashboard.js`) {
		t.Error("remote pre-authed plain /terminals must serve the dashboard SPA")
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
