package agentd

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// sseHeartbeatInterval is how often handleDashboardEvents emits a
// comment-line keepalive on an otherwise-idle stream. The browser's
// EventSource doesn't require it for normal flow, but some proxies
// (and a paranoid browser tab-throttler) close a connection that has
// gone fully silent for too long. An idle dashboard with no agent
// activity can sit silent for many minutes; 25s puts us well inside
// the typical 60s idle-close window.
//
// Var, not const, so tests can shrink it.
var sseHeartbeatInterval = 25 * time.Second

// handleDashboardEvents serves the dashboard's Server-Sent Events
// stream. Each connected dashboard tab opens one of these from
// new EventSource('/api/events'); on every fired event the client
// re-fetches /api/snapshot. See events.go for the broadcaster and
// docs/plans/.../dashboard-realtime-push.md for the design.
//
// Auth identical to /api/snapshot — cookie + Origin/Referer pin via
// checkDashboardAuth. Mounted on the loopback popup mux by
// registerDashboardRoutes alongside /api/snapshot, so it shares the
// same one-stable-URL contract.
func handleDashboardEvents(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Belt-and-braces: if a reverse proxy ever sits in front, opt out
	// of its response buffering so events reach the browser as they
	// land. tclaude runs on loopback today, but this is the standard
	// SSE hardening and costs nothing.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// One initial comment so EventSource.onopen fires immediately —
	// the client knows the stream is live without waiting for the
	// first real event (which on a quiet daemon could be seconds
	// away). Comments start with ':' per the SSE spec; clients
	// ignore them.
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	sub, cancel := dashboardEvents.Subscribe()
	defer cancel()

	heartbeat := time.NewTicker(sseHeartbeatInterval)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub:
			// Payload is intentionally minimal: clients always
			// react by re-fetching /api/snapshot, so the event
			// only needs to say "a change landed." The timestamp
			// makes the stream easy to eyeball in `curl -N`.
			ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
			if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", ts); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
