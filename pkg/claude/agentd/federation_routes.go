package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/federation/stream"
)

// Group routes across instances.
//
// A group exported with `routes` lists its ready routes in the peer's
// catalog. A member of a local group that imports that remote group opens
// one with `tclaude agent routes open <publisher>/<route>@<peer>`; both
// sandbox floors stay as they are, because neither sandbox helper ever sees
// the other instance:
//
//   - Consumer side: agentd creates a private "mirror" route owned by the
//     opening agent and attaches to the route broker as its publisher. The
//     agent's own helper consumes the mirror exactly like a local route.
//   - Publisher side: agentd opens a "proxy" lease on the real route in the
//     publisher's name and attaches to the broker as its consumer. The real
//     publisher helper serves it exactly like a local consumer.
//
// Each TCP connection the consumer makes becomes one broker stream on each
// side, joined by one end-to-end encrypted hub stream. The stream id and
// both ephemeral keys travel in sealed route_open/route_answer envelopes.
// Mirrors and proxies are re-authorized continuously (import/export still
// grant routes, peer still trusted, route still ready) on top of the
// broker's own generation checks, and torn down when that fails.
//
// Streams are flow-controlled end to end when the local helpers support it
// (routebroker flow.go): agentd grants the broker credit toward the hub only
// as it writes to the hub stream, and reads the hub stream only while it
// holds broker credit. A slow reader on either instance therefore stalls the
// hub stream, and through it the sender on the other instance, instead of
// filling a buffer.

const (
	fedRouteOpenTimeout   = 20 * time.Second
	fedRouteCheckInterval = 5 * time.Second
	// fedRouteStreamBuffer bounds bytes buffered toward one remote stream
	// that is not flow-controlled: a local sender faster than the hub's
	// per-instance bandwidth fills this and the connection is reset.
	fedRouteStreamBuffer = 4 << 20
	// fedRouteOpensPerMinute bounds route opens accepted from one peer,
	// separately from its mail budget.
	fedRouteOpensPerMinute = 240
)

// fedRouteState is the runtime's route bookkeeping, guarded by fedRuntime.mu.
type fedRouteState struct {
	waiters  map[string]chan fedRouteAnswer // stream id → open waiter
	proxies  map[string]*fedRouteEnd        // publisher side, peer+"|"+route id
	starting map[string]chan struct{}       // proxies being attached
	mirrors  map[string]*fedRouteEnd        // consumer side, mirror route id
	opens    map[string][]time.Time         // per-peer route open limiter
	// stopped is set when the runtime stops; ends finishing their start
	// afterwards are shut down instead of registered.
	stopped bool
}

type fedRouteAnswer struct {
	from string
	ans  proto.RouteAnswerPayload
}

func (rt *fedRuntime) routesLocked() *fedRouteState {
	if rt.routes == nil {
		rt.routes = &fedRouteState{
			waiters:  map[string]chan fedRouteAnswer{},
			proxies:  map[string]*fedRouteEnd{},
			starting: map[string]chan struct{}{},
			mirrors:  map[string]*fedRouteEnd{},
			opens:    map[string][]time.Time{},
		}
	}
	return rt.routes
}

// stopRoutes tears down every mirror and proxy when the runtime stops.
func (rt *fedRuntime) stopRoutes() {
	rt.mu.Lock()
	rt.routesLocked().stopped = true
	var ends []*fedRouteEnd
	if rt.routes != nil {
		for _, e := range rt.routes.mirrors {
			ends = append(ends, e)
		}
		for _, e := range rt.routes.proxies {
			ends = append(ends, e)
		}
	}
	rt.mu.Unlock()
	for _, e := range ends {
		e.shutdown()
		<-e.done
	}
}

// fedRouteEnd is agentd's broker channel for one mirror (publisher role) or
// one proxy lease (consumer role), multiplexing many TCP connections.
type fedRouteEnd struct {
	rt     *fedRuntime
	key    string
	role   string // "mirror" or "proxy"
	peer   string // remote instance id
	route  *db.AgentRoute
	lease  *db.AgentRouteLease // proxy only
	remote string              // mirror only: the remote route id
	// ctx scopes agentd's own goroutines. The broker channel itself is
	// attached without it and ended only by closing conn: a cancelled
	// broker session context would fail the real publisher's forwards.
	ctx    context.Context
	cancel context.CancelFunc
	conn   net.Conn
	// window is this end's flow-control receive window; 0 when flow
	// control is disabled and no stream on it negotiates it.
	window int

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	opening map[uint64]chan routebroker.Frame
	streams map[uint64]*fedRouteStream
	done    chan struct{}
}

