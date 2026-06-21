package agentd

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
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
//   - The channel id flows in from the dashboard as a query param, so it
//     must be validated against a fixed allowlist (config.SlopChannels)
//     before it ever reaches a URL — see the SSRF note on resolveSomaChannel.
//
// This is the ONLY outbound HTTP agentd makes. It is best-effort and
// fail-soft: any error returns an empty payload and the UI simply hides
// the song line (the music itself is unaffected — it plays from the
// browser). The browser polls this every ~30s while music plays; a short
// per-channel server-side cache keeps multiple dashboard tabs (and tabs on
// different channels) from each hitting SomaFM.

// The channel allowlist is the SECURITY boundary for this proxy, not just
// config. The channel id arrives from the browser as a ?channel= query
// param and is the only caller-influenced part of the one outbound request
// agentd makes. resolveSomaChannel rejects anything outside the allowlist
// (falling back to the default), so a crafted ?channel= can never steer the
// fetch off somafm.com/songs/<known-id>.xml — it is not a fetch-anything
// SSRF. The allowlist itself (config.SlopChannels) is the single source of
// truth shared with config validation and the browser's channel catalog.

// resolveSomaChannel maps an incoming ?channel= value onto a valid channel
// id, falling back to the default for empty/unknown input. This is the
// gate that keeps the fetch URL inside the allowlist.
func resolveSomaChannel(id string) string {
	id = strings.TrimSpace(id)
	if config.IsKnownSlopChannel(id) {
		return id
	}
	return config.DefaultSlopChannel
}

// somaSongsURL builds SomaFM's recent-songs feed URL for a channel — a
// small XML document whose FIRST <song> is the track currently on air.
// Callers MUST pass an allowlisted id (via resolveSomaChannel).
func somaSongsURL(channel string) string {
	return "https://somafm.com/songs/" + channel + ".xml"
}

const (
	nowPlayingTimeout  = 5 * time.Second
	nowPlayingCacheTTL = 12 * time.Second
)

// nowPlaying is the wire shape the dashboard renders. Empty fields render
// as "no song line"; SearchURL is a prebuilt "go hear this exact track"
// link the title links to. StartedAt is the unix second the track went on
// air (from the feed) — the dashboard counts elapsed "time on air" up from
// it. It is 0 when the feed omits/garbles the timestamp, which the UI
// reads as "no elapsed counter".
//
// Note there is no song *duration* anywhere in SomaFM's metadata, so there
// is no honest percent-complete to send — hence an elapsed counter, not a
// 0→length progress bar (a live stream has no per-song length anyway).
type nowPlaying struct {
	Title     string `json:"title"`
	Artist    string `json:"artist"`
	Album     string `json:"album"`
	SearchURL string `json:"search_url"`
	StartedAt int64  `json:"started_at"`
}

// somaSongsDoc is the subset of SomaFM's songs XML we parse. encoding/xml
// unwraps the CDATA in each field for us. <date> is the unix second the
// track started.
type somaSongsDoc struct {
	Songs []struct {
		Title  string `xml:"title"`
		Artist string `xml:"artist"`
		Album  string `xml:"album"`
		Date   string `xml:"date"`
	} `xml:"song"`
}

// cachedNowPlaying is one channel's last-fetched track plus its freshness
// deadline. Held per channel in nowPlayingCache so a tab on Groove Salad
// doesn't serve another tab's Illinois Street track.
type cachedNowPlaying struct {
	np     *nowPlaying
	expiry time.Time
}

var (
	nowPlayingMu    sync.Mutex
	nowPlayingCache = map[string]cachedNowPlaying{}

	// nowPlayingFetchBytes is the network boundary — swapped in tests so
	// they never hit SomaFM. Production reads the live feed for the given
	// (already-validated) channel.
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
	channel := resolveSomaChannel(r.URL.Query().Get("channel"))
	if np := nowPlayingCached(channel); np != nil {
		writeJSON(w, http.StatusOK, np)
		return
	}
	// Fail-soft: empty object, 200. The UI hides the song line on empty
	// rather than showing an error — a metadata blip shouldn't look like
	// the radio broke.
	writeJSON(w, http.StatusOK, map[string]any{})
}

// nowPlayingCached returns the cached track for a channel when fresh, else
// fetches once and refills that channel's slot. Returns nil when the feed
// is unreachable or empty. channel must already be allowlisted.
func nowPlayingCached(channel string) *nowPlaying {
	nowPlayingMu.Lock()
	if c, ok := nowPlayingCache[channel]; ok && time.Now().Before(c.expiry) {
		np := c.np
		nowPlayingMu.Unlock()
		return np
	}
	nowPlayingMu.Unlock()

	// Fetch outside the lock so a slow feed doesn't block other tabs'
	// reads of a still-valid cache. A cold-cache race may double-fetch
	// (two tabs at once) — harmless for a personal dashboard.
	np := fetchNowPlaying(channel)
	if np == nil {
		return nil
	}

	nowPlayingMu.Lock()
	nowPlayingCache[channel] = cachedNowPlaying{np: np, expiry: time.Now().Add(nowPlayingCacheTTL)}
	nowPlayingMu.Unlock()
	return np
}

// fetchNowPlaying pulls a channel's feed and parses the on-air track.
// Bounded by its own background context (not a request context) so one
// client disconnecting mid-poll can't poison a shared cache fill.
func fetchNowPlaying(channel string) *nowPlaying {
	ctx, cancel := context.WithTimeout(context.Background(), nowPlayingTimeout)
	defer cancel()
	raw, err := nowPlayingFetchBytes(ctx, channel)
	if err != nil {
		return nil
	}
	np, err := parseNowPlaying(raw)
	if err != nil {
		return nil
	}
	return np
}

// fetchSomaSongsLive is the real network read of a channel's SomaFM feed.
// channel must already be allowlisted (the URL is built from it directly).
func fetchSomaSongsLive(ctx context.Context, channel string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, somaSongsURL(channel), nil)
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
	// A bad/absent <date> just means "no elapsed counter" — never fail the
	// whole track over it.
	var startedAt int64
	if n, err := strconv.ParseInt(strings.TrimSpace(s.Date), 10, 64); err == nil && n > 0 {
		startedAt = n
	}
	return &nowPlaying{
		Title:     title,
		Artist:    artist,
		Album:     strings.TrimSpace(s.Album),
		SearchURL: youtubeSearchURL(artist, title),
		StartedAt: startedAt,
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
