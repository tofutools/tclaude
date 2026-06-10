package agentd

import (
	"io/fs"
	"strings"
	"testing"
)

// TestDashboardHTML_SlopVolume pins the wiring of the slop-mode volume
// mixer — the header 🎚️ popover (slop-volume.js) whose two sliders
// scale the Vegas radio (vegas.js) and the casino FX (slop-audio.js)
// and persist to config.json's "slop" block via /api/slop/volumes.
//
// Same playbook as TestDashboardHTML_SlopExtras: the feature is mostly
// client-side, so we string-search the embedded concatenation rather
// than running the JS. A renamed element id, a dropped bootstrap call,
// or a renamed endpoint would otherwise break the mixer silently in
// the browser.
func TestDashboardHTML_SlopVolume(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The module ships embedded — without this the import in
	// dashboard.js would 404 in the browser.
	if _, err := fs.ReadFile(dashboardAssetsFS, "js/slop-volume.js"); err != nil {
		t.Fatalf("embedded js/slop-volume.js missing: %v", err)
	}

	// Bootstrap wiring.
	must("export function bindSlopVolume", "slop-volume exports its entry-point")
	must("bindSlopVolume();", "dashboard.js installs the volume mixer at bootstrap")

	// HTML hooks — the HUD button, the popover, and the two sliders.
	for _, id := range []string{
		"slop-volume-btn", "slop-volume-pop", "slop-vol-music", "slop-vol-fx",
	} {
		must(`id="`+id+`"`, "dashboard.html carries the "+id+" element")
	}
	must("#slop-volume-pop", "dashboard.css styles the mixer popover")

	// The two audio owners expose the setters the mixer drives.
	must("export function setMusicVolume", "vegas.js exposes the music-volume setter")
	must("export function setEffectsVolume", "slop-audio.js exposes the FX-volume setter")

	// The native <audio controls> volume is mirrored back through the
	// tclaude:slopmusicvol event — pin both the emitter and a listener.
	must("new CustomEvent('tclaude:slopmusicvol'", "vegas.js surfaces native volume drags")
	must("addEventListener('tclaude:slopmusicvol'", "slop-volume mirrors native volume drags")

	// Persistence endpoint — the JS side of the path; the Go mux side is
	// covered functionally in dashboard_slop_test.go.
	must("'/api/slop/volumes'", "slop-volume.js persists via the volumes endpoint")
}