func newFedRouteEnd(rt *fedRuntime, key, role, peer string, route *db.AgentRoute, conn net.Conn) *fedRouteEnd {
	ctx, cancel := context.WithCancel(context.Background())
	return &fedRouteEnd{
		rt: rt, key: key, role: role, peer: peer, route: route, ctx: ctx, cancel: cancel, conn: conn, window: routeFlowWindow(),
		opening: map[uint64]chan routebroker.Frame{}, streams: map[uint64]*fedRouteStream{}, done: make(chan struct{}),
	}
}

// shutdown ends the broker channel; serve then tears everything down.
func (e *fedRouteEnd) shutdown() {
	_ = e.conn.Close()
	e.cancel()
}

// fedRouteStream is one TCP connection. It is registered as soon as its
// broker stream exists, so frames that arrive while the hub stream is
// still being joined (a server's greeting, an early close) are buffered
// or acted on rather than lost.
type fedRouteStream struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once

	mu     sync.Mutex
	conn   *stream.Conn
	frames []routebroker.Frame
	bytes  int
	wake   chan struct{}

	// halfIn/halfOut record each direction finishing. gotHalfClose is set
	// when the local side's HalfClose is queued: the broker follows the
	// second half-close with a terminal CLOSE, which is then an orderly
	// end and must not discard what is still queued.
	halfIn, halfOut bool
	gotHalfClose    bool
	// sentHalfClose is set before our HalfClose is written, so it is set
	// whenever the broker's orderly CLOSE can arrive.
	sentHalfClose bool

	// flow is set by serve, before any DATA for the stream is read, when
	// the broker marks the stream flow-controlled; the bridge starts after.
	flow *fedStreamFlow
}

// fedStreamFlow is a flow-controlled stream's credit: send toward the
// broker, recv for what the broker sends us.
type fedStreamFlow struct {
	send *routebroker.SendWindow
	recv *routebroker.RecvWindow
}

// enableFlow makes s flow-controlled and returns the credit to grant once
// the stream is open.
func (s *fedRouteStream) enableFlow(window int) int {
	recv, grant := routebroker.NewRecvWindow(window)
	s.flow = &fedStreamFlow{send: routebroker.NewSendWindow(), recv: recv}
	return grant
}

// push buffers a broker frame for the remote side; false when over budget.
func (s *fedRouteStream) push(f routebroker.Frame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flow != nil {
		// Credit bounds the buffer: the broker only lets the local sender
		// run a window ahead of what the bridge has written to the hub.
		if s.flow.recv.Received(len(f.Payload)) != nil {
			return false
		}
	} else if s.bytes+len(f.Payload) > fedRouteStreamBuffer {
		return false
	}
	s.frames = append(s.frames, f)
	s.bytes += len(f.Payload)
	if f.Kind == routebroker.KindHalfClose {
		s.gotHalfClose = true
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

// pop waits for the next buffered frame; false once the stream ended.
func (s *fedRouteStream) pop() (routebroker.Frame, bool) {
	for {
		s.mu.Lock()
		if len(s.frames) > 0 {
			f := s.frames[0]
			s.frames = s.frames[1:]
			s.bytes -= len(f.Payload)
			s.mu.Unlock()
			return f, true
		}
		s.mu.Unlock()
		select {
		case <-s.wake:
		case <-s.ctx.Done():
			return routebroker.Frame{}, false
		}
	}
}

// attach hands the joined hub stream over; false if the stream already ended.
func (s *fedRouteStream) attach(conn *stream.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		go func() { _ = conn.Close() }()
		return false
	}
	s.conn = conn
	return true
}

// halfClosed records one direction finishing and reports whether both have.
func (s *fedRouteStream) halfClosed(in bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if in {
		s.halfIn = true
	} else {
		s.halfOut = true
	}
	return s.halfIn && s.halfOut
}

func (e *fedRouteEnd) write(f routebroker.Frame) error {
	e.wmu.Lock()
	defer e.wmu.Unlock()
	_ = e.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	err := routebroker.WriteFrame(e.conn, f, 0)
	if err != nil {
		// A frame may be half written: the channel's framing can no
		// longer be trusted.
		e.shutdown()
	}
	return err
}

// newStream registers stream id before anything can arrive for it.
func (e *fedRouteEnd) newStream(id uint64) *fedRouteStream {
	ctx, cancel := context.WithCancel(e.ctx)
	s := &fedRouteStream{id: id, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1)}
	e.mu.Lock()
	e.streams[id] = s
	e.mu.Unlock()
	return s
}

