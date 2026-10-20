package agentd

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

const (
	fedPeerViewTimeout = 15 * time.Second
	// fedPeerViewRequestLimit fits the largest request body, an upload chunk
	// (peerViewBodyLimit), base64-encoded in its JSON frame.
	fedPeerViewRequestLimit      = 768 << 10
	fedPeerViewBodyLimit         = 32 << 10
	fedPeerViewResponseLimit     = 12 << 20
	fedPeerViewResponseBodyLimit = 8 << 20
	// The active limits stay within the hub's default per-instance stream
	// cap (tclaude-hub serve --max-streams).
	fedPeerViewActiveLimit  = 64
	fedPeerViewPerPeerLimit = 16
	// fedPeerViewQueueWait bounds how long an outbound request waits for a
	// stream slot, here or on the peer, before it fails busy. A dashboard
	// dialog opens with a burst of reads larger than the per-peer limit; they
	// queue instead of failing the ones that lose the race.
	fedPeerViewQueueWait = 5 * time.Second
	// fedPeerViewBusyBackoff is the first pause before asking a peer that
	// answered busy again; it doubles up to fedPeerViewBusyBackoffMax.
	fedPeerViewBusyBackoff    = 50 * time.Millisecond
	fedPeerViewBusyBackoffMax = 800 * time.Millisecond
)

var (
	errPeerViewOffline = errors.New("peer is offline")
	errPeerViewBusy    = errors.New("peer view is busy")
)

type fedPeerViewRequest struct {
	Method string      `json:"method"`
	URI    string      `json:"uri"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body,omitempty"`
}

type fedPeerViewReply struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body,omitempty"`
}

type fedPeerViewWaiter struct {
	peer, request string
	answer        chan proto.PeerViewAnswerPayload
}

type fedPeerViewState struct {
	active  map[string]string
	waiters map[string]fedPeerViewWaiter
	// freed is closed (and replaced) whenever a slot is released, waking
	// outbound requests queued for one.
	freed chan struct{}
}

func (rt *fedRuntime) peerViewsLocked() *fedPeerViewState {
	if rt.peerViews == nil {
		rt.peerViews = &fedPeerViewState{active: map[string]string{}, waiters: map[string]fedPeerViewWaiter{}, freed: make(chan struct{})}
	}
	return rt.peerViews
}

// Admission is bounded before spawning a worker or joining a stream.
func (rt *fedRuntime) reservePeerView(key, peer string) bool {
	rt.peerViewsMu.Lock()
	defer rt.peerViewsMu.Unlock()
	state := rt.peerViewsLocked()
	if _, exists := state.active[key]; exists || len(state.active) >= fedPeerViewActiveLimit {
		return false
	}
	n := 0
	for _, p := range state.active {
		if p == peer {
			n++
		}
	}
	if n >= fedPeerViewPerPeerLimit {
		return false
	}
	state.active[key] = peer
	return true
}

func (rt *fedRuntime) releasePeerView(key string) {
	rt.peerViewsMu.Lock()
	state := rt.peerViewsLocked()
	delete(state.active, key)
	close(state.freed)
	state.freed = make(chan struct{})
	rt.peerViewsMu.Unlock()
}

// reservePeerViewWait is reservePeerView that waits for a slot until ctx ends
// or the deadline passes.
func (rt *fedRuntime) reservePeerViewWait(ctx context.Context, key, peer string, deadline time.Time) bool {
	for {
		rt.peerViewsMu.Lock()
		freed := rt.peerViewsLocked().freed
		rt.peerViewsMu.Unlock()
		if rt.reservePeerView(key, peer) {
			return true
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return false
		}
		timer := time.NewTimer(wait)
		select {
		case <-freed:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false
		}
	}
}

func (rt *fedRuntime) handlePeerViewAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var a proto.PeerViewAnswerPayload
	if env.DecodePayload(&a) != nil || !proto.ValidStreamID(a.Stream) || (a.OK && len(a.Key) != 32) {
		return
	}
	rt.peerViewsMu.Lock()
	waiter, ok := rt.peerViewsLocked().waiters[a.Stream]
	rt.peerViewsMu.Unlock()
	if !ok || waiter.peer != peer.InstanceID || waiter.request != env.InReplyTo {
		return
	}
	select {
	case waiter.answer <- a:
	default:
	}
}

type fedPeerViewConn struct {
	*stream.Conn
	release func()
}

func (c *fedPeerViewConn) Close() error { c.release(); return c.Conn.Close() }

