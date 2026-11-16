package agentd

import (
	"encoding/json"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestBootPreloadModules_ResolvesRealStaticGraph: every returned module is a
// real embedded asset (never a 404), the set is dedup'd, includes the entry and
// vendor targets, and excludes lazily dynamic-imported island modules.
func TestBootPreloadModules_ResolvesRealStaticGraph(t *testing.T) {
	for _, layout := range []string{"desktop", "phone"} {
		t.Run(layout, func(t *testing.T) { checkBootPreloadGraph(t, bootPreloadModules(dashboardAssetsFS, layout)) })
	}
}

func checkBootPreloadGraph(t *testing.T, mods []string) {
	t.Helper()
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

// TestInjectBootPreload_EmitsLinksAndTotal: the marker is replaced by one
// modulepreload link per module both layouts share, and a script that adds
// the links of the layout in force and sets the total to all of them.
func TestInjectBootPreload_EmitsLinksAndTotal(t *testing.T) {
	out := string(dashboardIndexHTML)
	if strings.Contains(out, bootPreloadMarker) {
		t.Fatalf("boot-preload marker was not replaced in the served HTML")
	}
	mods, desktop := bootPreloadSets(dashboardAssetsFS, "desktop")
	_, phone := bootPreloadSets(dashboardAssetsFS, "phone")

	// The served page names the versioned prefix (dashboard_static.go).
	linkRe := regexp.MustCompile(`<link rel="modulepreload" href="` + regexp.QuoteMeta(dashboardStaticBase) + `([^"]+)">`)
	links := linkRe.FindAllStringSubmatch(out, -1)
	if len(links) != len(mods) {
		t.Fatalf("emitted %d modulepreload links, expected %d", len(links), len(mods))
	}
	for _, l := range links {
		if _, err := fs.ReadFile(dashboardAssetsFS, l[1]); err != nil {
			t.Errorf("emitted modulepreload for non-existent asset: %s", l[1])
		}
	}

	// The layout script: the phone list, the desktop list, then the shared
	// count the total adds the chosen list to. Its paths are versioned too.
	scriptRe := regexp.MustCompile(`'phone'\?(\[[^\]]*\]):(\[[^\]]*\]);.*window\.__BOOT_MODULE_TOTAL__=(\d+)\+own\.length;`)
	m := scriptRe.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the layout preload script is not in the served HTML")
	}
	for i, want := range [][]string{phone, desktop} {
		var got []string
		if err := json.Unmarshal([]byte(m[i+1]), &got); err != nil {
			t.Fatalf("layout list %q: %v", m[i+1], err)
		}
		if len(got) != len(want) || len(want) == 0 {
			t.Fatalf("layout list has %d modules, want %d (non-empty)", len(got), len(want))
		}
		for j, href := range got {
			if href != dashboardStaticBase+want[j] {
				t.Errorf("layout module %d = %s, want %s", j, href, dashboardStaticBase+want[j])
			}
		}
	}
	total, _ := strconv.Atoi(m[3])
	if total != len(mods) {
		t.Errorf("__BOOT_MODULE_TOTAL__ adds %d shared modules, but %d links emitted", total, len(mods))
	}

	// The inline counter reads these DOM hooks; keep them in lockstep with the
	// markup so a rename can't silently break the progress bar.
	for _, id := range []string{`id="boot-progress"`, `id="boot-progress-fill"`, `id="boot-progress-count"`} {
		if !strings.Contains(out, id) {
			t.Errorf("served HTML missing boot-progress hook %s", id)
		}
	}
}

// TestBootProgress_AppPhaseEventContract: the inline boot counter's app band
// only moves if dashboard.js actually dispatches the `tclaude:boot-app`
// milestone events the counter listens for. Guard both sides of that
// cross-file contract so a rename in either file can't silently park the bar
// at the module-band ceiling again.
func TestBootProgress_AppPhaseEventContract(t *testing.T) {
	const event = "tclaude:boot-app"
	if !strings.Contains(string(dashboardIndexHTML), event) {
		t.Errorf("served HTML inline boot counter does not listen for %s", event)
	}
	entry, err := fs.ReadFile(dashboardAssetsFS, bootPreloadEntry)
	if err != nil {
		t.Fatalf("read %s: %v", bootPreloadEntry, err)
	}
	if !strings.Contains(string(entry), event) {
		t.Errorf("%s does not dispatch %s boot milestones", bootPreloadEntry, event)
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

// TestBootPreloadCoversTheBootIslands: the islands a boot awaits load their
// modules by dynamic import(); their graphs are in that layout's preload set,
// or each boot walks them as a waterfall ("Starting shell…"). A layout does
// not preload the other's islands (tk_33321868).
func TestBootPreloadCoversTheBootIslands(t *testing.T) {
	for _, tc := range []struct {
		layout    string
		want, not []string
		desc      []string
	}{
		{"desktop", []string{"js/shell-island.js", "js/palette-island.js", "js/jobs-island.js"},
			[]string{"js/phone-shell-island.js", "js/phone-automations-island.js"}, bootLayoutIslands["desktop"]},
		{"phone", []string{"js/shell-island.js", "js/palette-island.js", "js/phone-shell-island.js", "js/phone-automations-island.js"},
			[]string{"js/jobs-island.js"}, bootLayoutIslands["phone"]},
	} {
		t.Run(tc.layout, func(t *testing.T) {
			roots := append(bootIslandRoots(dashboardAssetsFS, bootIslandDescriptors), bootIslandRoots(dashboardAssetsFS, tc.desc)...)
			for _, want := range tc.want {
				if !slices.Contains(roots, want) {
					t.Errorf("boot island roots %v miss %s: is a descriptor renamed?", roots, want)
				}
			}
			if slices.Contains(roots, "js/plugins-state.js") {
				t.Errorf("roots %v reach past the boot islands into the plugins descriptor", roots)
			}
			mods := bootPreloadModules(dashboardAssetsFS, tc.layout)
			for _, root := range roots {
				if !slices.Contains(mods, root) {
					t.Errorf("preload set misses boot island module %s", root)
				}
			}
			for _, other := range tc.not {
				if slices.Contains(mods, other) {
					t.Errorf("the %s preload carries the other layout's %s", tc.layout, other)
				}
			}
		})
	}
}
