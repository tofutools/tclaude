package agentd

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Workgraphs tab is a front-end-only feature (Step 5): a new tab in
// the dashboard SPA that consumes the workgraphs snapshot + /api/workgraphs
// detail endpoints (owned by the backend, Step 3/4) and renders the
// instance graph with mermaid. These tests guard the bits that fail only
// in the browser otherwise: the vendored mermaid asset is embedded +
// served, it is the self-contained build (no dynamic chunk imports that
// would 404 offline), and the tab is wired into the page shell + JS.

// TestDashboardEmbed_VendoredMermaid checks the offline-mermaid contract:
// the UMD bundle is embedded, large enough to be the real self-contained
// build (not a stub), and carries NO dynamic import() — the v11/v10
// builds lazy-load diagram chunks via import(), which 404 when the file
// is self-hosted as one asset. v9.4.3 has none; this pins that choice.
func TestDashboardEmbed_VendoredMermaid(t *testing.T) {
	data, err := fs.ReadFile(dashboardAssetsFS, "vendor/mermaid.min.js")
	if err != nil {
		t.Fatalf("embedded vendor/mermaid.min.js not found: %v", err)
	}
	if len(data) < 1_000_000 {
		t.Errorf("vendor/mermaid.min.js is %d bytes — too small to be the self-contained UMD bundle", len(data))
	}
	s := string(data)
	if !strings.Contains(s, "typeof exports") {
		t.Error("vendor/mermaid.min.js is not the expected UMD bundle (no UMD wrapper found)")
	}
	if strings.Contains(s, "import(") {
		t.Error("vendor/mermaid.min.js contains a dynamic import() — that build lazy-loads chunks and breaks offline; vendor the self-contained v9.4.3 UMD build")
	}
}

// TestDashboardStatic_ServesVendoredMermaid: the vendored mermaid asset
// is served from /static/vendor/ behind the dashboard auth cookie, with
// a JavaScript content-type the browser will execute and the same
// no-store cache policy as the rest of the assets.
func TestDashboardStatic_ServesVendoredMermaid(t *testing.T) {
	cookie, origin := withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, authedStaticRequest("/static/vendor/mermaid.min.js", cookie, origin))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/vendor/mermaid.min.js: status %d, want 200; body=%.120s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type %q, want it to contain %q", ct, "javascript")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q, want %q", cc, "no-store")
	}
	if rec.Body.Len() < 1_000_000 {
		t.Errorf("served mermaid body is %d bytes — truncated?", rec.Body.Len())
	}
}

// TestDashboardStatic_VendoredMermaidRequiresAuth: the vendored asset is
// behind the same cookie gate as every other /static/ file.
func TestDashboardStatic_VendoredMermaidRequiresAuth(t *testing.T) {
	withDashboardAuthForTest(t)

	rec := httptest.NewRecorder()
	handleDashboardStatic().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/vendor/mermaid.min.js", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated GET of vendored mermaid: status %d, want 403", rec.Code)
	}
}

// TestDashboardHTML_WorkgraphsTabWired pins the page-shell wiring: the nav
// button, the tab section with both panes + the detail host, the
// instantiate modal, and the classic <script> that loads vendored
// mermaid before the deferred module that reads window.mermaid.
func TestDashboardHTML_WorkgraphsTabWired(t *testing.T) {
	html := string(dashboardIndexHTML)
	for _, needle := range []string{
		`data-tab="workgraphs"`,
		`<section id="tab-workgraphs">`,
		`id="wf-templates-list"`,
		`id="wf-instances-list"`,
		`id="wf-detail"`,
		`id="wf-instantiate-modal"`,
		`<script src="/static/vendor/mermaid.min.js"></script>`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("dashboard.html missing %q — Workgraphs tab not wired into the shell", needle)
		}
	}
	// The mermaid classic script must come before the module entrypoint,
	// or window.mermaid is undefined when the module first renders.
	mermaidAt := strings.Index(html, `/static/vendor/mermaid.min.js`)
	moduleAt := strings.Index(html, `/static/js/dashboard.js`)
	if mermaidAt < 0 || moduleAt < 0 || mermaidAt > moduleAt {
		t.Errorf("mermaid <script> (at %d) must precede the dashboard.js module (at %d)", mermaidAt, moduleAt)
	}
}

// TestDashboardAssets_WorkgraphsJSWired guards the JS wiring across the
// module split: the workgraphs module exports its render + bind hooks,
// the refresh loop and bootstrap call them, and the module talks to the
// real /api/workgraphs surface + window.mermaid. Asserting on the
// embedded concatenation catches a rename in any one file at `go test`.
func TestDashboardAssets_WorkgraphsJSWired(t *testing.T) {
	for _, needle := range []string{
		"function renderWorkgraphsTab(",       // defined in workgraphs.js
		"function bindWorkgraphsUI(",          // defined in workgraphs.js
		"renderWorkgraphsTab, bindWorkgraphsUI", // exported
		"renderWorkgraphsTab();",              // called by refresh.js poll
		"bindWorkgraphsUI();",                 // called by dashboard.js bootstrap
		"window.mermaid",                     // consumes the vendored global
		"/api/workgraphs",                     // hits the backend surface
		"resolveBoundMember(",                // vitals overlay match logic
	} {
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard JS missing %q — Workgraphs tab wiring broken", needle)
		}
	}
}