// serve reads broker frames until the channel ends, then tears down. It
// never blocks on the remote side.
func (e *fedRouteEnd) serve() {
	defer e.teardown()
	for {
		f, err := routebroker.ReadFrame(e.conn, 0)
		if err != nil {
			return
		}
		switch f.Kind {
		case routebroker.KindPing:
			_ = e.write(routebroker.Frame{Kind: routebroker.KindPong})
		case routebroker.KindPong:
		case routebroker.KindOpen:
			if e.role != "mirror" {
				return
			}
			e.mu.Lock()
			dup := e.streams[f.Stream] != nil
			e.mu.Unlock()
			if dup {
				return
			}
			s := e.newStream(f.Stream)
			grant := 0
			if e.window > 0 && routebroker.IsFlowOpen(f.Payload) {
				grant = s.enableFlow(e.window)
			}
			go e.openRemote(s, grant)
		case routebroker.KindOpenOK, routebroker.KindOpenError:
			e.mu.Lock()
			ch := e.opening[f.Stream]
			delete(e.opening, f.Stream)
			s := e.streams[f.Stream]
			e.mu.Unlock()
			if ch == nil {
				continue
			}
			if f.Kind == routebroker.KindOpenOK && s != nil && e.window > 0 && routebroker.IsFlowOpen(f.Payload) {
				if grant := s.enableFlow(e.window); grant > 0 {
					_ = e.write(routebroker.WindowFrame(s.id, grant))
				}
			}
			ch <- f
		case routebroker.KindWindow:
			e.mu.Lock()
			s := e.streams[f.Stream]
			e.mu.Unlock()
			if s == nil || s.flow == nil {
				continue
			}
			n, err := routebroker.ParseWindow(f.Payload)
			if err == nil {
				err = s.flow.send.Grant(n)
			}
			if err != nil {
				e.endStream(s, true)
			}
		case routebroker.KindData, routebroker.KindHalfClose, routebroker.KindClose:
			e.mu.Lock()
			s := e.streams[f.Stream]
			e.mu.Unlock()
			if s == nil {
				continue
			}
			if f.Kind == routebroker.KindClose {
				s.mu.Lock()
				// Orderly only after both directions half-closed; a CLOSE
				// after just the local half-close is an abort.
				orderly := s.gotHalfClose && s.sentHalfClose
				s.mu.Unlock()
				if orderly {
					continue // the bridge finishes once the queue drains
				}
				// The local side reset the connection: abort, which also
				// cancels an open still in flight.
				e.endStream(s, false)
				continue
			}
			if !s.push(f) {
				e.endStream(s, true)
			}
		}
	}
}

// watch re-checks federation authority until it fails or the end closes.
func (e *fedRouteEnd) watch(check func() error) {
	t := time.NewTicker(fedRouteCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			if err := check(); err != nil {
				slog.Info("federation: route authority withdrawn", "role", e.role, "route", e.route.ID, "peer", e.peer, "reason", err)
				e.shutdown()
				return
			}
		}
	}
}

func (e *fedRouteEnd) teardown() {
	e.shutdown()
	e.mu.Lock()
	streams := make([]*fedRouteStream, 0, len(e.streams))
	for _, s := range e.streams {
		streams = append(streams, s)
	}
	e.mu.Unlock()
	for _, s := range streams {
		e.endStream(s, false)
	}
	e.rt.mu.Lock()
	st := e.rt.routesLocked()
	if e.role == "mirror" && st.mirrors[e.key] == e {
		delete(st.mirrors, e.key)
	}
	if e.role == "proxy" && st.proxies[e.key] == e {
		delete(st.proxies, e.key)
	}
	e.rt.mu.Unlock()
	switch e.role {
	case "mirror":
		// A mirror without its proxy cannot carry traffic: withdraw it so
		// the consumer's lease closes; the next open creates a fresh one.
		_ = db.WithdrawAgentRoute(e.route.ID, e.route.PublisherAgentID, e.route.PublisherConvID, "federation route proxy ended")
		routeAdapterCloseRoute(e.route.ID)
	case "proxy":
		_ = db.CloseAgentRouteLease(e.lease.ID, e.lease.ConsumerAgentID, e.lease.ConsumerConvID)
	}
	close(e.done)
}

