package agentd

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Drawing board live updates: a socket per open editor that says a drawing board changed, so
// the editor reads with GET ?since= right away instead of polling for it. It
// carries no drawing board content. Edits stay on PATCH and reads on GET, so the merge
// has one path, and an editor that cannot hold a socket simply keeps polling.

const (
	drawingBoardLiveMaxSubscribers = 256
	drawingBoardLivePingEvery      = 30 * time.Second
	drawingBoardLiveReadWait       = 2 * drawingBoardLivePingEvery
	drawingBoardLiveWriteWait      = 10 * time.Second
	drawingBoardClientHeader       = "X-Drawing-board-Client"
)

// drawingBoardLiveSub is one editor's mailbox. Signals coalesce: pending says one is
// due, and origin names the client behind it, or is empty when it is unknown
// or there was more than one.
type drawingBoardLiveSub struct {
	wake    chan struct{}
	pending bool
	origin  string
}

type drawingBoardLiveHub struct {
	sync.Mutex
	drawingBoards map[string]map[*drawingBoardLiveSub]struct{}
	count         int
}

var drawingBoardLive = &drawingBoardLiveHub{drawingBoards: map[string]map[*drawingBoardLiveSub]struct{}{}}

func (h *drawingBoardLiveHub) subscribe(id string) *drawingBoardLiveSub {
	h.Lock()
	defer h.Unlock()
	if h.count >= drawingBoardLiveMaxSubscribers {
		return nil
	}
	sub := &drawingBoardLiveSub{wake: make(chan struct{}, 1)}
	if h.drawingBoards[id] == nil {
		h.drawingBoards[id] = map[*drawingBoardLiveSub]struct{}{}
	}
	h.drawingBoards[id][sub] = struct{}{}
	h.count++
	return sub
}

func (h *drawingBoardLiveHub) unsubscribe(id string, sub *drawingBoardLiveSub) {
	h.Lock()
	defer h.Unlock()
	delete(h.drawingBoards[id], sub)
	if len(h.drawingBoards[id]) == 0 {
		delete(h.drawingBoards, id)
	}
	h.count--
}

// publish tells the drawing board's editors it changed. origin is the editor that
// made the change, which has nothing to fetch; empty when there is none.
func (h *drawingBoardLiveHub) publish(id, origin string) {
	h.Lock()
	defer h.Unlock()
	for sub := range h.drawingBoards[id] {
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

func (h *drawingBoardLiveHub) take(sub *drawingBoardLiveSub) (origin string, due bool) {
	h.Lock()
	defer h.Unlock()
	origin, due = sub.origin, sub.pending
	sub.pending, sub.origin = false, ""
	return origin, due
}

// drawingBoardClient reads the editor token a dashboard edit names, the same kind of
// token POST /view carries. A malformed one only costs that editor an echo.
func drawingBoardClient(r *http.Request) string {
	client := r.Header.Get(drawingBoardClientHeader)
	if len(client) > 64 || !drawingBoardPrintable(client, false) {
		return ""
	}
	return client
}

// GET /api/drawing-boards/{id}/live — see the top of this file. Each message is
// {"type":"changed","client":"<origin>"}, with client omitted when unknown.
func handleDashboardDrawingBoardLive(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id, ok := drawingBoardPathID(w, r)
	if !ok {
		return
	}
	if _, err := db.GetCollabDrawingBoard(id); err != nil {
		writeDrawingBoardStoreError(w, err)
		return
	}
	sub := drawingBoardLive.subscribe(id)
	if sub == nil {
		writeError(w, http.StatusServiceUnavailable, "busy", "too many live drawing board connections")
		return
	}
	defer drawingBoardLive.unsubscribe(id, sub)
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
		_ = ws.SetReadDeadline(time.Now().Add(drawingBoardLiveReadWait))
		ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(drawingBoardLiveReadWait)) })
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ping := time.NewTicker(drawingBoardLivePingEvery)
	defer ping.Stop()
	for {
		select {
		case <-gone:
			return
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(drawingBoardLiveWriteWait)); err != nil {
				return
			}
		case <-sub.wake:
			origin, due := drawingBoardLive.take(sub)
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
			_ = ws.SetWriteDeadline(time.Now().Add(drawingBoardLiveWriteWait))
			if err := ws.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}
		}
	}
}
