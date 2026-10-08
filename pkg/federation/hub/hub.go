// Package hub implements tclaude-hub: a relay that agentd instances dial out
// to. It admits instances, scopes who can see whom via spaces, rate-limits,
// and routes signed envelopes between online instances. It is never the
// authority over what an agent may do and cannot forge or alter envelopes;
// the sending and receiving daemons enforce permissions.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Config tunes a Hub. Zero values pick defaults.
type Config struct {
	// Open admits any instance that proves key possession into
	// DefaultSpace. Development only.
	Open bool
	// FramesPerMinute and BytesPerMinute bound what one instance may send.
	FramesPerMinute int
	BytesPerMinute  int
	// PolicyRefresh is how often admission/spaces are re-read from the
	// store, so admin CLI edits (revoke, spaces) take effect on live
	// connections.
	PolicyRefresh time.Duration
	// HelloTimeout bounds the challenge/hello handshake.
	HelloTimeout time.Duration
	// MaxStreams caps one instance's concurrent stream dialers (route
	// relays); StreamBytesPerSecond caps the bandwidth they share.
	MaxStreams           int
	StreamBytesPerSecond int
	// StreamWait bounds how long a stream dialer waits for its peer, and
	// StreamIdle how long a stream may go without traffic or pongs, or
	// with a receiver that does not accept what is forwarded to it.
	StreamWait time.Duration
	StreamIdle time.Duration
	Logger     *slog.Logger
	Version    string
}

func (c *Config) defaults() {
	if c.FramesPerMinute <= 0 {
		c.FramesPerMinute = 120
	}
	if c.BytesPerMinute <= 0 {
		c.BytesPerMinute = 8 << 20
	}
	if c.PolicyRefresh <= 0 {
		c.PolicyRefresh = 15 * time.Second
	}
	if c.HelloTimeout <= 0 {
		c.HelloTimeout = 10 * time.Second
	}
	if c.MaxStreams <= 0 {
		c.MaxStreams = 16
	}
	if c.StreamBytesPerSecond <= 0 {
		c.StreamBytesPerSecond = 1 << 20
	}
	if c.StreamWait <= 0 {
		c.StreamWait = 30 * time.Second
	}
	if c.StreamIdle <= 0 {
		c.StreamIdle = 90 * time.Second
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Hub routes envelopes between connected instances.
type Hub struct {
	cfg   Config
	store *Store
	hubID string
	log   *slog.Logger

	upgrader websocket.Upgrader

	mu     sync.Mutex
	conns  map[string]*conn
	policy *admittedSnapshot
	closed bool
	// limiters are per instance, not per connection, so reconnecting does
	// not refill an instance's budget.
	limiters map[string]*bucketPair
	// streams is the stream relay state (see stream.go).
	streams *streamState

	stop chan struct{}
	wg   sync.WaitGroup
}

// New creates a hub over store and starts its policy refresher.
func New(store *Store, cfg Config) (*Hub, error) {
	cfg.defaults()
	hubID, err := store.HubID()
	if err != nil {
		return nil, err
	}
	snap, err := store.snapshot()
	if err != nil {
		return nil, err
	}
	h := &Hub{
		cfg: cfg, store: store, hubID: hubID, log: cfg.Logger.With("component", "hub"),
		upgrader: websocket.Upgrader{ReadBufferSize: 16 << 10, WriteBufferSize: 16 << 10},
		conns:    map[string]*conn{}, policy: snap, stop: make(chan struct{}),
		limiters: map[string]*bucketPair{},
	}
	h.wg.Add(1)
	go h.refreshLoop()
	return h, nil
}

// ID returns the hub id.
func (h *Hub) ID() string { return h.hubID }

// Handler serves the hub endpoints.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(proto.WSPath, h.serveWS)
	mux.HandleFunc(proto.StreamPath, h.serveStream)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "hub_id": h.hubID, "online": h.OnlineCount()})
	})
	return mux
}