// bridge pumps one broker stream and its joined hub stream until both
// directions finish or either side aborts.
func (e *fedRouteEnd) bridge(s *fedRouteStream) {
	conn := s.conn
	// broker → remote
	go func() {
		for {
			f, ok := s.pop()
			if !ok {
				return
			}
			switch f.Kind {
			case routebroker.KindData:
				if _, err := conn.Write(f.Payload); err != nil {
					e.endStream(s, true)
					return
				}
				if s.flow != nil {
					if g := s.flow.recv.Consumed(len(f.Payload)); g > 0 {
						if err := e.write(routebroker.WindowFrame(s.id, g)); err != nil {
							return // the channel is gone; teardown ends the stream
						}
					}
				}
			case routebroker.KindHalfClose:
				if err := conn.CloseWrite(); err != nil {
					e.endStream(s, true)
					return
				}
				if s.halfClosed(true) {
					e.endStream(s, false)
				}
				return
			}
		}
	}()
	// remote → broker. With flow control the hub stream is read only as far
	// as the broker granted, so a slow local reader holds the remote sender.
	buf := make([]byte, routebroker.MaxFramePayload)
	for {
		limit := len(buf)
		if s.flow != nil {
			var werr error
			if limit, werr = s.flow.send.Wait(limit); werr != nil {
				return // the stream ended
			}
		}
		n, err := conn.Read(buf[:limit])
		if s.flow != nil {
			s.flow.send.Spend(n)
		}
		if n > 0 {
			if werr := e.write(routebroker.Frame{Kind: routebroker.KindData, Stream: s.id, Payload: append([]byte(nil), buf[:n]...)}); werr != nil {
				e.endStream(s, false)
				return
			}
		}
		if err == nil {
			continue
		}
		if !errors.Is(err, io.EOF) {
			// Reset, truncation or tampering: never an orderly end.
			e.endStream(s, true)
			return
		}
		s.mu.Lock()
		s.sentHalfClose = true
		s.mu.Unlock()
		_ = e.write(routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: s.id})
		if s.halfClosed(false) {
			e.endStream(s, false)
			return
		}
		// Keep reading so keepalives are answered and a later reset or a
		// dropped relay is noticed while our direction is still open.
		derr := conn.Drain()
		s.mu.Lock()
		both := s.halfIn && s.halfOut
		s.mu.Unlock()
		e.endStream(s, !both || errors.Is(derr, stream.ErrReset))
		return
	}
}

// endStream ends one connection: aborts the remote side unless it finished
// cleanly, and, if notify, resets the local side through the broker. It
// never blocks on the relay.
func (e *fedRouteEnd) endStream(s *fedRouteStream, notify bool) {
	s.once.Do(func() {
		e.mu.Lock()
		delete(e.streams, s.id)
		e.mu.Unlock()
		s.mu.Lock()
		s.cancel()
		conn := s.conn
		s.mu.Unlock()
		if s.flow != nil {
			s.flow.send.Close()
		}
		if conn != nil {
			go func() { _ = conn.Close() }()
		}
		if notify {
			_ = e.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: s.id})
		}
	})
}

// --- consumer side: mirrors ---

// openRemote handles one consumer connection on a mirror: ask the peer to
// open the real route, then join the hub stream.
func (e *fedRouteEnd) openRemote(s *fedRouteStream, grant int) {
	fail := func(reason string) {
		if s.ctx.Err() != nil {
			return // the consumer already gave up
		}
		e.endStream(s, false)
		_ = e.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: s.id, Payload: []byte(routebroker.OpenErrorTargetUnavailable)})
		slog.Info("federation: remote route open failed", "route", e.remote, "peer", e.peer, "reason", reason)
	}
	kp, err := stream.NewKeyPair()
	if err != nil {
		fail(err.Error())
		return
	}
	sid := proto.NewEnvelopeID()
	ans, err := e.rt.askRouteOpen(s.ctx, e.peer, proto.RouteOpenPayload{Route: e.remote, Stream: sid, Key: kp.Pub})
	if err != nil {
		fail(err.Error())
		return
	}
	conn, err := e.rt.joinStream(s.ctx, e.peer, sid, kp, ans.Key, true)
	if err != nil {
		fail(err.Error())
		return
	}
	if !s.attach(conn) {
		return
	}
	if err := e.write(routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: s.id}); err != nil {
		e.endStream(s, false)
		return
	}
	if grant > 0 {
		if err := e.write(routebroker.WindowFrame(s.id, grant)); err != nil {
			e.endStream(s, false)
			return
		}
	}
	e.bridge(s)
}

