package agentd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Board live updates: a socket per open editor that says a board changed, so
// the editor reads with GET ?since= right away instead of polling for it. It
// carries no board content. Edits stay on PATCH and reads on GET, so the merge
// has one path, and an editor that cannot hold a socket simply keeps polling.

const (
	boardLiveMaxSubscribers = 256
	boardLivePingEvery      = 30 * time.Second
	boardLiveReadWait       = 2 * boardLivePingEvery
	boardLiveWriteWait      = 10 * time.Second
	boardClientHeader       = "X-Board-Client"
)

// boardLiveSub is one editor's mailbox. Signals coalesce: pending says one is
// due, and origin names the client behind it, or is empty when it is unknown
// or there was more than one.
type boardLiveSub struct {
	wake    chan struct{}
	pending bool
	origin  string
}

type boardLiveHub struct {
	sync.Mutex
	boards map[string]map[*boardLiveSub]struct{}
	count  int
}

var boardLive = &boardLiveHub{boards: map[string]map[*boardLiveSub]struct{}{}}

func (h *boardLiveHub) subscribe(id string) *boardLiveSub {
	h.Lock()
	defer h.Unlock()
	if h.count >= boardLiveMaxSubscribers {
		return nil
	}
	sub := &boardLiveSub{wake: make(chan struct{}, 1)}
	if h.boards[id] == nil {
		h.boards[id] = map[*boardLiveSub]struct{}{}
	}
	h.boards[id][sub] = struct{}{}
	h.count++
	return sub
}

func (h *boardLiveHub) unsubscribe(id string, sub *boardLiveSub) {
	h.Lock()
	defer h.Unlock()
	delete(h.boards[id], sub)
	if len(h.boards[id]) == 0 {
		delete(h.boards, id)
	}
	h.count--
}

// publish tells the board's editors it changed. origin is the editor that
// made the change, which has nothing to fetch; empty when there is none.
func (h *boardLiveHub) publish(id, origin string) {
	h.Lock()
	defer h.Unlock()
	for sub := range h.boards[id] {
		switch {
		case !sub.pending:
			sub.pending, sub.origin = true, origin
		case sub.origin != origin:
			sub.origin = ""
		}
		select {
		case sub.wake <- struct{}{}:
		default:
		}
	}
}

func (h *boardLiveHub) take(sub *boardLiveSub) (origin string, due bool) {
	h.Lock()
	defer h.Unlock()
	origin, due = sub.origin, sub.pending
	sub.pending, sub.origin = false, ""
	return origin, due
}

// boardClient reads the editor token a dashboard edit names, the same kind of
// token POST /view carries. A malformed one only costs that editor an echo.
func boardClient(r *http.Request) string {
	client := r.Header.Get(boardClientHeader)
	if len(client) > 64 || !boardPrintable(client, false) {
		return ""
	}
	return client
}

// GET /api/boards/{id}/live — see the top of this file. Each message is
// {"type":"changed","client":"<origin>"}, with client omitted when unknown.
func handleDashboardBoardLive(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := boardPathID(w, r)
	if !ok {
		return
	}
	if _, err := db.GetCollabBoard(id); err != nil {
		writeBoardStoreError(w, err)
		return
	}
	sub := boardLive.subscribe(id)
	if sub == nil {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many live board connections")
		return
	}
	defer boardLive.unsubscribe(id, sub)
	ws, err := upgradeTerminalWebSocket(w, r)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	ws.SetReadLimit(512)

	// Nothing is expected from the browser but pongs, and reading is what
	// notices it has gone.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		_ = ws.SetReadDeadline(time.Now().Add(boardLiveReadWait))
		ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(boardLiveReadWait)) })
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(boardLivePingEvery)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(boardLiveWriteWait)); err != nil {
				return
			}
		case <-sub.wake:
			origin, due := boardLive.take(sub)
			if !due {
				continue
			}
			message, err := json.Marshal(struct {
				Type   string `json:"type"`
				Client string `json:"client,omitempty"`
			}{"changed", origin})
			if err != nil {
				return
			}
			_ = ws.SetWriteDeadline(time.Now().Add(boardLiveWriteWait))
			if err := ws.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}
}
