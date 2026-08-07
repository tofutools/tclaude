package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardMarkdownVendorAssets pins the vendored markdown-it bundle. A
// silent swap of a parser that renders agent-published text is exactly the
// change that must be deliberate, so the hashes live here and in
// dashboard/vendor/markdown-it/README.md together.
func TestDashboardMarkdownVendorAssets(t *testing.T) {
	wantHashes := map[string]string{
		"vendor/markdown-it/markdown-it.esm.min.mjs":     "eb0a6cb2beb08326ea4d3e0e3b25ac72c1e6f119a619d9bbe061e72000ffa118",
		"vendor/markdown-it/markdown-it.esm.min.mjs.map": "a1fccb4bda2e184b3f5e25b8dd7d020bedc30975e0e8bfec89d03811aee3312a",
	}
	for name, want := range wantHashes {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("embedded dashboard asset %q not found: %v", name, err)
			continue
		}
		got := sha256.Sum256(data)
		if hex.EncodeToString(got[:]) != want {
			t.Errorf("embedded dashboard asset %q hash changed; update the vendored manifest intentionally", name)
		}
	}

	for _, name := range []string{
		"vendor/markdown-it/LICENSE-markdown-it.txt",
		"vendor/markdown-it/README.md",
	} {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("embedded dashboard asset %q not found: %v", name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("embedded dashboard asset %q is empty", name)
		}
	}

	// The browser bundle is self-contained. If an upgrade ever brings one that
	// imports its dependencies instead, the module would 404 at runtime.
	bundle := string(mustReadFS(dashboardAssetsFS, "vendor/markdown-it/markdown-it.esm.min.mjs"))
	if strings.Contains(bundle, "from\"") || strings.Contains(bundle, "from '") {
		t.Error("vendored markdown-it bundle carries external imports; it must be the self-contained dist/browser build")
	}
}

func TestDashboardMarkdownImportMap(t *testing.T) {
	html := string(dashboardIndexHTML)
	const mapping = `"markdown-it": "/static/vendor/markdown-it/markdown-it.esm.min.mjs"`
	if !strings.Contains(html, mapping) {
		t.Errorf("dashboard import map missing %s", mapping)
	}
	// The parser is ~130 KiB serving one optional surface. It must stay out of
	// the boot graph, or every dashboard load pays for it.
	for _, m := range bootVendorPreload {
		if strings.Contains(m, "markdown-it") {
			t.Errorf("markdown-it is preloaded on boot (%s); the viewer imports it on demand", m)
		}
	}
	if strings.Contains(dashboardAssetFile(t, "js/dashboard.js"), "markdown") {
		t.Error("the dashboard entrypoint statically reaches the Markdown viewer; it must stay lazily loaded")
	}
}

// TestDashboardMarkdownRendersWithoutHTMLInjection pins the property the whole
// design rests on: agent-published Markdown becomes Preact vnodes drawn from an
// allowlist, never an HTML string. Behaviour is covered by the jstest suites;
// this fails the build if the implementation ever takes the innerHTML shortcut,
// which would silently turn a document into markup.
func TestDashboardMarkdownRendersWithoutHTMLInjection(t *testing.T) {
	for _, name := range []string{"js/markdown-model.js", "js/markdown-document.js", "js/markdown-attachment.js"} {
		source := dashboardAssetFile(t, name)
		for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "dangerouslySetInnerHTML", ".render("} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s uses %q; the Markdown viewer must build vnodes from the token stream, not inject markup", name, forbidden)
			}
		}
	}
	// The parser option is the one property that has no behavioural test to
	// stand on: with html:true the walk would still produce an allowlisted
	// tree, just one built from markup the document supplied. Everything else
	// this file could assert about the model — the token walk, the link
	// hardening, the URL rejections — is pinned by behaviour in
	// jstest/markdown-model.test.mjs instead, where a passing string proves
	// something.
	if model := dashboardAssetFile(t, "js/markdown-model.js"); !strings.Contains(model, "html: false") {
		t.Error("js/markdown-model.js must construct markdown-it with html:false, " +
			"or raw HTML in an agent's document becomes markup")
	}
}

// TestDashboardMarkdownViewerWired keeps both notification surfaces rendering
// the document. The drawer and Messages render attachments independently, so a
// file readable in one and download-only in the other is a real and easy
// regression.
func TestDashboardMarkdownViewerWired(t *testing.T) {
	// Naming the component is enough: the module graph test resolves the import
	// it must come from, so pinning the import statement's exact spelling would
	// only break on a reformat.
	for _, name := range []string{"js/groups-notification-reader.js", "js/mail-island.js"} {
		if !strings.Contains(dashboardAssetFile(t, name), "MarkdownAttachment") {
			t.Errorf("%s does not render published Markdown documents", name)
		}
	}

	source := dashboardAssetFile(t, "js/markdown-attachment.js")
	for needle, why := range map[string]string{
		`role="dialog"`:     "the document viewer exposes dialog semantics",
		`aria-modal="true"`: "the document viewer is modal",
	} {
		if !strings.Contains(source, needle) {
			t.Errorf("markdown attachment source missing %q (%s)", needle, why)
		}
	}

	css := dashboardAssetFile(t, "dashboard.css")
	for _, rule := range []string{
		".markdown-preview-dialog {", ".markdown-document {",
		".markdown-attachment-document {", ".human-attachment-markdown-trigger {",
		".markdown-preview-mode {", ".markdown-preview-document {",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("dashboard CSS is missing the %s rule", rule)
		}
	}
}
