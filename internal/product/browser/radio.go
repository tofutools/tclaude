package browser

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
)

type radioTrack struct {
	Artist    string `json:"artist"`
	Title     string `json:"title"`
	Album     string `json:"album"`
	StartedAt string `json:"started_at"`
}
type radioCacheEntry struct {
	track radioTrack
	until time.Time
}
type radioMetadata struct {
	mu     sync.Mutex
	cache  map[string]radioCacheEntry
	client *http.Client
}

func (s *Server) radioNowPlaying(w http.ResponseWriter, r *http.Request) {
	if !s.authenticated(r) {
		http.Error(w, "browser login required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	channel := r.URL.Query().Get("channel")
	known := false
	for _, c := range model.RadioChannels() {
		if c.ID == channel {
			known = true
			break
		}
	}
	if !known {
		http.Error(w, "unknown station", http.StatusBadRequest)
		return
	}
	// One bounded upstream read at a time; the cache belongs to this browser instance.
	s.radio.mu.Lock()
	defer s.radio.mu.Unlock()
	if entry, ok := s.radio.cache[channel]; ok && time.Now().Before(entry.until) {
		writeRadioTrack(w, entry.track)
		return
	}
	client := s.radio.client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "https://somafm.com/songs/"+channel+".xml", nil)
	if err != nil {
		http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	decoder := xml.NewDecoder(io.LimitReader(response.Body, 128<<10))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "iso-8859-1") {
			data, err := io.ReadAll(input)
			if err != nil {
				return nil, err
			}
			var converted strings.Builder
			for _, b := range data {
				converted.WriteRune(rune(b))
			}
			return strings.NewReader(converted.String()), nil
		}
		return nil, errors.New("unsupported metadata encoding")
	}
	var feed struct {
		Songs []struct {
			Artist string `xml:"artist"`
			Title  string `xml:"title"`
			Album  string `xml:"album"`
			Date   string `xml:"date"`
		} `xml:"song"`
	}
	if err := decoder.Decode(&feed); err != nil || len(feed.Songs) == 0 {
		http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
		return
	}
	song := feed.Songs[0]
	track := radioTrack{Artist: song.Artist, Title: song.Title, Album: song.Album, StartedAt: song.Date}
	if s.radio.cache == nil {
		s.radio.cache = make(map[string]radioCacheEntry)
	}
	s.radio.cache[channel] = radioCacheEntry{track: track, until: time.Now().Add(12 * time.Second)}
	writeRadioTrack(w, track)
}
func writeRadioTrack(w http.ResponseWriter, track radioTrack) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(track)
}