// OnlineCount returns how many instances are connected.
func (h *Hub) OnlineCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.conns)
}

// Close drops every connection and stops background work.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	conns := make([]*conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	streams := h.streamsToDropLocked(func(string) bool { return false })
	h.mu.Unlock()
	close(h.stop)
	for _, c := range conns {
		c.fail(proto.CodeShuttingDown, "hub shutting down")
	}
	for _, s := range streams {
		s.close()
	}
	h.wg.Wait()
}

// RefreshPolicy re-reads admission and spaces now, dropping revoked
// connections and re-sending directories. The refresher calls it
// periodically; tests and admin paths may call it directly.
func (h *Hub) RefreshPolicy() {
	snap, err := h.store.snapshot()
	if err != nil {
		h.log.Warn("policy refresh failed", "error", err)
		return
	}
	h.mu.Lock()
	h.policy = snap
	var drop []*conn
	for id, c := range h.conns {
		if !snap.admitted[id] {
			drop = append(drop, c)
		}
	}
	// A stream survives only while both of its ends stay admitted and
	// visible to each other.
	var dropStreams []*streamSession
	if h.streams != nil {
		for _, set := range h.streams.sessions {
			for s := range set {
				if !snap.admitted[s.id] || !snap.visible(s.id, s.peer) {
					dropStreams = append(dropStreams, s)
				}
			}
		}
	}
	h.mu.Unlock()
	for _, c := range drop {
		c.fail(proto.CodeNotAdmitted, "instance revoked")
	}
	for _, s := range dropStreams {
		s.fail(proto.CodeNotAdmitted, "stream endpoint revoked")
	}
	h.broadcastDirectories()
}

func (h *Hub) refreshLoop() {
	defer h.wg.Done()
	t := time.NewTicker(h.cfg.PolicyRefresh)
	defer t.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-t.C:
			h.RefreshPolicy()
		}
	}
}

type conn struct {
	hub     *Hub
	ws      *websocket.Conn
	id      string
	name    string
	version string
	pub     []byte
	out     chan *proto.Frame
	done    chan struct{}
	once    sync.Once
	limiter *bucketPair
	// wmu serialises writes: gorilla/websocket allows one concurrent
	// writer, and fail may run alongside writeLoop.
	wmu sync.Mutex
}

func (c *conn) write(f *proto.Frame, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(timeout))
	return c.ws.WriteJSON(f)
}

// send queues f; a full queue means a stuck reader, which is dropped rather
// than allowed to stall routing for everyone.
func (c *conn) send(f *proto.Frame) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- f:
		return true
	default:
		c.hub.log.Warn("dropping slow instance", "instance", c.id)
		go c.fail(proto.CodeRateLimited, "outbound queue full")
		return false
	}
}

func (c *conn) fail(code, msg string) {
	c.once.Do(func() {
		if code != "" {
			_ = c.write(&proto.Frame{Type: proto.FrameError, Code: code, Message: msg}, 2*time.Second)
		}
		close(c.done)
		_ = c.ws.Close()
	})
}

func (c *conn) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.out:
			if err := c.write(f, 10*time.Second); err != nil {
				c.fail("", "")
				return
			}
		case <-ping.C:
			c.wmu.Lock()
			err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			c.wmu.Unlock()
			if err != nil {
				c.fail("", "")
				return
			}
		}
	}
}

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(proto.MaxEnvelopeBytes*2 + 64<<10)
	c, err := h.handshake(ws)
	if err != nil {
		h.log.Info("handshake refused", "remote", r.RemoteAddr, "error", err)
		return
	}
	h.wg.Add(1)
	go func() { defer h.wg.Done(); c.writeLoop() }()
	h.register(c)
	h.readLoop(c)
	h.unregister(c)
}

type refusal struct{ code, msg string }

func (r *refusal) Error() string { return r.code + ": " + r.msg }

