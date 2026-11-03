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

const pageLiveMaxSubscribers = 256

var pageLiveCount atomic.Int64

// GET /api/pages/{id}/live — messages are first {"type":"reachable"} or
// {"type":"unreachable"}, then {"type":"reload"}, and, last, {"type":"gone"}.
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
	ws.SetReadLimit(512)

	// Nothing is expected from the browser but pongs; reading notices it left.
	left := make(chan struct{})
	go func() {
		defer close(left)
		_ = ws.SetReadDeadline(time.Now().Add(boardLiveReadWait))
		ws.SetPongHandler(func(string) error { return ws.SetReadDeadline(time.Now().Add(boardLiveReadWait)) })
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}()

	send := func(kind string) bool {
		_ = ws.SetWriteDeadline(time.Now().Add(boardLiveWriteWait))
		return ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"`+kind+`"}`)) == nil
	}
	if !send(pageReachability(r.Context(), stage, start)) {
		return
	}
	ping := time.NewTicker(boardLivePingEvery)
	defer ping.Stop()
	for {
		select {
		case <-left:
			return
		case <-stage.done:
			_ = send("gone")
			return
		case <-ping.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(boardLiveWriteWait)); err != nil {
				return
			}
		case <-reload:
			if !send("reload") {
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
