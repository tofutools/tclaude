package agentd

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Live page reloads: a socket per open page view (a Collab tab, an overlay, a
// separate window) that says the page's source asked for a reload, such as a
// saved file under a static target. It carries no page content and nothing
// the agent wrote; the view reloads its own frame. A page re-shown in place
// reloads through the same signal. When the page's origin closes (the page
// closed, or was re-shown with another kind of source and so a fresh origin)
// the socket says "gone" and ends; the view follows the snapshot from there.
//
// Each connection starts by asking the page's source for its start path once,
// and says whether anything answered: the view shows an unreachable state with
// Retry, which reconnects and so asks again. Only the outcome is sent.
//
// The view talks back on the same socket (tcl-2dwbad): {"type":"view",...}
// reports its viewport and scroll, which `page snapshot` renders at, and
// {"type":"dom-result",...} answers a {"type":"dom",...} read the daemon sent
// for `page dom` (pages_marks.go). Both carry page content and are checked
// there.

const pageLiveMaxSubscribers = 256

var pageLiveCount atomic.Int64

// GET /api/pages/{id}/live — messages are first {"type":"reachable"} or
// {"type":"unreachable"}, then {"type":"reload"} and {"type":"dom"}, and, last,
// {"type":"gone"}.
func handleDashboardPageLive(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	id := r.PathValue("id")
	livePages.mu.Lock()
	p := livePages.byID[id]
	var stage *stagePage
	start := "/"
	if p != nil {
		stage, start = p.stage, p.path
	}
	livePages.mu.Unlock()
	if stage == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such page")
		return
	}
	if pageLiveCount.Add(1) > pageLiveMaxSubscribers {
		pageLiveCount.Add(-1)
		writeError(w, http.StatusServiceUnavailable, "busy", "too many live page connections")
		return
	}
	defer pageLiveCount.Add(-1)
	reload, cancel := stage.subscribeReload()
	defer cancel()
	ws, err := upgradeTerminalWebSocket(w, r)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	ws.SetReadLimit(pageLiveReadLimit)
	view, unregister := registerPageView(id)
	defer unregister()

	// The browser sends pongs, view reports and DOM answers; reading also
	// notices it left.
	left := make(chan struct{})
	go func() {
		defer close(left)
		_ = ws.SetReadDeadline(time.Now().Add(drawingBoardLiveReadWait))
		ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(drawingBoardLiveReadWait)) })
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.TextMessage {
				pageViewMessage(id, view, data)
			}
		}
	}()

	send := func(kind string) bool {
		_ = ws.SetWriteDeadline(time.Now().Add(drawingBoardLiveWriteWait))
		return ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"`+kind+`"}`)) == nil
	}
	if !send(pageReachability(r.Context(), stage, start)) {
		return
	}
	ping := time.NewTicker(drawingBoardLivePingEvery)
	defer ping.Stop()
	for {
		select {
		case <-left:
			return
		case <-stage.done:
			_ = send("gone")
			return
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(drawingBoardLiveWriteWait)); err != nil {
				return
			}
		case <-reload:
			if !send("reload") {
				return
			}
		case msg := <-view.out:
			_ = ws.SetWriteDeadline(time.Now().Add(drawingBoardLiveWriteWait))
			if ws.WriteMessage(websocket.TextMessage, msg) != nil {
				return
			}
		}
	}
}

// pageReachability asks the page's source for its start path, as the frame
// would, and answers "reachable" when anything answered without a gateway
// error (a 404 from a dev server is the page's own answer).
func pageReachability(ctx context.Context, stage *stagePage, start string) string {
	up := stage.transport()
	if up == nil {
		return "unreachable"
	}
	if start == "" || start[0] != '/' {
		start = "/"
	}
	ctx, cancel := context.WithTimeout(ctx, pageProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://stage-upstream.invalid"+start, nil)
	if err != nil {
		return "unreachable"
	}
	res, err := up.RoundTrip(req)
	if err != nil {
		return "unreachable"
	}
	_, _ = io.CopyN(io.Discard, res.Body, 64<<10)
	_ = res.Body.Close()
	if res.StatusCode == http.StatusBadGateway || res.StatusCode == http.StatusServiceUnavailable || res.StatusCode == http.StatusGatewayTimeout {
		return "unreachable"
	}
	return "reachable"
}
