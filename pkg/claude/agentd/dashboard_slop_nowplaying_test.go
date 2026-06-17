package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for /api/slop/nowplaying — agentd's SomaFM recent-songs proxy that
// feeds the Vegas tab's "now playing" line (js/vegas.js). The network
// boundary (nowPlayingFetchBytes) is stubbed so nothing here touches
// SomaFM; the XML parsing, charset decoding, caching, and auth gate are
// the system under test.

// isoLatin1Feed builds a SomaFM-shaped songs feed declared ISO-8859-1,
// with a raw 0xE9 byte (é in latin-1) inside the first track to prove the
// charset decode — a naive UTF-8 read would mangle it.
func isoLatin1Feed() []byte {
	const head = `<?xml version="1.0" encoding="ISO-8859-1"?>` + "\n" +
		`<songs id="illstreet">` + "\n" +
		`<song><title><![CDATA[Caf` // + 0xE9 + ...
	const mid = ` Society]]></title><artist><![CDATA[Andr` // + 0xE9 + ...
	const tail = ` Previn]]></artist><album><![CDATA[Live]]></album><albumart><![CDATA[]]></albumart><date>1781704016</date></song>` + "\n" +
		`<song><title><![CDATA[Older Tune]]></title><artist><![CDATA[Someone]]></artist><album><![CDATA[]]></album><albumart><![CDATA[]]></albumart><date>1781703800</date></song>` + "\n" +
		`</songs>`
	b := []byte(head)
	b = append(b, 0xE9) // é
	b = append(b, []byte(mid)...)
	b = append(b, 0xE9) // é
	b = append(b, []byte(tail)...)
	return b
}

func resetNowPlayingCache() {
	nowPlayingMu.Lock()
	nowPlayingCache = nil
	nowPlayingExpiry = time.Time{}
	nowPlayingMu.Unlock()
}

// stubNowPlaying swaps the network boundary for the duration of a test and
// clears the shared cache before and after so cases don't bleed.
func stubNowPlaying(t *testing.T, fn func(ctx context.Context) ([]byte, error)) {
	t.Helper()
	prev := nowPlayingFetchBytes
	nowPlayingFetchBytes = fn
	resetNowPlayingCache()
	t.Cleanup(func() {
		nowPlayingFetchBytes = prev
		resetNowPlayingCache()
	})
}

func TestParseNowPlaying_FirstSongAndLatin1(t *testing.T) {
	np, err := parseNowPlaying(isoLatin1Feed())
	require.NoError(t, err)
	require.NotNil(t, np, "a feed with songs must yield the on-air track")

	// The FIRST <song> is the on-air one, decoded from latin-1 to UTF-8.
	assert.Equal(t, "Café Society", np.Title)
	assert.Equal(t, "André Previn", np.Artist)
	assert.Equal(t, "Live", np.Album)

	// The first song's <date> is its start time — the dashboard counts
	// elapsed time up from it.
	assert.Equal(t, int64(1781704016), np.StartedAt)

	// The search link points at YouTube for "Artist Title", URL-escaped.
	assert.True(t, strings.HasPrefix(np.SearchURL, "https://www.youtube.com/results?search_query="),
		"search_url must be a YouTube search, got %q", np.SearchURL)
	assert.Contains(t, np.SearchURL, "Caf%C3%A9", "the é must be percent-encoded UTF-8 in the query")
}

func TestParseNowPlaying_EmptyAndBlankAreNoSong(t *testing.T) {
	cases := map[string]string{
		"no songs":     `<?xml version="1.0" encoding="ISO-8859-1"?><songs id="illstreet"></songs>`,
		"blank fields": `<?xml version="1.0" encoding="ISO-8859-1"?><songs><song><title><![CDATA[ ]]></title><artist><![CDATA[]]></artist></song></songs>`,
	}
	for name, feed := range cases {
		np, err := parseNowPlaying([]byte(feed))
		require.NoError(t, err, name)
		assert.Nil(t, np, "%s must produce no song", name)
	}
}

func TestParseNowPlaying_StartedAtDegradesToZero(t *testing.T) {
	// A missing or garbled <date> must not drop the track — it just means
	// "no elapsed counter" (StartedAt 0), which the UI reads as "hide it".
	cases := map[string]string{
		"no date":      `<?xml version="1.0" encoding="ISO-8859-1"?><songs><song><title><![CDATA[T]]></title><artist><![CDATA[A]]></artist></song></songs>`,
		"garbage date": `<?xml version="1.0" encoding="ISO-8859-1"?><songs><song><title><![CDATA[T]]></title><artist><![CDATA[A]]></artist><date>not-a-number</date></song></songs>`,
		"zero date":    `<?xml version="1.0" encoding="ISO-8859-1"?><songs><song><title><![CDATA[T]]></title><artist><![CDATA[A]]></artist><date>0</date></song></songs>`,
	}
	for name, feed := range cases {
		np, err := parseNowPlaying([]byte(feed))
		require.NoError(t, err, name)
		require.NotNil(t, np, "%s: the track must survive a bad date", name)
		assert.Equal(t, "T", np.Title, name)
		assert.Equal(t, int64(0), np.StartedAt, "%s: bad date → no elapsed", name)
	}
}

