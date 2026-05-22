package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_SlopFx pins the slop-mode visual feedback wiring:
// the slop-fx.js module ships embedded, exports the two entry-points
// dashboard.js + modal-spawn.js call, and the CSS classes its DOM
// uses survive into dashboardAssets.
//
// Same playbook as the other dashboard render guards
// (TestDashboardHTML_OptionsMenu et al.) — the effect is purely
// client-side, so we string-search the embedded source rather than
// running the JS.
func TestDashboardHTML_SlopFx(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module itself is embedded — without this the import in
	// dashboard.js / modal-spawn.js would 404 in the browser.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/slop-fx.js"); err != nil {
		t.Fatalf("embedded js/slop-fx.js missing: %v", err)
	}

	// Public surface of the module.
	must("export function bindSlopClickFx",
		"the click-burst entry-point dashboard.js calls")
	must("export function slopJackpot",
		"the celebration entry-point modal-spawn.js calls")

	// Wiring on both sides — a future refactor that drops either call
	// loses the feature silently.
	must("bindSlopClickFx();",
		"dashboard.js installs the delegated click listener once at bootstrap")
	must("slopJackpot();",
		"modal-spawn.js fires the celebration on a successful spawn")

	// slop-fx.js leans on isSlopActive() for its gating; if that
	// helper is renamed/removed we want a test break here, not a
	// silent always-off effect.
	must("export function isSlopActive",
		"slop.js exposes the body.slop check the fx module reads on every click")

	// CSS hooks the JS creates by classname. The animation defaults
	// for --dx/--dy/--rot need typed units so the CSS engine doesn't
	// reject the keyframe end-state.
	must(".slop-coin {",
		"per-coin styles ship — without them the spawned span has no animation")
	must("@keyframes slop-coin-arc",
		"the coin-arc keyframes ship")
	must("var(--dx, 0px)",
		"--dx defaults to a typed length so translate() parses on un-set coins")
	must(".slop-jackpot {",
		"the JACKPOT banner styles ship")
	must("@keyframes slop-jackpot-flash",
		"the jackpot flash keyframes ship")
	must("@media (prefers-reduced-motion: reduce)",
		"both effects are CSS-gated on the OS reduce-motion preference (belt + braces with the JS check)")
}
