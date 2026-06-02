package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_SlopExtras pins the wiring for the three slop-mode
// extras that hang off slop-fx's `tclaude:slopfx` event bus — casino
// sound FX (slop-audio.js), the credits counter + high-rollers
// leaderboard (slop-credits.js), and the Konami / lever / confetti
// spectacle (slop-spectacle.js).
//
// Same playbook as TestDashboardHTML_SlopFx / _VegasTab: the features are
// purely client-side, so we string-search the embedded concatenation
// rather than running the JS. A rename on either side of the bus, a
// dropped bootstrap call, or a missing HTML/CSS hook would otherwise
// break the feature silently in the browser.
func TestDashboardHTML_SlopExtras(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// Every new module ships embedded — without this the import in
	// dashboard.js would 404 in the browser.
	for _, mod := range []string{"js/slop-audio.js", "js/slop-credits.js", "js/slop-spectacle.js"} {
		if _, err := fs.ReadFile(dashboardAssetsFS, mod); err != nil {
			t.Fatalf("embedded %s missing: %v", mod, err)
		}
	}

	// ─── The shared event bus ───────────────────────────────────────
	// slop-fx is the emitter; the three extras subscribe. Pin both ends
	// so a rename of the event name or the exported emitter fails here
	// rather than going silently dead.
	must("export function emitSlopFx", "slop-fx exports the bus emitter")
	must("new CustomEvent('tclaude:slopfx'", "slop-fx dispatches the fx bus")
	must("'tclaude:slopfx'", "the extras listen on the fx bus")

	// ─── Bootstrap wiring (dashboard.js) ────────────────────────────
	for _, b := range []struct{ exp, call, why string }{
		{"export function bindSlopAudio", "bindSlopAudio();", "casino sound FX"},
		{"export function bindSlopCredits", "bindSlopCredits();", "credits + leaderboard"},
		{"export function bindSlopSpectacle", "bindSlopSpectacle();", "Konami / lever / confetti"},
	} {
		must(b.exp, "module exports the entry-point for "+b.why)
		must(b.call, "dashboard.js installs "+b.why+" at bootstrap")
	}

	// ─── Casino sound FX + master switch (slop-audio.js) ────────────
	// Synthesized via Web Audio — no asset files. The header button is
	// the MASTER switch for all slop sound (FX + Vegas radio): it owns a
	// persisted preference, exports its state, and broadcasts
	// tclaude:slopsound so vegas.js can start/stop the music off one
	// button. Pin all of that.
	must("AudioContext", "slop-audio synthesizes via Web Audio (no asset files)")
	must("tclaude.slop.sound", "the master sound preference persists under a stable key")
	must(`id="slop-sound-btn"`, "the header master sound toggle ships")
	must("export function isSlopSoundEnabled", "slop-audio exposes the master state vegas.js reads")
	must("new CustomEvent('tclaude:slopsound'", "the toggle broadcasts the master sound state")
	must("'tclaude:slopsound'", "vegas.js listens for the master sound state")
	must("isSlopSoundEnabled", "vegas.js gates the radio on the master switch")

	// ─── Credits + leaderboard (slop-credits.js) ────────────────────
	must(`id="slop-credits"`, "the header credits counter ships")
	must(`id="vegas-leaderboard"`, "the Vegas high-rollers leaderboard host ships")
	must(".slop-credits.slop-credits-bump", "the credits bump animation styles ship")
	must(".vegas-leaderboard", "the leaderboard card styles ship")

	// ─── Spectacle (slop-spectacle.js) ──────────────────────────────
	// The Konami sequence drives the mega-jackpot; the lever spins every
	// machine; confetti rains on big wins. Pin the lever element, the
	// confetti keyframes, the shake, and that slop-fx still exports the
	// two helpers the spectacle reuses.
	must(`id="slop-lever"`, "the side pull-lever element ships")
	must("export function pullAllMachines", "slop-fx exports the global pull the lever calls")
	must("export function showJackpotBanner", "slop-fx exports the banner the mega-jackpot reuses")
	must(".slop-confetti", "confetti piece styles ship")
	must("@keyframes slop-confetti-fall", "the confetti fall keyframes ship")
	must("body.slop-shake", "the mega-jackpot screen-shake styles ship")

	// Every lever pull must give obvious feedback regardless of a win:
	// a coin fountain (slopCoinBurst), the 'lever' sound, and a punchy
	// yank animation. Pin all three so a refactor can't quietly make the
	// lever feel dead again.
	must("export function slopCoinBurst", "slop-fx exports the lever coin fountain")
	must("emitSlopFx('lever')", "the lever pull announces itself on the bus")
	must("case 'lever'", "slop-audio plays the lever ka-chunk")
	must("@keyframes slop-lever-yank-stick", "the punchy lever yank keyframes ship")

	// ─── Slop-only chrome stays out of the plain dashboard ──────────
	// The HUD + lever must not leak into the non-slop dashboard.
	must(".slop-hud { display: none; }", "the HUD is hidden in the plain dashboard")
	must("#slop-lever { display: none; }", "the lever is hidden in the plain dashboard")
	must("body.slop .slop-hud", "the HUD is revealed only in slop mode")
	must("body.slop #slop-lever", "the lever is revealed only in slop mode")

	// Belt + braces: the new motion is reduced-motion gated, matching
	// the rest of slop's CSS.
	must("@media (prefers-reduced-motion: reduce)",
		"the new effects are CSS-gated on the OS reduce-motion preference")
}