func (h *Hub) handshake(ws *websocket.Conn) (*conn, error) {
	nonce := randHex(16)
	deadline := time.Now().Add(h.cfg.HelloTimeout)
	_ = ws.SetWriteDeadline(deadline)
	if err := ws.WriteJSON(&proto.Frame{Type: proto.FrameChallenge, HubID: h.hubID, Nonce: nonce, Proto: proto.ProtocolVersion}); err != nil {
		_ = ws.Close()
		return nil, err
	}
	_ = ws.SetReadDeadline(deadline)
	var hello proto.Frame
	if err := ws.ReadJSON(&hello); err != nil {
		_ = ws.Close()
		return nil, err
	}
	refuse := func(code, msg string) (*conn, error) {
		_ = ws.WriteJSON(&proto.Frame{Type: proto.FrameError, Code: code, Message: msg})
		_ = ws.Close()
		return nil, &refusal{code, msg}
	}
	if hello.Type != proto.FrameHello {
		return refuse(proto.CodeBadFrame, "expected hello")
	}
	if hello.Proto != proto.ProtocolVersion {
		return refuse(proto.CodeBadVersion, "unsupported protocol version")
	}
	if !proto.VerifyHello(&hello, h.hubID, nonce) {
		return refuse(proto.CodeBadAuth, "hello signature does not verify")
	}
	id := hello.InstanceID
	now := time.Now()
	h.mu.Lock()
	admitted := h.policy.admitted[id]
	h.mu.Unlock()
	if !admitted {
		// The cache may predate an admin-CLI admission; consult the store
		// before refusing.
		if snap, err := h.store.snapshot(); err == nil {
			h.mu.Lock()
			h.policy = snap
			h.mu.Unlock()
			admitted = snap.admitted[id]
		}
	}
	if !admitted {
		switch {
		case hello.Invite != "":
			if err := h.store.RedeemInvite(hello.Invite, id, now); err != nil {
				return refuse(proto.CodeNotAdmitted, err.Error())
			}
		case h.cfg.Open:
			if err := h.store.Admit(id); err != nil {
				return refuse(proto.CodeNotAdmitted, err.Error())
			}
		default:
			return refuse(proto.CodeNotAdmitted, "instance "+id+" is not admitted to this hub")
		}
		h.log.Info("instance admitted", "instance", id, "via_invite", hello.Invite != "")
		snap, err := h.store.snapshot()
		if err != nil {
			return refuse(proto.CodeNotAdmitted, "policy reload failed")
		}
		h.mu.Lock()
		h.policy = snap
		h.mu.Unlock()
	}
	name := truncate(hello.Name, 64)
	if err := h.store.RecordSeen(id, hello.PubKey, name, truncate(hello.Version, 64), now); err != nil {
		return refuse(proto.CodeNotAdmitted, "store: "+err.Error())
	}
	h.mu.Lock()
	spaces := h.policy.spaces[id]
	h.mu.Unlock()
	_ = ws.SetReadDeadline(time.Time{})
	_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := ws.WriteJSON(&proto.Frame{Type: proto.FrameWelcome, HubID: h.hubID, InstanceID: id, Spaces: spaces, Version: h.cfg.Version}); err != nil {
		_ = ws.Close()
		return nil, err
	}
	_ = ws.SetWriteDeadline(time.Time{})
	ws.SetPongHandler(func(string) error { _ = ws.SetReadDeadline(time.Now().Add(90 * time.Second)); return nil })
	_ = ws.SetReadDeadline(time.Now().Add(90 * time.Second))
	h.mu.Lock()
	lim := h.limiters[id]
	if lim == nil {
		lim = newBucketPair(h.cfg.FramesPerMinute, h.cfg.BytesPerMinute)
		h.limiters[id] = lim
	}
	h.mu.Unlock()
	return &conn{
		hub: h, ws: ws, id: id, name: name, version: hello.Version, pub: hello.PubKey,
		out: make(chan *proto.Frame, 256), done: make(chan struct{}),
		limiter: lim,
	}, nil
}

