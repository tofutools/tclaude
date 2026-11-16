package agentd

// Terminal diagnostics, the server half (tk_f7e6804f). A browser terminal
// with its Diagnostics overlay on sends {"type":"diag","id":"<id>"} on its
// socket; the bridge consumes that frame (it is never typed into the session)
// and from then on logs, for that socket, when each output frame went out, how
// big it was and how long the write took (a long write is the network pushing
// back), and when input arrived. The overlay reads the log from
// GET /api/terminal-diag?id=<id>&since=<seq>, so nothing is added to the
// terminal stream itself. {"type":"diag","id":""} stops it. With no overlay
// on, the pump pays one atomic load per frame.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// termDiagCap bounds one socket's log: the overlay polls every second,
	// far more often than this many frames go by.
	termDiagCap = 2048
	// termDiagIdle drops a log nobody has polled for this long (a tab that
	// went away with its overlay on).
	termDiagIdle = time.Minute
)

var termDiagIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// termDiagEvent is one logged frame: "out" a frame sent to the browser,
// "in" input that arrived from it.
type termDiagEvent struct {
	Seq     int64   `json:"seq"`
	Kind    string  `json:"kind"`
	AtMS    float64 `json:"at_ms"`
	Bytes   int     `json:"bytes"`
	WriteMS float64 `json:"write_ms,omitempty"`
}

type termDiagLog struct {
	mu     sync.Mutex
	events []termDiagEvent
	seq    int64
	polled time.Time
}

func (l *termDiagLog) add(kind string, at time.Time, bytes int, write time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ev := termDiagEvent{Seq: l.seq, Kind: kind, AtMS: float64(at.UnixMicro()) / 1000, Bytes: bytes}
	if kind == "out" {
		ev.WriteMS = float64(write.Microseconds()) / 1000
	}
	if len(l.events) >= termDiagCap {
		l.events = append(l.events[:0], l.events[len(l.events)-termDiagCap/2:]...)
	}
	l.events = append(l.events, ev)
}

func (l *termDiagLog) since(seq int64) []termDiagEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.polled = time.Now()
	out := []termDiagEvent{}
	for _, ev := range l.events {
		if ev.Seq > seq {
			out = append(out, ev)
		}
	}
	return out
}

var termDiagLogs = struct {
	sync.Mutex
	byID map[string]*termDiagLog
}{byID: map[string]*termDiagLog{}}

// termDiagOpen starts (or restarts) the log for id, dropping logs left idle.
func termDiagOpen(id string) *termDiagLog {
	termDiagLogs.Lock()
	defer termDiagLogs.Unlock()
	for other, log := range termDiagLogs.byID {
		log.mu.Lock()
		idle := time.Since(log.polled) > termDiagIdle
		log.mu.Unlock()
		if idle {
			delete(termDiagLogs.byID, other)
		}
	}
	log := &termDiagLog{polled: time.Now()}
	termDiagLogs.byID[id] = log
	return log
}

func termDiagClose(id string) {
	termDiagLogs.Lock()
	defer termDiagLogs.Unlock()
	delete(termDiagLogs.byID, id)
}

func termDiagLookup(id string) *termDiagLog {
	termDiagLogs.Lock()
	defer termDiagLogs.Unlock()
	return termDiagLogs.byID[id]
}

// parseTermDiag classifies one WebSocket frame: isDiag for any
// {"type":"diag",...} text frame, which the bridge consumes; id is the log
// to start, or "" to stop (an invalid id stops too).
func parseTermDiag(messageType int, data []byte) (id string, isDiag bool) {
	if messageType != websocket.TextMessage || !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return "", false
	}
	var msg struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.Type != "diag" {
		return "", false
	}
	if !termDiagIDPattern.MatchString(msg.ID) {
		return "", true
	}
	return msg.ID, true
}

// termDiagSocket is one socket's diagnostics state: the log while the
// overlay is on, nil otherwise.
type termDiagSocket struct {
	log atomic.Pointer[termDiagLog]
	id  atomic.Value // string
}

// apply handles a diag frame: start a log under id, or stop.
func (s *termDiagSocket) apply(id string) {
	if old, _ := s.id.Load().(string); old != "" && old != id {
		termDiagClose(old)
	}
	s.id.Store(id)
	if id == "" {
		s.log.Store(nil)
		return
	}
	s.log.Store(termDiagOpen(id))
}

func (s *termDiagSocket) close() {
	s.apply("")
}

// send wraps one output frame's write, logging it while the overlay is on.
func (s *termDiagSocket) send(frame []byte, write func([]byte) error) error {
	log := s.log.Load()
	if log == nil {
		return write(frame)
	}
	start := time.Now()
	err := write(frame)
	log.add("out", start, len(frame), time.Since(start))
	return err
}

// input logs input from the browser while the overlay is on.
func (s *termDiagSocket) input(bytes int) {
	if log := s.log.Load(); log != nil {
		log.add("in", time.Now(), bytes, 0)
	}
}

// handleTerminalDiag serves a socket's log since a sequence number, with the
// server's clock, so the overlay can line up gaps without syncing clocks.
func handleTerminalDiag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("id")
	if !termDiagIDPattern.MatchString(id) {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	log := termDiagLookup(id)
	if log == nil {
		http.Error(w, "no diagnostics for that terminal", http.StatusNotFound)
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"now_ms": float64(time.Now().UnixMicro()) / 1000,
		"events": log.since(since),
	})
}
