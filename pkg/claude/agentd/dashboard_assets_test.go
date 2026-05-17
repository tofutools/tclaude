package agentd

import (
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// dashboardJSModules lists every embedded ES-module file under js/,
// sorted — dashboard.js (the entrypoint) plus whatever feature modules
// the Stage 2 split has extracted so far.
func dashboardJSModules() []string {
	mods, err := fs.Glob(dashboardAssetsFS, "js/*.js")
	if err != nil {
		panic("agentd: globbing embedded dashboard js/: " + err.Error())
	}
	sort.Strings(mods)
	return mods
}

// dashboardAssets is the embedded dashboard source — dashboard.html,
// dashboard.css and every js/ ES module — concatenated into one string.
//
// Before the ES-module cutover the dashboard was a single assembled
// `dashboardHTML` blob and content tests searched it directly. The
// files are now embedded and served separately, so tests that assert
// "X is present in the dashboard source" search this concatenation
// instead. Globbing js/*.js keeps it correct as the Stage 2 split
// extracts more modules. A genuinely missing file surfaces through
// TestDashboardEmbed_HasExpectedFiles, not a panic here.
var dashboardAssets = func() string {
	var b strings.Builder
	names := append([]string{"dashboard.html", "dashboard.css"}, dashboardJSModules()...)
	for _, name := range names {
		data, _ := fs.ReadFile(dashboardAssetsFS, name)
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}()

// TestDashboardEmbed_HasExpectedFiles guards that `//go:embed dashboard`
// captured the page shell, its stylesheet, and the ES-module entrypoint
// — a renamed or misplaced file would otherwise fail only at runtime,
// when the daemon serves an empty page or 404s a module.
func TestDashboardEmbed_HasExpectedFiles(t *testing.T) {
	for _, name := range []string{"dashboard.html", "dashboard.css", "js/dashboard.js"} {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("embedded dashboard asset %q not found: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("embedded dashboard asset %q is empty", name)
		}
	}
	if mods := dashboardJSModules(); len(mods) == 0 {
		t.Error("no js/*.js modules embedded")
	}
}

// TestDashboardHTML_ReferencesStaticAssets pins that the served
// dashboard.html loads the stylesheet and the ES-module entrypoint from
// the /static/ route by absolute path (so it resolves the same whatever
// path the document was served from), and that the retired Stage-1
// inline splice points (<style></style> / <script></script>) are gone.
func TestDashboardHTML_ReferencesStaticAssets(t *testing.T) {
	html := string(dashboardIndexHTML)
	for _, needle := range []string{
		`<link rel="stylesheet" href="/static/dashboard.css">`,
		`<script type="module" src="/static/js/dashboard.js"></script>`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("dashboard.html missing %q", needle)
		}
	}
	for _, stale := range []string{"<style></style>", "<script></script>"} {
		if strings.Contains(html, stale) {
			t.Errorf("dashboard.html still carries the retired splice point %q", stale)
		}
	}
}
