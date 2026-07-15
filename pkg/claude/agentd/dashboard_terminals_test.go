package agentd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardXtermVendorAssets(t *testing.T) {
	wantHashes := map[string]string{
		"vendor/xterm/xterm.min.js":           "e4d246be46c786a973e6d6eea46aef1eed56660b2a7f469a3b48b3738321646e",
		"vendor/xterm/xterm.min.css":          "99ae5d3f0651a557ba34946aeaa384c4ddd0e697ff205c7c1f5f955867063907",
		"vendor/xterm/addon-fit.min.js":       "696bd2890cb91f96b6db0a83103d49088892ff440bf01d2da654c905cff7696c",
		"vendor/xterm/addon-web-links.min.js": "7b5d634522f0e93ef567b4f6d72d4b71f0a5e95070f079b004f7b945b7c4c9ab",
		"vendor/xterm/LICENSE.xterm":          "b569f629d00f2626a8100df2a1798210535621e42164dfd426a6fe5aac7b0ccd",
	}
	for name, want := range wantHashes {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("reading embedded xterm asset %q: %v", name, err)
			continue
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("embedded xterm asset %q hash changed; update the vendored manifest intentionally", name)
		}
	}
	manifest, err := fs.ReadFile(dashboardAssetsFS, "vendor/xterm/README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"@xterm/xterm", "6.0.0", "@xterm/addon-fit", "0.11.0", "@xterm/addon-web-links", "0.12.0"} {
		if !strings.Contains(string(manifest), needle) {
			t.Errorf("xterm vendor manifest missing %q", needle)
		}
	}
}

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
// /terminals?solo=1 serves the standalone Preact terminal popout page — no-store,
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
		`id="terminals-root"`,
		`type="importmap"`,
		`"preact": "/static/vendor/preact/preact.module.js"`,
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
	for _, forbidden := range []string{`id="mux"`, `id="mux-tabs"`, `id="mux-panes"`, `id="mux-empty"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("terminals popout page retains static shell writer %q", forbidden)
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
	if !strings.Contains(body, `/static/js/dashboard.js`) || !strings.Contains(body, `id="tab-terminals"`) {
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
