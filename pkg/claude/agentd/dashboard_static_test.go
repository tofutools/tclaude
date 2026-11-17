package agentd

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
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

// TestDashboardStatic_VersionedIsImmutable: under the current build's
// versioned prefix an asset is cached for good (an upgrade changes the URL);
// a prefix this binary does not serve — a page from the previous agentd — gets
// the current file, uncached.
func TestDashboardStatic_VersionedIsImmutable(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)
	for _, tc := range []struct{ path, cache string }{
		{dashboardStaticBase + "js/dashboard.js", dashboardStaticImmutable},
		{dashboardStaticBase + "tclaude-icon.svg", dashboardStaticImmutable},
		{"/static/v-0000000000000000/js/dashboard.js", "no-store"},
	} {
		rec := httptest.NewRecorder()
		handleDashboardStatic().ServeHTTP(rec, authedStaticRequest(tc.path, cookie, origin))
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Errorf("GET %s: status %d, %d bytes", tc.path, rec.Code, rec.Body.Len())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != tc.cache {
			t.Errorf("GET %s: Cache-Control %q, want %q", tc.path, cc, tc.cache)
		}
	}
	for _, p := range []string{dashboardStaticBase, dashboardStaticBase + "js/", "/static/v-" + dashboardAssetsVersion, dashboardStaticBase + "../dashboard.go"} {
		rec := httptest.NewRecorder()
		handleDashboardStatic().ServeHTTP(rec, authedStaticRequest(p, cookie, origin))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, rec.Code)
		}
	}
}

// TestDashboardStatic_Gzip: a client that accepts gzip gets text assets
// gzipped, with the right type and a body that inflates to the file; one that
// does not gets the file as is. Both are told the response varies by encoding.
func TestDashboardStatic_Gzip(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)
	want := mustReadFS(dashboardAssetsFS, "js/dashboard.js")

	req := authedStaticRequest(dashboardStaticBase+"js/dashboard.js", cookie, origin)
	req.Header.Set("Accept-Encoding", "br, gzip, deflate")
	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, req)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding %q, want gzip", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type %q, want javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != dashboardStaticImmutable {
		t.Errorf("Cache-Control %q on the gzip path, want %q", cc, dashboardStaticImmutable)
	}
	if rec.Body.Len() >= len(want) {
		t.Errorf("gzip body %d bytes, not smaller than the %d-byte file", rec.Body.Len(), len(want))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("gzip body does not inflate to the file (err %v)", err)
	}

	for _, enc := range []string{"", "gzip;q=0"} {
		req := authedStaticRequest(dashboardStaticBase+"js/dashboard.js", cookie, origin)
		if enc != "" {
			req.Header.Set("Accept-Encoding", enc)
		}
		rec := httptest.NewRecorder()
		handleDashboardStatic().ServeHTTP(rec, req)
		if rec.Header().Get("Content-Encoding") != "" || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("Accept-Encoding %q: got an encoded body", enc)
		}
		if !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("Accept-Encoding %q: no Vary: Accept-Encoding", enc)
		}
	}
}

// TestDashboardPages_NameTheVersionedPrefix: the served pages reference their
// assets (links, scripts, the import map, the boot preloads) under the
// versioned prefix only.
func TestDashboardPages_NameTheVersionedPrefix(t *testing.T) {
	for name, page := range map[string][]byte{"dashboard.html": dashboardIndexHTML(), "terminals.html": terminalsPageHTML} {
		html := string(page)
		if strings.Contains(strings.ReplaceAll(html, `"`+dashboardStaticBase, ""), `"/static/`) {
			t.Errorf("%s still names a bare \"/static/ asset", name)
		}
		if !strings.Contains(html, `"`+dashboardStaticBase+`js/`) {
			t.Errorf("%s names no versioned module", name)
		}
	}
}

// TestDashboardJS_NoAbsoluteStaticPaths: a module names another asset
// relative to itself (import.meta.url), never as an absolute /static/ path. An
// absolute one bypasses the versioned prefix: it is refetched on every load,
// and a module imported under both URLs would run twice.
func TestDashboardJS_NoAbsoluteStaticPaths(t *testing.T) {
	literal := regexp.MustCompile("['\"`]/static/")
	err := fs.WalkDir(dashboardAssetsFS, "js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		for i, line := range strings.Split(string(mustReadFS(dashboardAssetsFS, p)), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if literal.MatchString(line) && !strings.Contains(line, "/static/${name}") {
				t.Errorf("%s:%d names an absolute /static/ path: %s", p, i+1, trimmed)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// unversionedStatic maps a served page's versioned asset prefix back to the
// bare /static/ its source names, for tests that assert on the source paths.
func unversionedStatic(page string) string {
	return strings.ReplaceAll(page, dashboardStaticBase, "/static/")
}