// askRouteOpen sends route_open to peer and waits for its answer.
func (rt *fedRuntime) askRouteOpen(ctx context.Context, peer string, p proto.RouteOpenPayload) (*proto.RouteAnswerPayload, error) {
	ch := make(chan fedRouteAnswer, 1)
	rt.mu.Lock()
	rt.routesLocked().waiters[p.Stream] = ch
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		delete(rt.routesLocked().waiters, p.Stream)
		rt.mu.Unlock()
	}()
	rt.sendControl(peer, proto.KindRouteOpen, "", p)
	select {
	case a := <-ch:
		if a.from != peer {
			return nil, errors.New("answer from the wrong instance")
		}
		if !a.ans.OK {
			return nil, fmt.Errorf("refused: %s", proto.StripControls(a.ans.Reason))
		}
		return &a.ans, nil
	case <-time.After(fedRouteOpenTimeout):
		return nil, errors.New("peer did not answer")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// joinStream dials the hub stream and wraps it with the derived keys.
func (rt *fedRuntime) joinStream(ctx context.Context, peer, sid string, kp *stream.KeyPair, peerKey []byte, initiator bool) (*stream.Conn, error) {
	keys, err := stream.DeriveKeys(kp, peerKey, sid, initiator)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, fedRouteOpenTimeout+10*time.Second)
	defer cancel()
	ws, err := rt.cl.DialStream(dctx, sid, peer)
	if err != nil {
		return nil, err
	}
	return stream.New(ws, keys)
}

// handleRouteAnswer hands a peer's answer to the waiting open.
func (rt *fedRuntime) handleRouteAnswer(peer *db.FederationPeer, env *proto.Envelope) {
	var a proto.RouteAnswerPayload
	if err := env.DecodePayload(&a); err != nil {
		return
	}
	rt.mu.Lock()
	ch := rt.routesLocked().waiters[a.Stream]
	rt.mu.Unlock()
	if ch != nil {
		select {
		case ch <- fedRouteAnswer{from: peer.InstanceID, ans: a}:
		default:
		}
	}
}

// attachEnd attaches e to the broker through attach and waits until the
// broker has accepted it. The channel then lives until e.conn closes.
func attachEnd(e *fedRouteEnd, theirs net.Conn, attach func(ready func(error)) error) error {
	attached := make(chan error, 1)
	report := func(err error) {
		select {
		case attached <- err:
		default:
		}
	}
	go func() {
		report(attach(report))
		_ = theirs.Close()
		e.shutdown()
	}()
	select {
	case err := <-attached:
		if err == nil {
			return nil
		}
		e.shutdown()
		return err
	case <-time.After(10 * time.Second):
		e.shutdown()
		return errors.New("route broker did not accept the federation channel")
	}
}

// startMirror attaches agentd as the broker publisher of a mirror route.
func (rt *fedRuntime) startMirror(route *db.AgentRoute, peer, remote string, check func() error) error {
	ours, theirs := net.Pipe()
	e := newFedRouteEnd(rt, route.ID, "mirror", peer, route, ours)
	e.remote = remote
	auth := routebroker.PublisherAuth{
		RouteID: route.ID, AgentID: route.PublisherAgentID, ConvID: route.PublisherConvID,
		LaunchGeneration: route.PublisherLaunchGeneration, GroupGeneration: route.GroupGeneration,
		FlowControl: e.window > 0,
	}
	err := attachEnd(e, theirs, func(ready func(error)) error {
		return GroupRouteBroker().AttachPublisherReady(context.Background(), auth, theirs, ready)
	})
	if err != nil {
		return err
	}
	rt.mu.Lock()
	st := rt.routesLocked()
	if st.stopped {
		rt.mu.Unlock()
		e.shutdown()
		return errors.New("federation stopped")
	}
	st.mirrors[route.ID] = e
	rt.mu.Unlock()
	go e.serve()
	go e.watch(check)
	return nil
}

type fedRouteOpenReq struct {
	Group string `json:"group"`
	Peer  string `json:"peer"`
	Route string `json:"route"` // "<publisher>/<name>" or a remote route id
}