// openPeerView opens a stream to peer. A request that finds no free slot,
// here or on the peer, waits and retries for up to fedPeerViewQueueWait
// before it fails busy.
func (rt *fedRuntime) openPeerView(ctx context.Context, peer *db.FederationPeer) (*fedPeerViewConn, error) {
	deadline := time.Now().Add(fedPeerViewQueueWait)
	backoff := fedPeerViewBusyBackoff
	for {
		conn, err := rt.openPeerViewOnce(ctx, peer, deadline)
		if !errors.Is(err, errPeerViewBusy) || !time.Now().Add(backoff).Before(deadline) {
			return conn, err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
		backoff = min(backoff*2, fedPeerViewBusyBackoffMax)
	}
}

func (rt *fedRuntime) openPeerViewOnce(ctx context.Context, peer *db.FederationPeer, deadline time.Time) (*fedPeerViewConn, error) {
	sid := proto.NewEnvelopeID()
	if !rt.reservePeerViewWait(ctx, "out:"+sid, peer.InstanceID, deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errPeerViewBusy
	}
	// The caller releases admission when the returned stream is closed.
	success := false
	defer func() {
		if !success {
			rt.releasePeerView("out:" + sid)
		}
	}()
	kp, err := stream.NewKeyPair()
	if err != nil {
		return nil, err
	}
	env, err := proto.NewEnvelope(rt.id, proto.KindPeerViewOpen, proto.Endpoint{Name: rt.name}, proto.Endpoint{Instance: peer.InstanceID}, fedPeerViewTimeout+time.Minute, proto.PeerViewOpenPayload{Stream: sid, Key: kp.Pub})
	if err != nil {
		return nil, err
	}
	ch := make(chan proto.PeerViewAnswerPayload, 1)
	rt.peerViewsMu.Lock()
	rt.peerViewsLocked().waiters[sid] = fedPeerViewWaiter{peer.InstanceID, env.ID, ch}
	rt.peerViewsMu.Unlock()
	defer func() { rt.peerViewsMu.Lock(); delete(rt.peerViewsLocked().waiters, sid); rt.peerViewsMu.Unlock() }()
	sealed, err := proto.Seal(rt.id, env, peer.PubKey)
	if err != nil {
		return nil, err
	}
	sent, err := rt.cl.Send(ctx, peer.InstanceID, sealed)
	if err != nil {
		return nil, err
	}
	if sent.Status != proto.SendDelivered {
		return nil, errPeerViewOffline
	}
	select {
	case a := <-ch:
		if !a.OK {
			if a.Reason == "busy" {
				return nil, errPeerViewBusy
			}
			return nil, fmt.Errorf("peer refused view: %s", proto.StripControls(a.Reason))
		}
		conn, err := rt.joinStream(ctx, peer.InstanceID, sid, kp, a.Key, true)
		if err != nil {
			// The hub's per-instance stream cap is busy too: retried.
			var refused *client.RefusedError
			if errors.As(err, &refused) && refused.Code == proto.CodeStreamLimit {
				return nil, errPeerViewBusy
			}
			return nil, err
		}
		success = true
		return &fedPeerViewConn{Conn: conn, release: func() { rt.releasePeerView("out:" + sid) }}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (rt *fedRuntime) acceptPeerViewOpen(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.PeerViewOpenPayload
	if env.DecodePayload(&p) != nil || !proto.ValidStreamID(p.Stream) || len(p.Key) != 32 {
		return
	}
	answer := func(ok bool, key []byte, reason string) {
		rt.sendControl(peer.InstanceID, proto.KindPeerViewAnswer, env.ID, proto.PeerViewAnswerPayload{Stream: p.Stream, OK: ok, Key: key, Reason: reason})
	}
	key := "in:" + peer.InstanceID + ":" + p.Stream
	if !rt.reservePeerView(key, peer.InstanceID) {
		answer(false, nil, "busy")
		return
	}
	// Keep replay protection for the full signed envelope lifetime. Admission
	// runs first, so a saturated peer cannot grow the replay table unchecked.
	fresh, err := db.MarkFederationEnvelopeSeen(peer.InstanceID, "peerview:"+env.ID, env.ExpiresAt.Add(time.Minute))
	if err != nil || !fresh {
		rt.releasePeerView(key)
		return
	}
	rt.wg.Add(1)
	go func() {
		defer rt.wg.Done()
		defer rt.releasePeerView(key)
		ctx, cancel := context.WithTimeout(rt.ctx, fedPeerViewTimeout)
		defer cancel()
		kp, err := stream.NewKeyPair()
		if err != nil {
			answer(false, nil, "unavailable")
			return
		}
		answer(true, kp.Pub, "")
		conn, err := rt.joinStream(ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		var wire fedPeerViewRequest
		if readPeerViewFrame(conn, fedPeerViewRequestLimit, &wire) != nil {
			return
		}
		parsed, err := url.ParseRequestURI(wire.URI)
		if err != nil || len(wire.URI) > 4096 || len(wire.Body) > peerViewBodyLimit(parsed.Path) || parsed.IsAbs() || parsed.Host != "" {
			return
		}
		req, err := http.NewRequestWithContext(ctx, wire.Method, "http://peer"+wire.URI, bytes.NewReader(wire.Body))
		if err != nil {
			return
		}
		// Copy only application headers. No caller identity, cookie, Origin or
		// operator token reaches the receiving node's authorization boundary.
		req.Header = peerViewRequestHeaders(wire.Header)
		out := &peerViewResponse{header: make(http.Header)}
		PeerViewHandler(peer.InstanceID).ServeHTTP(out, req)
		if out.body.Len() > fedPeerViewResponseBodyLimit {
			out = &peerViewResponse{header: make(http.Header)}
			writeError(out, 502, "peer_response_too_large", "peer response exceeds limit")
		}
		_ = writePeerViewFrame(conn, fedPeerViewResponseLimit, fedPeerViewReply{Status: out.statusCode(), Header: peerViewReplyHeaders(out.Header()), Body: out.body.Bytes()})
	}()
}

func peerViewRequestHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for _, key := range []string{"Content-Type", "If-None-Match"} {
		if v := h.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	return out
}

func peerViewReplyHeaders(h http.Header) http.Header {
	out := make(http.Header)
	for _, key := range []string{"Content-Type", "ETag", "Vary"} {
		if v := h.Get(key); v != "" {
			out.Set(key, v)
		}
	}
	return out
}

func writePeerViewFrame(w io.Writer, limit int, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("peer view frame exceeds limit")
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	if _, err = w.Write(prefix[:]); err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func readPeerViewFrame(r io.Reader, limit int, value any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size == 0 || size > uint32(limit) {
		return errors.New("peer view frame exceeds limit")
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(r, raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
