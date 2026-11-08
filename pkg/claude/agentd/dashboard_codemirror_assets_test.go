package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardCodeMirrorVendorAssets pins the vendored CodeMirror bundle the
// Markdown editor runs on. The hash lives here and in
// dashboard/third_party/codemirror/README.md together, so a swap is deliberate.
func TestDashboardCodeMirrorVendorAssets(t *testing.T) {
	const name = "third_party/codemirror/codemirror.esm.min.mjs"
	const want = "8a904ef1d88f557696d16d89826f5fa39b8373a13f6cf0774f1622ea0be1e016"
	data, err := fs.ReadFile(dashboardAssetsFS, name)
	if err != nil {
		t.Fatalf("embedded dashboard asset %q not found: %v", name, err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		t.Errorf("embedded dashboard asset %q hash changed; update the vendored manifest intentionally", name)
	}
	// One self-contained file: an import of a sibling package would 404.
	bundle := string(data)
	if strings.Contains(bundle, "from\"") || strings.Contains(bundle, "from '") || strings.Contains(bundle, "import(\"") {
		t.Error("vendored CodeMirror bundle carries external imports; it must be one self-contained esbuild bundle")
	}
	for _, name := range []string{
		"third_party/codemirror/LICENSE-codemirror.txt",
		"third_party/codemirror/README.md",
	} {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil || len(data) == 0 {
			t.Errorf("embedded dashboard asset %q missing or empty: %v", name, err)
		}
	}
}

// TestDashboardCodeMirrorImportMap keeps the editor's library mapped and off
// the boot path: it serves the task forms only, so a dashboard load must not
// fetch it.
func TestDashboardCodeMirrorImportMap(t *testing.T) {
	html := string(dashboardIndexHTML)
	const mapping = `"codemirror": "/static/third_party/codemirror/codemirror.esm.min.mjs"`
	if !strings.Contains(html, mapping) {
		t.Errorf("dashboard import map missing %s", mapping)
	}
	for _, m := range bootVendorPreload {
		if strings.Contains(m, "codemirror") {
			t.Errorf("CodeMirror is preloaded on boot (%s); the editor imports it on demand", m)
		}
	}
	editor := dashboardAssetFile(t, "js/markdown-codemirror.js")
	if !strings.Contains(editor, "import('codemirror')") {
		t.Error("js/markdown-codemirror.js must load CodeMirror with a dynamic import('codemirror')")
	}
	for _, line := range strings.Split(editor, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "import") && strings.Contains(line, "'codemirror'") {
			t.Errorf("js/markdown-codemirror.js imports CodeMirror statically (%q); it must stay lazily loaded", strings.TrimSpace(line))
		}
	}
}
