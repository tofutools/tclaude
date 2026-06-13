package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// The dashboard's sticky view/config prefs moved off the browser's
// localStorage (which is origin-scoped, and the dashboard's random
// per-start port made that reset every restart) and into the
// server-backed dashPrefs store (prefs.js → /api/dashboard/prefs →
// SQLite). These structural guards keep a future change from silently
// regressing a pref back onto localStorage, where it would once again
// vanish on restart — there is no JS test runner, so we pin the wiring
// against the embedded module source.

// prefsLocalStorageAllowlist is the only module permitted to call
// localStorage: the slop master-mute is deliberately a per-browser whim
// (its own comment makes that call), not shared/persistent config.
var prefsLocalStorageAllowlist = map[string]bool{
	"js/slop-audio.js": true,
}

// TestDashboardPrefs_NoStrayLocalStorage asserts that no dashboard
// module outside the allowlist calls localStorage.{get,set,remove}Item —
// every persistent pref must go through dashPrefs instead.
func TestDashboardPrefs_NoStrayLocalStorage(t *testing.T) {
	calls := []string{
		"localStorage.getItem(",
		"localStorage.setItem(",
		"localStorage.removeItem(",
	}
	for _, name := range dashboardJSModules() {
		if prefsLocalStorageAllowlist[name] {
			continue
		}
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("read embedded module %q: %v", name, err)
			continue
		}
		src := string(data)
		for _, c := range calls {
			if strings.Contains(src, c) {
				t.Errorf("%s calls %s — persistent prefs must use dashPrefs (prefs.js), "+
					"not localStorage, which resets on the dashboard's random per-start port", name, c)
			}
		}
	}
}

// TestDashboardPrefs_ClientWiring pins the prefs.js store shape and that
// the boot loads it before anything reads a pref.
func TestDashboardPrefs_ClientWiring(t *testing.T) {
	prefs, err := fs.ReadFile(dashboardAssetsFS, "js/prefs.js")
	if err != nil {
		t.Fatalf("read js/prefs.js: %v", err)
	}
	for _, needle := range []string{
		"const API = '/api/dashboard/prefs'", // talks to the right endpoint
		"async function initDashPrefs(",       // boot loader
		"getItem(key)",                        // localStorage-shaped API
		"setItem(key, value)",
		"removeItem(key)",
		"export { dashPrefs, initDashPrefs }",
	} {
		if !strings.Contains(string(prefs), needle) {
			t.Errorf("prefs.js missing %q — store wiring broken", needle)
		}
	}

	boot, err := fs.ReadFile(dashboardAssetsFS, "js/dashboard.js")
	if err != nil {
		t.Fatalf("read js/dashboard.js: %v", err)
	}
	bootSrc := string(boot)
	// The boot must await the pref load, and it must precede the first
	// refresh() (which renders, reading prefs) — otherwise a fresh page
	// renders against an empty cache and ignores every saved pref.
	awaitAt := strings.Index(bootSrc, "await initDashPrefs()")
	if awaitAt < 0 {
		t.Fatal("dashboard.js boot must `await initDashPrefs()` before rendering")
	}
	if refreshAt := strings.Index(bootSrc, "\n  refresh();"); refreshAt >= 0 && awaitAt > refreshAt {
		t.Error("dashboard.js: `await initDashPrefs()` must precede the first refresh()")
	}
}
