package agentd

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

func TestDashboardPreactRuntimeAssets(t *testing.T) {
	wantHashes := map[string]string{
		"vendor/preact/preact.module.js":           "850dcba8ed3535b0a3611495c405551b9887724885d3b8482207a03de365d64e",
		"vendor/preact/preact.module.js.map":       "a24b8606d61210775bbd11a742054c25b74f82c33bcde21efa1883253ce65630",
		"vendor/preact/hooks.module.js":            "a6ee626f2d01570592dd569a792e3f050154aa02890eead8c223fa3ed5aa3d5a",
		"vendor/preact/hooks.module.js.map":        "a13936803e904e19f2f154e953541c20dbbd0667c881f446e7aefafcfe487a97",
		"vendor/preact/signals.module.js":          "0439faa8ed059c955df6ab43bf02e67b886daf73adb795f6252ca9e783d68190",
		"vendor/preact/signals.module.js.map":      "a34390151735a6c7cce8342dce89a1b27efd571baa42fbae94440995b4beaadb",
		"vendor/preact/signals-core.module.js":     "bfbb64b74f7f06a4f7c6f6bb854cccb40d03f1e96305d43c41876cba581ea112",
		"vendor/preact/signals-core.module.js.map": "14f651f12e13f5f51b29fd1108c01a6408c9d87c0f49f0d96ffb19b0e1fc75a3",
		"vendor/preact/htm.module.js":              "ab33dd3f38059b9be4d5f5350128eefb2356639c4e0bbe9d9e8b3ba75847e9e4",
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
		"js/preact-loader.js",
		"js/island-lifecycle.js",
		"js/request-lifecycle.js",
		"js/async-load-state.js",
		"vendor/preact/LICENSE-preact.txt",
		"vendor/preact/LICENSE-signals.txt",
		"vendor/preact/LICENSE-htm.txt",
		"vendor/preact/README.md",
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

	loader := string(mustReadFS(dashboardAssetsFS, "js/preact-loader.js"))
	if strings.Contains(loader, "preact-probe") || strings.Contains(loader, "mountPreactRuntimeProbe") {
		t.Error("Preact loader retains the obsolete transition-era runtime probe")
	}
	for _, needle := range []string{"import('./shell-island.js')", "import('./groups-island.js')", "import('./links-island.js')"} {
		if !strings.Contains(loader, needle) {
			t.Errorf("Preact loader missing production runtime consumer %q", needle)
		}
	}
}

func TestDashboardPreactImportMap(t *testing.T) {
	html := string(dashboardIndexHTML)
	for _, mapping := range []string{
		`"preact": "/static/vendor/preact/preact.module.js"`,
		`"preact/hooks": "/static/vendor/preact/hooks.module.js"`,
		`"@preact/signals-core": "/static/vendor/preact/signals-core.module.js"`,
		`"@preact/signals": "/static/vendor/preact/signals.module.js"`,
		`"htm": "/static/vendor/preact/htm.module.js"`,
	} {
		if !strings.Contains(html, mapping) {
			t.Errorf("dashboard import map missing %s", mapping)
		}
	}
	mapAt := strings.Index(html, `<script type="importmap">`)
	entryAt := strings.Index(html, `<script type="module" src="/static/js/dashboard.js"></script>`)
	if mapAt < 0 || entryAt < 0 || mapAt > entryAt {
		t.Fatal("dashboard import map must appear before its module entry point")
	}
	if strings.Contains(html[mapAt:entryAt], "https://") || strings.Contains(html[mapAt:entryAt], "http://") {
		t.Error("dashboard import map must only resolve to embedded same-origin modules")
	}
}

func TestDashboardPreactRuntimeWired(t *testing.T) {
	if !strings.Contains(dashboardAssets, "await mountJobsFeature({") {
		t.Error("dashboard production Preact runtime is not wired")
	}
	for _, forbidden := range []string{"mountPreactRuntimeProbe", "preact-runtime-probe", "preact-probe.js"} {
		if strings.Contains(dashboardAssets, forbidden) {
			t.Errorf("dashboard retains obsolete runtime probe contract %q", forbidden)
		}
	}
}