// handleFederatedRouteOpen opens a remote route for the calling agent and
// returns an ordinary lease on its private mirror.
func handleFederatedRouteOpen(w http.ResponseWriter, r *http.Request) {
	rt := currentFederation()
	if rt == nil {
		writeRouteError(w, http.StatusServiceUnavailable, "federation_off", "federation is not running")
		return
	}
	var req fedRouteOpenReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&req); err != nil {
		writeRouteError(w, http.StatusBadRequest, "route_invalid_argument", err.Error())
		return
	}
	g, err := routeGroup(strings.TrimSpace(req.Group), 0)
	if err != nil || g == nil {
		writeRouteError(w, http.StatusBadRequest, "route_group", "explicit group selection is required")
		return
	}
	convID, agentID, ok := requireRouteConsumeCapability(w, r, g)
	if !ok {
		return
	}
	peer, err := resolveFederationPeerOpt(req.Peer, false)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	remote, remoteGroup, label, err := fedResolveRemoteRoute(peer, req.Route)
	if err != nil {
		writeFedErr(w, err)
		return
	}
	if !fedGroupImports(g.ID, peer.InstanceID, remoteGroup) {
		writeRouteError(w, http.StatusForbidden, "not_imported",
			fmt.Sprintf("%s/%s is not imported into %s", peerDisplay(peer), remoteGroup, g.Name))
		return
	}
	launchGeneration, err := routeLaunchGeneration(convID, "")
	if err != nil {
		writeRouteError(w, http.StatusConflict, "route_generation_stale", err.Error())
		return
	}
	mirror, err := db.FindFederationRouteMirror(g.ID, agentID, peer.InstanceID, remote)
	if err != nil {
		writeRouteError(w, http.StatusInternalServerError, "route_io", err.Error())
		return
	}
	rt.mu.Lock()
	live := mirror != nil && rt.routesLocked().mirrors[mirror.ID] != nil
	rt.mu.Unlock()
	if mirror != nil && (!live || mirror.GroupGeneration != g.RouteGeneration || mirror.PublisherConvID != convID || mirror.PublisherLaunchGeneration != launchGeneration) {
		_ = db.WithdrawAgentRoute(mirror.ID, mirror.PublisherAgentID, mirror.PublisherConvID, "federation mirror replaced")
		mirror = nil
	}
	if mirror == nil {
		name := label + "@" + proto.SafeName(peerDisplay(peer), true)
		name = strings.ReplaceAll(name, "/", ".")
		if len(name) > 120 {
			name = strings.ToValidUTF8(name[:120], "")
		}
		mirror, err = db.CreateFederationRouteMirror(g.ID, agentID, convID, launchGeneration, g.RouteGeneration, name,
			db.FederationRouteMirror{Peer: peer.InstanceID, RemoteRoute: remote, RemoteLabel: label})
		if err != nil {
			writeRouteError(w, http.StatusConflict, "route_open_refused", err.Error())
			return
		}
		mirrorID := mirror.ID
		check := func() error { return fedMirrorAuthorized(mirrorID, g.ID, peer.InstanceID, remoteGroup, remote) }
		if err := rt.startMirror(mirror, peer.InstanceID, remote, check); err != nil {
			_ = db.WithdrawAgentRoute(mirror.ID, agentID, convID, "federation publisher refused")
			writeRouteError(w, http.StatusConflict, "route_adapter", err.Error())
			return
		}
	}
	lease, err := db.OpenAgentRouteLease(mirror.ID, agentID, convID, launchGeneration, g.RouteGeneration)
	if err != nil {
		writeRouteError(w, http.StatusConflict, "route_open_refused", err.Error())
		return
	}
	endpoint, enabled, err := routeAdapterOpen(context.Background(), mirror, lease)
	if err != nil {
		_ = db.CloseAgentRouteLease(lease.ID, agentID, convID)
		writeRouteError(w, http.StatusConflict, "route_adapter", err.Error())
		return
	}
	if enabled && strings.TrimSpace(endpoint) != "" {
		_ = setRouteConsumerEndpointReady(lease.ID, endpoint)
	}
	setAuditTargetLabel(r, label+"@"+peerDisplay(peer))
	writeJSON(w, http.StatusCreated, routeLeaseViewFor(lease))
}

// fedResolveRemoteRoute finds ref ("<publisher>/<name>" or a route id) in
// the peer's catalog among groups exporting routes.
func fedResolveRemoteRoute(peer *db.FederationPeer, ref string) (routeID, group, label string, err error) {
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil {
		return "", "", "", err
	}
	if cat == nil {
		return "", "", "", newFedErr(http.StatusNotFound, "no_catalog", "no catalog received from %s yet", peerDisplay(peer))
	}
	ref = strings.TrimSpace(ref)
	for _, g := range cat.Groups {
		if !g.HasCap(proto.CapRoutes) {
			continue
		}
		for _, r := range g.Routes {
			lbl := r.Publisher + "/" + r.Name
			if r.ID == ref || strings.EqualFold(lbl, ref) {
				if routeID != "" && routeID != r.ID {
					return "", "", "", newFedErr(http.StatusConflict, "ambiguous", "%q matches several routes on %s; use the route id", ref, peerDisplay(peer))
				}
				routeID, group, label = r.ID, g.Name, lbl
			}
		}
	}
	if routeID == "" {
		return "", "", "", newFedErr(http.StatusNotFound, "route_not_found", "%s exports no route %q", peerDisplay(peer), ref)
	}
	return routeID, group, label, nil
}