func TestParseNowPlaying_MalformedErrors(t *testing.T) {
	_, err := parseNowPlaying([]byte(`<songs><song><title>unterminated`))
	assert.Error(t, err, "garbage XML must surface as an error (caller fails soft)")
}

func TestYouTubeSearchURL(t *testing.T) {
	assert.Equal(t, "", youtubeSearchURL("", ""), "nothing to search → empty")
	assert.Equal(t, "", youtubeSearchURL("  ", "  "), "whitespace only → empty")

	got := youtubeSearchURL("Dean Martin", "Ain't That a Kick in the Head")
	assert.Equal(t, "https://www.youtube.com/results?search_query=Dean+Martin+Ain%27t+That+a+Kick+in+the+Head", got)

	// Artist-only / title-only still build a sensible query (no stray
	// leading/trailing space).
	assert.Equal(t, "https://www.youtube.com/results?search_query=Solo+Artist", youtubeSearchURL("Solo Artist", ""))
	assert.Equal(t, "https://www.youtube.com/results?search_query=Solo+Title", youtubeSearchURL("", "Solo Title"))
}

// serveNowPlaying routes a request through a fresh mux carrying just the
// now-playing route, the same dispatch a browser poll takes.
func serveNowPlaying(r *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/slop/nowplaying", handleDashboardSlopNowPlayingAPI)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestNowPlayingAPI_ReturnsTrack(t *testing.T) {
	withDashboardAuth(t)
	stubNowPlaying(t, func(context.Context) ([]byte, error) { return isoLatin1Feed(), nil })

	w := serveNowPlaying(dashboardRequest(http.MethodGet, "/api/slop/nowplaying", ""))
	require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())

	var np nowPlaying
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &np))
	assert.Equal(t, "Café Society", np.Title)
	assert.Equal(t, "André Previn", np.Artist)
	assert.Contains(t, np.SearchURL, "youtube.com")
}

func TestNowPlayingAPI_FailSoftEmptyObject(t *testing.T) {
	withDashboardAuth(t)
	// Unreachable feed → empty payload, still 200, so the UI just hides
	// the song line instead of showing a broken-radio error.
	stubNowPlaying(t, func(context.Context) ([]byte, error) { return nil, errors.New("network down") })

	w := serveNowPlaying(dashboardRequest(http.MethodGet, "/api/slop/nowplaying", ""))
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{}`, w.Body.String(), "an unreachable feed must degrade to an empty object")
}

func TestNowPlayingAPI_CachesUpstream(t *testing.T) {
	withDashboardAuth(t)
	var calls int
	stubNowPlaying(t, func(context.Context) ([]byte, error) {
		calls++
		return isoLatin1Feed(), nil
	})

	for range 3 {
		w := serveNowPlaying(dashboardRequest(http.MethodGet, "/api/slop/nowplaying", ""))
		require.Equal(t, http.StatusOK, w.Code)
	}
	assert.Equal(t, 1, calls, "three polls within the TTL must hit SomaFM once")
}

func TestNowPlayingAPI_RequiresAuth(t *testing.T) {
	withDashboardAuth(t)
	stubNowPlaying(t, func(context.Context) ([]byte, error) { return isoLatin1Feed(), nil })

	// No cookie/Origin → rejected before any fetch.
	r := httptest.NewRequest(http.MethodGet, "/api/slop/nowplaying", nil)
	w := serveNowPlaying(r)
	assert.Equal(t, http.StatusForbidden, w.Code, "an unauthenticated poll must be refused")
}

func TestNowPlayingAPI_RejectsNonGet(t *testing.T) {
	withDashboardAuth(t)
	stubNowPlaying(t, func(context.Context) ([]byte, error) { return isoLatin1Feed(), nil })

	w := serveNowPlaying(dashboardRequest(http.MethodPost, "/api/slop/nowplaying", `{}`))
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// TestSlopNowPlaying_ChannelMatchesVegasJS pins the server channel to the
// station vegas.js actually streams. If someone swaps one station and not
// the other, the song line would show tracks from the wrong channel — this
// fails instead.
func TestSlopNowPlaying_ChannelMatchesVegasJS(t *testing.T) {
	assert.Contains(t, somaSongsURL, somaChannel, "the songs-feed URL must use the channel constant")

	vegas, err := fs.ReadFile(dashboardAssetsFS, "js/vegas.js")
	require.NoError(t, err)
	src := string(vegas)
	assert.Contains(t, src, somaChannel, "vegas.js must stream the same SomaFM channel the proxy reads")
	assert.Contains(t, src, "/api/slop/nowplaying", "vegas.js must poll the now-playing proxy")
}