func (h *Hub) register(c *conn) {
	h.mu.Lock()
	old := h.conns[c.id]
	h.conns[c.id] = c
	h.mu.Unlock()
	if old != nil {
		old.fail(proto.CodeReplaced, "replaced by a newer connection from the same instance")
	}
	h.log.Info("instance connected", "instance", c.id, "name", c.name)
	h.broadcastDirectories()
}

func (h *Hub) unregister(c *conn) {
	c.fail("", "")
	h.mu.Lock()
	if h.conns[c.id] == c {
		delete(h.conns, c.id)
	}
	closed := h.closed
	h.mu.Unlock()
	_ = h.store.RecordSeen(c.id, c.pub, c.name, c.version, time.Now())
	h.log.Info("instance disconnected", "instance", c.id)
	if !closed {
		h.broadcastDirectories()
	}
}

func (h *Hub) readLoop(c *conn) {
	for {
		var f proto.Frame
		_, raw, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(90 * time.Second))
		if err := json.Unmarshal(raw, &f); err != nil {
			c.fail(proto.CodeBadFrame, "malformed frame")
			return
		}
		switch f.Type {
		case proto.FrameSend:
			h.route(c, &f, len(raw))
		default:
			c.send(&proto.Frame{Type: proto.FrameError, Code: proto.CodeBadFrame, Message: "unexpected frame " + f.Type})
		}
	}
}

func (h *Hub) route(c *conn, f *proto.Frame, size int) {
	result := func(status, code, msg string) {
		c.send(&proto.Frame{Type: proto.FrameSendResult, Ref: f.Ref, To: f.To, Status: status, Code: code, Message: msg})
	}
	if !c.limiter.allow(size, time.Now()) {
		result(proto.SendRateLimited, proto.CodeRateLimited, "instance rate limit exceeded")
		return
	}
	if f.Sealed == nil || len(f.Sealed.Env) == 0 || len(f.Sealed.Env) > proto.MaxEnvelopeBytes {
		result(proto.SendRefused, proto.CodeBadFrame, "missing or oversized envelope")
		return
	}
	h.mu.Lock()
	visible := h.policy.visible(c.id, f.To)
	target := h.conns[f.To]
	h.mu.Unlock()
	if !visible {
		result(proto.SendRefused, proto.CodeNotVisible, "target is not visible to this instance")
		return
	}
	if target == nil {
		result(proto.SendOffline, "", "target instance is offline")
		return
	}
	if !target.send(&proto.Frame{Type: proto.FrameDeliver, From: c.id, Sealed: f.Sealed}) {
		result(proto.SendOffline, "", "target connection unavailable")
		return
	}
	result(proto.SendDelivered, "", "")
}

func (h *Hub) broadcastDirectories() {
	all, err := h.store.List()
	if err != nil {
		h.log.Warn("directory list failed", "error", err)
		return
	}
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.conns))
	for _, c := range h.conns {
		conns = append(conns, c)
	}
	online := map[string]bool{}
	for id := range h.conns {
		online[id] = true
	}
	policy := h.policy
	h.mu.Unlock()
	for _, c := range conns {
		var entries []proto.DirectoryEntry
		for _, in := range all {
			if len(in.PubKey) == 0 || !policy.visible(c.id, in.ID) {
				continue
			}
			entries = append(entries, proto.DirectoryEntry{
				InstanceID: in.ID, PubKey: in.PubKey, Name: in.Name,
				Online: online[in.ID], LastSeen: in.LastSeen, Version: in.Version,
			})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].InstanceID < entries[j].InstanceID })
		c.send(&proto.Frame{Type: proto.FrameDirectory, Instances: entries})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ListenAndServe serves h on srv until ctx is done.
func ListenAndServe(ctx context.Context, srv *http.Server, h *Hub, certFile, keyFile string) error {
	srv.Handler = h.Handler()
	errc := make(chan error, 1)
	go func() {
		if certFile != "" {
			errc <- srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		h.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		h.Close()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(sctx)
	}
}
