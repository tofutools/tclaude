package agentd

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"
)

// dashboard_slop_nowplaying.go backs the "now playing" song line in the
// slop-mode Vegas tab (js/vegas.js).
//
// The lounge radio streams as a plain <audio> element, and browsers do
// NOT expose ICY/Icecast in-stream metadata to script — so the current
// song's title/artist can't be read from the stream itself. SomaFM
// publishes a tiny per-channel recent-songs feed instead; we fetch + parse
// it here, server-side, rather than from the browser, because:
//
//   - No CORS dependency. A browser fetch of the feed would need SomaFM to
//     send Access-Control-Allow-Origin; this path doesn't care.
//   - The feed is ISO-8859-1; Go decodes it correctly (CharsetReader
//     below) instead of us fighting charset quirks in JS.
//   - The channel id stays pinned to one constant, next to the rest of the
//     slop server config (/api/slop/volumes).
//
// This is the ONLY outbound HTTP agentd makes. It is best-effort and
// fail-soft: any error returns an empty payload and the UI simply hides
// the song line (the music itself is unaffected — it plays from the
// browser). The browser polls this every ~30s while music plays; a short
// server-side cache keeps multiple dashboard tabs from each hitting SomaFM.

// somaChannel is the SomaFM channel id whose recent-songs feed we read. It
// MUST match the station js/vegas.js streams (VEGAS_STREAM there); a test
// (TestSlopNowPlaying_ChannelMatchesVegasJS) pins both so they can't
// drift. Swapping the station is a two-line edit: here and in vegas.js.
const somaChannel = "illstreet"

// somaSongsURL is SomaFM's recent-songs feed for the channel — a small XML
// document whose FIRST <song> is the track currently on air.
var somaSongsURL = "https://somafm.com/songs/" + somaChannel + ".xml"

const (
	nowPlayingTimeout  = 5 * time.Second
	nowPlayingCacheTTL = 12 * time.Second
)

// nowPlaying is the wire shape the dashboard renders. Empty fields render
// as "no song line"; SearchURL is a prebuilt "go hear this exact track"
// link the title links to.
type nowPlaying struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album"`
	SearchURL string `json:"search_url"`
}

// somaSongsDoc is the subset of SomaFM's songs XML we parse. encoding/xml
// unwraps the CDATA in each field for us.
type somaSongsDoc struct {
	Songs []struct {
		Title  string `xml:"title"`
		Artist string `xml:"artist"`
		Album  string `xml:"album"`
	} `xml:"song"`
}

var (
	nowPlayingMu     sync.Mutex
	nowPlayingCache  *nowPlaying
	nowPlayingExpiry time.Time

	// nowPlayingFetchBytes is the network boundary — swapped in tests so
	// they never hit SomaFM. Production reads the live feed.
	nowPlayingFetchBytes = fetchSomaSongsLive

	// nowPlayingClient bounds the live fetch so a hung SomaFM can't pile
	// up; the per-request context adds a hard deadline on top.
	nowPlayingClient = &http.Client{Timeout: nowPlayingTimeout}
)

// handleDashboardSlopNowPlayingAPI serves GET /api/slop/nowplaying with
// the track currently on the Vegas radio. Mounted on the loopback
// dashboard mux by registerDashboardEditRoutes. Dashboard-only, like
// /api/slop/volumes — agents have no business reading the casino's radio.
func handleDashboardSlopNowPlayingAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	if np := nowPlayingCached(); np != nil {
		writeJSON(w, http.StatusOK, np)
		return
	}
	// Fail-soft: empty object, 200. The UI hides the song line on empty
	// rather than showing an error — a metadata blip shouldn't look like
	// the radio broke.
	writeJSON(w, http.StatusOK, map[string]any{})
}

// nowPlayingCached returns the cached track when fresh, else fetches once
// and refills. Returns nil when the feed is unreachable or empty.
func nowPlayingCached() *nowPlaying {
	nowPlayingMu.Lock()
	if nowPlayingCache != nil && time.Now().Before(nowPlayingExpiry) {
		np := nowPlayingCache
		nowPlayingMu.Unlock()
		return np
	}
	nowPlayingMu.Unlock()

	// Fetch outside the lock so a slow feed doesn't block other tabs'
	// reads of a still-valid cache. A cold-cache race may double-fetch
	// (two tabs at once) — harmless for a personal dashboard.
	np := fetchNowPlaying()
	if np == nil {
		return nil
	}

	nowPlayingMu.Lock()
	nowPlayingCache = np
	nowPlayingExpiry = time.Now().Add(nowPlayingCacheTTL)
	nowPlayingMu.Unlock()
	return np
}

// fetchNowPlaying pulls the feed and parses the on-air track. Bounded by
// its own background context (not a request context) so one client
// disconnecting mid-poll can't poison a shared cache fill.
func fetchNowPlaying() *nowPlaying {
	ctx, cancel := context.WithTimeout(context.Background(), nowPlayingTimeout)
	defer cancel()
	raw, err := nowPlayingFetchBytes(ctx)
	if err != nil {
		return nil
	}
	np, err := parseNowPlaying(raw)
	if err != nil {
		return nil
	}
	return np
}

// fetchSomaSongsLive is the real network read of the SomaFM feed.
func fetchSomaSongsLive(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, somaSongsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := nowPlayingClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, io.EOF // treated as "unavailable" upstream
	}
	// Cap the read — the feed is a few KB; this guards against a
	// misbehaving upstream streaming forever.
	return io.ReadAll(io.LimitReader(resp.Body, 256*1024))
}

// parseNowPlaying decodes the SomaFM songs XML and returns the first
// (on-air) track. Returns (nil, nil) when the feed has no usable song.
func parseNowPlaying(raw []byte) (*nowPlaying, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	// SomaFM serves ISO-8859-1; without a CharsetReader encoding/xml
	// errors out on the declared non-UTF-8 encoding. Map the latin-1
	// family explicitly and pass anything else through unchanged.
	dec.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
			return charmap.ISO8859_1.NewDecoder().Reader(in), nil
		case "windows-1252", "cp1252":
			return charmap.Windows1252.NewDecoder().Reader(in), nil
		default:
			return in, nil
		}
	}

	var doc somaSongsDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Songs) == 0 {
		return nil, nil
	}
	s := doc.Songs[0]
	title := strings.TrimSpace(s.Title)
	artist := strings.TrimSpace(s.Artist)
	if title == "" && artist == "" {
		return nil, nil
	}
	return &nowPlaying{
		Title:     title,
		Artist:    artist,
		Album:     strings.TrimSpace(s.Album),
		SearchURL: youtubeSearchURL(artist, title),
	}, nil
}

// youtubeSearchURL builds a "hear this exact track" link: a YouTube search
// for "Artist Title". YouTube is the most reliable place a free-text track
// query resolves to the actual song; swapping providers is a one-line edit
// here. Returns "" when there's nothing to search for.
func youtubeSearchURL(artist, title string) string {
	q := strings.TrimSpace(strings.TrimSpace(artist) + " " + strings.TrimSpace(title))
	if q == "" {
		return ""
	}
	return "https://www.youtube.com/results?search_query=" + url.QueryEscape(q)
}
