package agentd

import (
	"strings"
	"testing"
)

// TestDashboardHTML_NowPlaying pins the client wiring of the Vegas tab's
// "now playing" song line: vegas.js polls the proxy, targets the song
// element, and the CSS hooks it paints into survive into dashboardAssets.
// Like the other slop HTML tests, the feature is client-side, so we
// string-search the embedded source rather than running the JS.
func TestDashboardHTML_NowPlaying(t *testing.T) {
	must := func(needle, why string) {
		t.Helper()
		if !strings.Contains(dashboardAssets, needle) {
			t.Errorf("dashboard source missing %q (%s)", needle, why)
		}
	}

	// The poller hits agentd's SomaFM proxy. A typo'd path ships a silent
	// (always-empty) song line.
	must("/api/slop/nowplaying", "vegas.js polls the now-playing proxy")

	// The element the poller paints into, plus the static station line it
	// sits above.
	must("'vegas-song'", "vegas.js targets the #vegas-song line")
	must("vegas-station", "vegas.js renders the static station line")

	// The render path: a YouTube-search link opened in a new tab. rel
	// noopener guards the opened tab; target _blank keeps the dashboard.
	must("search_url", "vegas.js links the title to the prebuilt search URL")
	must("a.rel = 'noopener'", "the song link opens safely in a new tab")

	// Lifecycle: the poll starts with the player and is torn down when
	// music stops, so it never runs on the plain dashboard.
	must("startNowPlayingPoll();", "the poll starts when the player is built")
	must("stopNowPlayingPoll();", "the poll stops when music stops")

	// CSS hooks for the two lines.
	must(".vegas-song {", "the song line is styled")
	must(".vegas-station {", "the station line is styled")
}