// fedGroupImports reports whether local group localID imports peer's group.
func fedGroupImports(localID int64, peer, remoteGroup string) bool {
	imports, err := db.ListFederationImports()
	if err != nil {
		return false
	}
	for _, im := range imports {
		if im.LocalGroupID == localID && im.Peer == peer && im.RemoteGroup == remoteGroup {
			return true
		}
	}
	return false
}

// fedMirrorAuthorized re-checks a mirror's federation authority: peer
// trusted, import present, route still in the peer's catalog.
func fedMirrorAuthorized(mirrorID string, localID int64, peer, remoteGroup, remote string) error {
	if route, _ := db.GetAgentRoute(mirrorID); route == nil || route.State != db.RouteStateReady {
		return errors.New("mirror withdrawn")
	}
	if p, _ := db.GetFederationPeer(peer); p == nil {
		return errors.New("peer no longer trusted")
	}
	if !fedGroupImports(localID, peer, remoteGroup) {
		return errors.New("import removed")
	}
	cat, _, err := fedCatalogFor(peer)
	if err != nil || cat == nil {
		return errors.New("no catalog")
	}
	for _, g := range cat.Groups {
		if g.Name != remoteGroup || !g.HasCap(proto.CapRoutes) {
			continue
		}
		for _, r := range g.Routes {
			if r.ID == remote {
				return nil
			}
		}
	}
	return errors.New("route no longer exported")
}

// --- publisher side: proxies ---

// fedProxyAuthorized re-checks a proxy's authority: peer trusted, route
// ready and not itself a mirror, group exported to the peer with routes.
func fedProxyAuthorized(peer, routeID string) (*db.AgentRoute, error) {
	if p, _ := db.GetFederationPeer(peer); p == nil {
		return nil, errors.New("peer not trusted")
	}
	route, err := db.GetAgentRoute(routeID)
	if err != nil || route == nil || route.State != db.RouteStateReady {
		return nil, errors.New("no such ready route")
	}
	if m, _ := db.GetFederationRouteMirror(route.ID); m != nil {
		return nil, errors.New("no such ready route") // no transit through mirrors
	}
	g, _ := db.GetAgentGroupByID(route.GroupID)
	if g == nil || g.IsArchived() || !fedGroupExportsCap(peer, g.ID, proto.CapRoutes) {
		return nil, errors.New("no such ready route")
	}
	return route, nil
}

