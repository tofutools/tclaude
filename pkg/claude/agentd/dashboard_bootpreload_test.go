package agentd

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestBootPreloadModules_ResolvesRealStaticGraph: every returned module is a
// real embedded asset (never a 404), the set is dedup'd, includes the entry and
// vendor targets, and excludes lazily dynamic-imported island modules.
func TestBootPreloadModules_ResolvesRealStaticGraph(t *testing.T) {
	mods := bootPreloadModules(dashboardAssetsFS)
	if len(mods) < 20 {
		t.Fatalf("expected a substantial static graph, got %d modules: %v", len(mods), mods)
	}
	seen := map[string]bool{}
	for _, m := range mods {
		if seen[m] {
			t.Errorf("duplicate module in preload set: %s", m)
		}
		seen[m] = true
		if _, err := fs.ReadFile(dashboardAssetsFS, m); err != nil {
			t.Errorf("preload module does not resolve to an embedded asset: %s (%v)", m, err)
		}
	}
	if !seen[bootPreloadEntry] {
		t.Errorf("entry %s missing from preload set", bootPreloadEntry)
	}
	for _, v := range bootVendorPreload {
		if !seen[v] {
			t.Errorf("vendor target %s missing from preload set", v)
		}
	}
	// Sanity: an island loaded only via dynamic import() must not ride the boot
	// path. plugins-island.js is dynamically imported per-tab.
	if seen["js/plugins-island.js"] {
		t.Errorf("dynamically-imported island js/plugins-island.js leaked into the static boot graph")
	}
}

// TestInjectBootPreload_EmitsLinksAndTotal: the marker is replaced by a
// module-total script plus one modulepreload link per module, and the count in
// the script matches the number of links emitted.
func TestInjectBootPreload_EmitsLinksAndTotal(t *testing.T) {
	out := string(dashboardIndexHTML)
	if strings.Contains(out, bootPreloadMarker) {
		t.Fatalf("boot-preload marker was not replaced in the served HTML")
	}
	mods := bootPreloadModules(dashboardAssetsFS)

	linkRe := regexp.MustCompile(`<link rel="modulepreload" href="/static/([^"]+)">`)
	links := linkRe.FindAllStringSubmatch(out, -1)
	if len(links) != len(mods) {
		t.Fatalf("emitted %d modulepreload links, expected %d", len(links), len(mods))
	}
	for _, l := range links {
		if _, err := fs.ReadFile(dashboardAssetsFS, l[1]); err != nil {
			t.Errorf("emitted modulepreload for non-existent asset: %s", l[1])
		}
	}

	totalRe := regexp.MustCompile(`window\.__BOOT_MODULE_TOTAL__=(\d+);`)
	m := totalRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("__BOOT_MODULE_TOTAL__ assignment not found in served HTML")
	}
	total, _ := strconv.Atoi(m[1])
	if total != len(mods) {
		t.Errorf("__BOOT_MODULE_TOTAL__ = %d, but %d modules emitted", total, len(mods))
	}

	// The inline counter reads these DOM hooks; keep them in lockstep with the
	// markup so a rename can't silently break the progress bar.
	for _, id := range []string{`id="boot-progress"`, `id="boot-progress-fill"`, `id="boot-progress-count"`} {
		if !strings.Contains(out, id) {
			t.Errorf("served HTML missing boot-progress hook %s", id)
		}
	}
}

// TestInjectBootPreload_PanicsWithoutMarker: the marker is a build-time
// invariant; losing it must fail loudly, not silently ship a dashboard with no
// preload or progress.
func TestInjectBootPreload_PanicsWithoutMarker(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatalf("injectBootPreload did not panic on HTML missing the marker")
		}
	}()
	injectBootPreload([]byte("<html><head></head><body></body></html>"), dashboardAssetsFS)
}
