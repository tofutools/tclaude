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

	// The element the poller paints into, plus the station line (now the
	// channel picker's home) it sits above.
	must("'vegas-song'", "vegas.js targets the #vegas-song line")
	must("vegas-station", "vegas.js renders the station line")

	// The poll is tagged with the active channel so the proxy reads the
	// matching feed.
	must("/api/slop/nowplaying?channel=", "the poll names the active channel")

	// The channel picker: a <select> built from the catalog, persisted to
	// the backend on change. A typo here ships a dead dropdown.
	must("'vegas-channel'", "vegas.js builds the channel <select>")
	must("/api/slop/channel", "vegas.js persists the channel choice to the backend")
	must("function switchChannel", "the channel-change handler exists")
	must("function loadChannel", "the saved channel is loaded on activation")
	must(".vegas-channel {", "the channel select is styled")

	// The stereo-display readout the poller paints into #vegas-song: an
	// artist line above the title (the focal point). These fill the player
	// card so it doesn't read as an empty box.
	must("'vegas-artist'", "vegas.js paints the artist line")
	must("'vegas-title'", "vegas.js paints the title line")

	// The render path: a YouTube-search link opened in a new tab. rel
	// noopener guards the opened tab; target _blank keeps the dashboard.
	must("search_url", "vegas.js links the title to the prebuilt search URL")
	must("a.rel = 'noopener'", "the song link opens safely in a new tab")

	// Lifecycle: the poll starts with the player and is torn down when
	// music stops, so it never runs on the plain dashboard.
	must("startNowPlayingPoll();", "the poll starts when the player is built")
	must("stopNowPlayingPoll();", "the poll stops when music stops")

	// Elapsed counter: real time-on-air from the feed's start timestamp,
	// ticked every second (not a progress bar — the live stream has no
	// duration).
	must("started_at", "vegas.js reads the track start timestamp")
	must("'vegas-elapsed'", "vegas.js targets the elapsed-time element")
	must("function tickElapsed", "the 1s elapsed ticker exists")
	must("function formatElapsed", "elapsed seconds are formatted m:ss")

	// CSS hooks for the lines + the elapsed counter.
	must(".vegas-song {", "the song line is styled")
	must(".vegas-title {", "the title line is styled")
	must(".vegas-station {", "the station line is styled")
	must(".vegas-elapsed {", "the elapsed counter is styled")

	// The transport is a custom themed play/pause button. The native
	// <audio controls> bar was replaced — it couldn't be themed and, with
	// the meaningless live-stream seek bar hidden, rendered as an empty
	// white pill. Volume lives in the header mixer, not the player.
	must("'vegas-transport'", "vegas.js builds the custom transport row")
	must("'vegas-play'", "vegas.js builds the play/pause button")
	must(".vegas-play {", "the play/pause button is styled")
}
