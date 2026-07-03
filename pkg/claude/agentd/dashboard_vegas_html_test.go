package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_VegasTab pins the slop-mode "Vegas" music tab wiring:
// the vegas.js module ships embedded, exports the entry-point dashboard.js
// calls, slop.js broadcasts the state event it listens for, and the HTML +
// CSS hooks it depends on survive into dashboardAssets.
//
// Same playbook as TestDashboardHTML_SlopFx — the feature is purely
// client-side, so we string-search the embedded source rather than
// running the JS.
func TestDashboardHTML_VegasTab(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module itself is embedded — without this the import in
	// dashboard.js would 404 in the browser.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/vegas.js"); err != nil {
		t.Fatalf("embedded js/vegas.js missing: %v", err)
	}

	// Public surface + bootstrap wiring. A refactor that drops the call
	// loses the music silently.
	must("export function bindVegasMusic", "the entry-point dashboard.js calls")
	must("bindVegasMusic();", "dashboard.js installs the slop-music listener at bootstrap")

	// vegas.js gates on the tclaude:vegas event slop.js dispatches (fired
	// for both slop mode and the regular-mode opt-in); if that contract
	// breaks we want a test failure, not silent dead music. Both ends
	// asserted.
	must("new CustomEvent('tclaude:vegas'", "slop.js broadcasts Vegas-feature state changes")
	must("'tclaude:vegas'", "vegas.js listens for the Vegas state event")

	// The music source — an ad-free, embeddable ICE/MP3 stream (the
	// original YouTube video disabled embedding). Pin host + station so
	// a typo ships a silent/broken player and this fails instead.
	must("ice1.somafm.com", "the stream host is embedded")
	must("illstreet", "the lounge station is embedded")
	must("createElement('audio')", "vegas.js builds an <audio> player")
	must("audio.autoplay = true", "the player autoplays so music starts with the mode")

	// HTML hooks: the nav button, its section, and the player host
	// vegas.js injects the iframe into.
	must(`data-tab="vegas"`, "the Vegas nav button ships")
	must(`id="tab-vegas"`, "the Vegas tab section ships")
	must(`id="vegas-player"`, "the iframe host vegas.js targets ships")

	// Persistent "brought to you by SomaFM" credit — a static card-footer
	// link so the user always knows where the music comes from, even when
	// muted (the player chrome only links SomaFM on a stream error). Pin the
	// class, the somafm.com link, and the CSS hook so a refactor that drops
	// the attribution fails here.
	must(`class="vegas-credit"`, "the SomaFM credit line ships")
	must(`href="https://somafm.com/"`, "the credit links to SomaFM's site")
	must(".vegas-credit {", "the credit CSS ships")

	// CSS: the nav button is hidden by default — the tab must not leak into
	// a plain dashboard — and revealed in slop mode OR the regular-mode
	// opt-in (body.vegas), so both reveal selectors must ship.
	must("nav button[data-tab=\"vegas\"] { display: none; }",
		"the Vegas nav button is hidden in the plain dashboard")
	must("body.slop nav button[data-tab=\"vegas\"]",
		"the Vegas nav button is revealed in slop mode")
	must("body.vegas nav button[data-tab=\"vegas\"]",
		"the Vegas nav button is revealed by the regular-mode opt-in (body.vegas)")
}

// TestDashboardHTML_VegasRegularMode pins the opt-in that surfaces the
// Vegas music features (tab, volume HUD, radio) on the plain dashboard:
// the snapshot flag, the refresh.js → setVegasRegularMode wiring, the
// body.vegas CSS reveals for the volume HUD, and the Config-tab checkbox
// + its config.js round-trip. All client/server string contracts, like
// TestDashboardHTML_VegasTab.
func TestDashboardHTML_VegasRegularMode(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// slop.js owns the body.vegas class + the tclaude:vegas event; refresh.js
	// drives it off the snapshot flag every poll.
	must("export function setVegasRegularMode", "slop.js exposes the regular-mode toggle")
	must("export function isVegasActive", "slop.js exposes the combined slop-OR-vegas predicate")
	must("setVegasRegularMode(!!data.vegas_in_regular_mode)",
		"refresh.js applies the snapshot flag every poll")

	// The volume HUD (sound switch + mixer) is revealed by body.vegas too,
	// so the controls follow the music into regular mode.
	must("body.vegas .slop-hud",
		"the volume HUD is revealed by the regular-mode opt-in")
	// The casino-only credits tally + leaderboard are hidden in the
	// regular-mode (non-slop) Vegas view.
	must("body.vegas:not(.slop) .slop-credits",
		"the casino credits tally is hidden in regular mode")

	// Config tab: the checkbox + its populate/assemble round-trip through
	// the slop.vegas_in_regular_mode key.
	must(`id="cfg-slop-vegas-regular"`, "the Config-tab checkbox ships")
	must("cfg.slop && cfg.slop.vegas_in_regular_mode", "config.js populates the checkbox")
	must("slop.vegas_in_regular_mode = true", "config.js assembles the key on save")
}
