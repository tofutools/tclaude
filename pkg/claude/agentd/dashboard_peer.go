package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func registerDashboardPeerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/peer/{node}/{tail...}", handleDashboardPeer)
	mux.HandleFunc("POST /api/peer/{node}/{tail...}", handleDashboardPeer)
}

func handleDashboardPeer(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	servePeerViewProxyWith(w, r, true)
}

// Both local operator surfaces use the same pinned peer transport.
func servePeerViewProxy(w http.ResponseWriter, r *http.Request) {
	servePeerViewProxyWith(w, r, false)
}

// cached lets the dashboard's declared polls read through peerReads
// (peer_read_cache.go); every other request is one exchange, as before.
func servePeerViewProxyWith(w http.ResponseWriter, r *http.Request, cached bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
	// The browser selects a pinned identity; labels and request headers never
	// choose the transport's source identity or substitute a different target.
	peer, err := db.GetFederationPeer(r.PathValue("node"))
	if err != nil {
		writeError(w, 503, "peer_unavailable", "could not read peer trust")
		return
	}
	if peer == nil {
		writeError(w, 403, "not_trusted", "linked trusted peer required")
		return
	}
	rt := currentFederation()
	var lastSeen time.Time
	if rt != nil {
		for _, entry := range rt.cl.Directory() {
			if entry.InstanceID == peer.InstanceID {
				lastSeen = entry.LastSeen
				break
			}
		}
	}
	fail := func(err error) {
		if errors.Is(err, errPeerViewBusy) {
			writeError(w, 503, "peer_busy", "peer view concurrency limit reached")
			return
		}
		status, reason := http.StatusBadGateway, "peer_offline"
		if errors.Is(err, errPeerViewTimeout) {
			status, reason = http.StatusGatewayTimeout, "peer_timeout"
		}
		out := map[string]any{"error": "peer is unreachable", "code": "peer_unreachable", "reason": reason}
		if !lastSeen.IsZero() {
			out["last_seen"] = lastSeen
		}
		writeJSON(w, status, out)
	}
	if rt == nil {
		fail(errPeerViewOffline)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, fedPeerViewBodyLimit+1))
	if err != nil {
		writeError(w, 400, "invalid_arg", "could not read request body")
		return
	}
	if len(body) > fedPeerViewBodyLimit {
		writeError(w, 413, "request_too_large", "peer request body exceeds limit")
		return
	}
	body, err = prepareHarnessCredentialPush(r, body)
	if err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return
	}
	auditNodeRunProxyRequest(r, peer.InstanceID, body)
	auditPeerAccessProxy(r, peer.InstanceID, "requested", 202, nil)
	auditPeerActionProxy(r, peer.InstanceID, "requested", 202)
	target := url.URL{Path: "/api/" + r.PathValue("tail"), RawQuery: r.URL.RawQuery}
	if len(target.RequestURI()) > 4096 {
		writeError(w, 414, "request_too_large", "peer request URI exceeds limit")
		return
	}
	req := fedPeerViewRequest{Method: r.Method, URI: target.RequestURI(), Header: peerViewRequestHeaders(r.Header), Body: body}
	var reply fedPeerViewReply
	poll := peerReadPollInterval(r, body)
	cachedRead := cached && poll > 0
	if cachedRead {
		reply, err = peerReads.read(r.Context(), rt, peer, req, poll)
	} else {
		reply, err = exchangePeerView(r.Context(), rt, peer, req)
	}
	if err != nil {
		fail(err)
		return
	}
	if !validPeerViewReply(reply) {
		writeError(w, 502, "peer_invalid_response", "peer returned an invalid response")
		return
	}
	auditNodeRunProxy(r, peer.InstanceID, body, reply.Status, reply.Body)
	auditPeerAccessProxy(r, peer.InstanceID, "result", reply.Status, reply.Body)
	auditPeerActionProxy(r, peer.InstanceID, "result", reply.Status)
	// Recheck local trust before returning data from a stream opened earlier.
	current, err := db.GetFederationPeer(peer.InstanceID)
	if err != nil || current == nil {
		writeError(w, 403, "not_trusted", "peer is no longer trusted")
		return
	}
	for key, values := range peerViewReplyHeaders(reply.Header) {
		w.Header()[key] = values
	}
	// Only a copy this daemon cached says how old it is; a peer's own Age
	// header never passes.
	if age := reply.Header.Get("Age"); cachedRead && age != "" {
		w.Header().Set("Age", age)
	}
	// A restricted or compromised peer must never serve executable content on
	// the local operator's authenticated origin, even on direct navigation.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(reply.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(reply.Body)
	}
}

var errPeerViewTimeout = errors.New("peer view timed out")

// exchangePeerView runs one request/reply with peer over a fresh relayed
// stream. Timeouts come back as errPeerViewTimeout, busy as errPeerViewBusy.
func exchangePeerView(parent context.Context, rt *fedRuntime, peer *db.FederationPeer, req fedPeerViewRequest) (fedPeerViewReply, error) {
	ctx, cancel := context.WithTimeout(parent, fedPeerViewTimeout)
	defer cancel()
	stopRuntime := context.AfterFunc(rt.ctx, cancel)
	defer stopRuntime()
	classify := func(err error) error {
		if errors.Is(err, errPeerViewBusy) {
			return err
		}
		deadline, hasDeadline := ctx.Deadline()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || (hasDeadline && !time.Now().Before(deadline)) {
			return fmt.Errorf("%w: %v", errPeerViewTimeout, err)
		}
		return err
	}
	conn, err := rt.openPeerView(ctx, peer)
	if err != nil {
		return fedPeerViewReply{}, classify(err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	if err := writePeerViewFrame(conn, fedPeerViewRequestLimit, req); err != nil {
		return fedPeerViewReply{}, classify(err)
	}
	var reply fedPeerViewReply
	if err := readPeerViewFrame(conn, fedPeerViewResponseLimit, &reply); err != nil {
		return fedPeerViewReply{}, classify(err)
	}
	return reply, nil
}

func validPeerViewReply(reply fedPeerViewReply) bool {
	return reply.Status >= 200 && reply.Status <= 599 && len(reply.Body) <= fedPeerViewResponseBodyLimit &&
		(len(reply.Body) == 0 || (json.Valid(reply.Body) && reply.Status != http.StatusNoContent && reply.Status != http.StatusNotModified))
}