// handleRouteOpen serves a peer's request to connect to one of our routes.
// It runs on its own goroutine: it waits on the broker and the hub.
func (rt *fedRuntime) handleRouteOpen(peer *db.FederationPeer, env *proto.Envelope) {
	var p proto.RouteOpenPayload
	if err := env.DecodePayload(&p); err != nil || !proto.ValidStreamID(p.Stream) {
		return
	}
	answer := func(a proto.RouteAnswerPayload) {
		a.Stream = p.Stream
		rt.sendControl(peer.InstanceID, proto.KindRouteAnswer, env.ID, a)
	}
	rt.mu.Lock()
	allowed := allowPerMinute(rt.routesLocked().opens, peer.InstanceID, fedRouteOpensPerMinute)
	rt.mu.Unlock()
	if !allowed {
		answer(proto.RouteAnswerPayload{Reason: "rate limited"})
		return
	}
	route, err := fedProxyAuthorized(peer.InstanceID, p.Route)
	if err != nil {
		answer(proto.RouteAnswerPayload{Reason: err.Error()})
		return
	}
	e, err := rt.proxyFor(peer.InstanceID, route)
	if err != nil {
		answer(proto.RouteAnswerPayload{Reason: "route unavailable"})
		slog.Info("federation: route proxy failed", "route", route.ID, "error", err)
		return
	}
	// Register the stream before opening it, so whatever the target sends
	// first is buffered while the peer joins; and open the broker stream
	// before answering, so the peer only joins a hub stream for a
	// connection the publisher accepted.
	e.mu.Lock()
	e.nextID++
	id := e.nextID
	ch := make(chan routebroker.Frame, 1)
	e.opening[id] = ch
	e.mu.Unlock()
	s := e.newStream(id)
	refuse := func(reason string, notify bool) {
		e.mu.Lock()
		delete(e.opening, id)
		e.mu.Unlock()
		e.endStream(s, notify)
		answer(proto.RouteAnswerPayload{Reason: reason})
	}
	if err := e.write(routebroker.Frame{Kind: routebroker.KindOpen, Stream: id}); err != nil {
		refuse("route unavailable", false)
		return
	}
	select {
	case f := <-ch:
		if f.Kind != routebroker.KindOpenOK {
			refuse("publisher refused: "+string(f.Payload), false)
			return
		}
	case <-time.After(fedRouteOpenTimeout):
		refuse("publisher did not answer", true)
		return
	case <-s.ctx.Done():
		refuse("route unavailable", false)
		return
	}
	kp, err := stream.NewKeyPair()
	if err != nil {
		refuse("route unavailable", true)
		return
	}
	answer(proto.RouteAnswerPayload{OK: true, Key: kp.Pub})
	conn, err := rt.joinStream(s.ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
	if err != nil {
		e.endStream(s, true)
		return
	}
	if s.attach(conn) {
		e.bridge(s)
	}
}

// proxyFor returns the live proxy consumer for (peer, route), starting one.
// Concurrent opens share one start: each proxy holds a broker consumer
// slot on the route.
func (rt *fedRuntime) proxyFor(peer string, route *db.AgentRoute) (*fedRouteEnd, error) {
	key := peer + "|" + route.ID
	for {
		rt.mu.Lock()
		st := rt.routesLocked()
		if e := st.proxies[key]; e != nil {
			select {
			case <-e.done:
			default:
				rt.mu.Unlock()
				return e, nil
			}
		}
		wait := st.starting[key]
		if wait == nil {
			st.starting[key] = make(chan struct{})
			rt.mu.Unlock()
			break
		}
		rt.mu.Unlock()
		select {
		case <-wait:
		case <-time.After(15 * time.Second):
			return nil, errors.New("route proxy start timed out")
		}
	}
	e, err := rt.startProxy(key, peer, route)
	rt.mu.Lock()
	st := rt.routesLocked()
	if err == nil && st.stopped {
		e.shutdown()
		_ = db.CloseAgentRouteLease(e.lease.ID, e.lease.ConsumerAgentID, e.lease.ConsumerConvID)
		e, err = nil, errors.New("federation stopped")
	}
	if err == nil {
		st.proxies[key] = e
	}
	close(st.starting[key])
	delete(st.starting, key)
	rt.mu.Unlock()
	if err != nil {
		return nil, err
	}
	go e.serve()
	go e.watch(func() error { _, err := fedProxyAuthorized(peer, route.ID); return err })
	return e, nil
}

func (rt *fedRuntime) startProxy(key, peer string, route *db.AgentRoute) (*fedRouteEnd, error) {
	lease, err := db.OpenFederationProxyLease(route, peer)
	if err != nil {
		return nil, err
	}
	ours, theirs := net.Pipe()
	e := newFedRouteEnd(rt, key, "proxy", peer, route, ours)
	e.lease = lease
	auth := routebroker.ConsumerAuth{
		LeaseID: lease.ID, RouteID: route.ID, AgentID: lease.ConsumerAgentID, ConvID: lease.ConsumerConvID,
		LaunchGeneration: lease.ConsumerLaunchGeneration, GroupGeneration: lease.GroupGeneration,
		FlowControl: e.window > 0,
	}
	err = attachEnd(e, theirs, func(ready func(error)) error {
		return GroupRouteBroker().AttachConsumerWithReady(context.Background(), auth, theirs, func() error { ready(nil); return nil })
	})
	if err != nil {
		_ = db.CloseAgentRouteLease(lease.ID, lease.ConsumerAgentID, lease.ConsumerConvID)
		return nil, err
	}
	return e, nil
}

// withdrawStaleFederationMirrors withdraws mirrors left from a previous
// runtime (their proxy is gone, so they cannot carry traffic) and drops
// withdrawn mirror rows.
func withdrawStaleFederationMirrors() {
	mirrors, err := db.ListFederationRouteMirrors()
	if err != nil {
		return
	}
	for _, m := range mirrors {
		if route, _ := db.GetAgentRoute(m.RouteID); route != nil && route.State == db.RouteStateReady {
			_ = db.WithdrawAgentRoute(route.ID, route.PublisherAgentID, route.PublisherConvID, "federation runtime restarted")
			routeAdapterCloseRoute(route.ID)
		}
	}
	if err := db.DeleteStaleFederationRouteMirrors(); err != nil {
		slog.Debug("federation: drop stale route mirrors failed", "error", err)
	}
}

// fedCatalogRoutes lists a group's ready routes for its catalog entry.
func fedCatalogRoutes(groupID int64) []proto.CatalogRoute {
	routes, err := db.ListAgentRoutes(groupID)
	if err != nil {
		return nil
	}
	var out []proto.CatalogRoute
	for _, r := range routes {
		if r.State != db.RouteStateReady || !routePublisherLive(r) {
			continue
		}
		out = append(out, proto.CatalogRoute{ID: r.ID, Publisher: proto.SafeName(agent.TitleFor(r.PublisherConvID), false), Name: proto.SafeName(r.Name, false)})
	}
	return out
}
