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

const (
	fedRouteOpenTimeout   = 20 * time.Second
	fedRouteCheckInterval = 5 * time.Second
	// fedRouteStreamQueue bounds frames buffered toward one remote stream.
	fedRouteStreamQueue = 32
)

// fedRouteState is the runtime's route bookkeeping, guarded by fedRuntime.mu.
type fedRouteState struct {
	waiters map[string]chan fedRouteAnswer // stream id → open waiter
	proxies map[string]*fedRouteEnd        // publisher side, peer+"|"+route id
	mirrors map[string]*fedRouteEnd        // consumer side, mirror route id
}

type fedRouteAnswer struct {
	from string
	ans  proto.RouteAnswerPayload
}

func (rt *fedRuntime) routesLocked() *fedRouteState {
	if rt.routes == nil {
		rt.routes = &fedRouteState{
			waiters: map[string]chan fedRouteAnswer{},
			proxies: map[string]*fedRouteEnd{},
			mirrors: map[string]*fedRouteEnd{},
		}
	}
	return rt.routes
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
	ctx    context.Context
	cancel context.CancelFunc
	conn   net.Conn

	wmu sync.Mutex

	mu      sync.Mutex
	nextID  uint64
	opening map[uint64]chan routebroker.Frame
	streams map[uint64]*fedRouteStream
	done    chan struct{}
}

type fedRouteStream struct {
	id   uint64
	conn *stream.Conn
	q    chan routebroker.Frame
	once sync.Once
	done chan struct{}

	// The broker retires a stream silently once both directions have
	// half-closed, so the bridge must notice that itself.
	hmu             sync.Mutex
	halfIn, halfOut bool
}

// halfClosed records one direction finishing and reports whether both have.
func (s *fedRouteStream) halfClosed(in bool) bool {
	s.hmu.Lock()
	defer s.hmu.Unlock()
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
	return routebroker.WriteFrame(e.conn, f, 0)
}

// serve reads broker frames until the channel ends, then tears down.
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
			go e.openRemote(f.Stream)
		case routebroker.KindOpenOK, routebroker.KindOpenError:
			e.mu.Lock()
			ch := e.opening[f.Stream]
			delete(e.opening, f.Stream)
			e.mu.Unlock()
			if ch != nil {
				ch <- f
			}
		case routebroker.KindData, routebroker.KindHalfClose, routebroker.KindClose:
			e.mu.Lock()
			s := e.streams[f.Stream]
			e.mu.Unlock()
			if s == nil {
				continue
			}
			select {
			case s.q <- f:
			default:
				// The remote side is not draining; drop the connection
				// rather than stall every stream on this channel.
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
				e.cancel()
				_ = e.conn.Close()
				return
			}
		}
	}
}

func (e *fedRouteEnd) teardown() {
	e.cancel()
	_ = e.conn.Close()
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

// bridge pumps one broker stream and one encrypted hub stream until both
// directions finish.
func (e *fedRouteEnd) bridge(s *fedRouteStream) {
	// broker → remote
	go func() {
		for {
			select {
			case <-e.ctx.Done():
				e.endStream(s, false)
				return
			case <-s.done:
				return
			case f := <-s.q:
				switch f.Kind {
				case routebroker.KindData:
					if _, err := s.conn.Write(f.Payload); err != nil {
						e.endStream(s, true)
						return
					}
				case routebroker.KindHalfClose:
					_ = s.conn.CloseWrite()
					if s.halfClosed(true) {
						e.endStream(s, false)
						return
					}
				case routebroker.KindClose:
					e.endStream(s, false)
					return
				}
			}
		}
	}()
	// remote → broker
	buf := make([]byte, routebroker.MaxFramePayload)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			if werr := e.write(routebroker.Frame{Kind: routebroker.KindData, Stream: s.id, Payload: append([]byte(nil), buf[:n]...)}); werr != nil {
				e.endStream(s, false)
				return
			}
		}
		if errors.Is(err, io.EOF) {
			_ = e.write(routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: s.id})
			if s.halfClosed(false) {
				e.endStream(s, false)
			}
			return
		}
		if err != nil {
			e.endStream(s, true)
			return
		}
	}
}

// endStream closes the remote side and, if notify, tells the broker.
func (e *fedRouteEnd) endStream(s *fedRouteStream, notify bool) {
	s.once.Do(func() {
		e.mu.Lock()
		delete(e.streams, s.id)
		e.mu.Unlock()
		close(s.done)
		_ = s.conn.Close()
		if notify {
			_ = e.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: s.id})
		}
	})
}

func (e *fedRouteEnd) addStream(id uint64, conn *stream.Conn) *fedRouteStream {
	s := &fedRouteStream{id: id, conn: conn, q: make(chan routebroker.Frame, fedRouteStreamQueue), done: make(chan struct{})}
	e.mu.Lock()
	e.streams[id] = s
	e.mu.Unlock()
	return s
}

// --- consumer side: mirrors ---

// openRemote handles one consumer connection on a mirror: ask the peer to
// open the real route, then join the hub stream.
func (e *fedRouteEnd) openRemote(id uint64) {
	fail := func(reason string) {
		_ = e.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: id, Payload: []byte(routebroker.OpenErrorTargetUnavailable)})
		slog.Info("federation: remote route open failed", "route", e.remote, "peer", e.peer, "reason", reason)
	}
	kp, err := stream.NewKeyPair()
	if err != nil {
		fail(err.Error())
		return
	}
	sid := proto.NewEnvelopeID()
	ans, err := e.rt.askRouteOpen(e.ctx, e.peer, proto.RouteOpenPayload{Route: e.remote, Stream: sid, Key: kp.Pub})
	if err != nil {
		fail(err.Error())
		return
	}
	conn, err := e.rt.joinStream(e.ctx, e.peer, sid, kp, ans.Key, true)
	if err != nil {
		fail(err.Error())
		return
	}
	s := e.addStream(id, conn)
	if err := e.write(routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: id}); err != nil {
		e.endStream(s, false)
		return
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

// startMirror attaches agentd as the broker publisher of a mirror route.
func (rt *fedRuntime) startMirror(route *db.AgentRoute, peer, remote string, check func() error) error {
	ctx, cancel := context.WithCancel(rt.ctx)
	ours, theirs := net.Pipe()
	e := &fedRouteEnd{
		rt: rt, key: route.ID, role: "mirror", peer: peer, route: route, remote: remote,
		ctx: ctx, cancel: cancel, conn: ours,
		opening: map[uint64]chan routebroker.Frame{}, streams: map[uint64]*fedRouteStream{}, done: make(chan struct{}),
	}
	auth := routebroker.PublisherAuth{
		RouteID: route.ID, AgentID: route.PublisherAgentID, ConvID: route.PublisherConvID,
		LaunchGeneration: route.PublisherLaunchGeneration, GroupGeneration: route.GroupGeneration,
	}
	attached := make(chan error, 1)
	go func() {
		err := GroupRouteBroker().AttachPublisherReady(ctx, auth, theirs, func(err error) { attached <- err })
		if err != nil {
			select {
			case attached <- err:
			default:
			}
		}
		_ = theirs.Close()
		cancel()
	}()
	select {
	case err := <-attached:
		if err != nil {
			cancel()
			_ = ours.Close()
			return err
		}
	case <-time.After(10 * time.Second):
		cancel()
		_ = ours.Close()
		return errors.New("route broker did not accept the federation publisher")
	}
	rt.mu.Lock()
	rt.routesLocked().mirrors[route.ID] = e
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
	if !rt.allowInbound(peer.InstanceID) {
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
	// Open the broker stream first, so the peer only joins a hub stream
	// for a connection the publisher actually accepted.
	e.mu.Lock()
	e.nextID++
	id := e.nextID
	ch := make(chan routebroker.Frame, 1)
	e.opening[id] = ch
	e.mu.Unlock()
	if err := e.write(routebroker.Frame{Kind: routebroker.KindOpen, Stream: id}); err != nil {
		answer(proto.RouteAnswerPayload{Reason: "route unavailable"})
		return
	}
	select {
	case f := <-ch:
		if f.Kind != routebroker.KindOpenOK {
			answer(proto.RouteAnswerPayload{Reason: "publisher refused: " + string(f.Payload)})
			return
		}
	case <-time.After(fedRouteOpenTimeout):
		e.mu.Lock()
		delete(e.opening, id)
		e.mu.Unlock()
		_ = e.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
		answer(proto.RouteAnswerPayload{Reason: "publisher did not answer"})
		return
	case <-e.ctx.Done():
		answer(proto.RouteAnswerPayload{Reason: "route unavailable"})
		return
	}
	kp, err := stream.NewKeyPair()
	if err != nil {
		_ = e.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
		return
	}
	answer(proto.RouteAnswerPayload{OK: true, Key: kp.Pub})
	conn, err := rt.joinStream(e.ctx, peer.InstanceID, p.Stream, kp, p.Key, false)
	if err != nil {
		_ = e.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
		return
	}
	e.bridge(e.addStream(id, conn))
}

// proxyFor returns the live proxy consumer for (peer, route), starting one.
func (rt *fedRuntime) proxyFor(peer string, route *db.AgentRoute) (*fedRouteEnd, error) {
	key := peer + "|" + route.ID
	rt.mu.Lock()
	if e := rt.routesLocked().proxies[key]; e != nil {
		select {
		case <-e.done:
		default:
			rt.mu.Unlock()
			return e, nil
		}
	}
	rt.mu.Unlock()
	lease, err := db.OpenFederationProxyLease(route, peer)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(rt.ctx)
	ours, theirs := net.Pipe()
	e := &fedRouteEnd{
		rt: rt, key: key, role: "proxy", peer: peer, route: route, lease: lease,
		ctx: ctx, cancel: cancel, conn: ours,
		opening: map[uint64]chan routebroker.Frame{}, streams: map[uint64]*fedRouteStream{}, done: make(chan struct{}),
	}
	auth := routebroker.ConsumerAuth{
		LeaseID: lease.ID, RouteID: route.ID, AgentID: lease.ConsumerAgentID, ConvID: lease.ConsumerConvID,
		LaunchGeneration: lease.ConsumerLaunchGeneration, GroupGeneration: lease.GroupGeneration,
	}
	attached := make(chan error, 1)
	go func() {
		err := GroupRouteBroker().AttachConsumerWithReady(ctx, auth, theirs, func() error { attached <- nil; return nil })
		select {
		case attached <- err:
		default:
		}
		_ = theirs.Close()
		cancel()
	}()
	select {
	case err := <-attached:
		if err != nil {
			cancel()
			_ = ours.Close()
			_ = db.CloseAgentRouteLease(lease.ID, lease.ConsumerAgentID, lease.ConsumerConvID)
			return nil, err
		}
	case <-time.After(10 * time.Second):
		cancel()
		_ = ours.Close()
		_ = db.CloseAgentRouteLease(lease.ID, lease.ConsumerAgentID, lease.ConsumerConvID)
		return nil, errors.New("route broker did not accept the federation consumer")
	}
	rt.mu.Lock()
	rt.routesLocked().proxies[key] = e
	rt.mu.Unlock()
	go e.serve()
	go e.watch(func() error { _, err := fedProxyAuthorized(peer, route.ID); return err })
	return e, nil
}

// withdrawStaleFederationMirrors withdraws mirrors left from a previous
// runtime: their proxy is gone, so they cannot carry traffic.
func withdrawStaleFederationMirrors() {
	mirrors, err := db.ListFederationRouteMirrors()
	if err != nil {
		return
	}
	for _, m := range mirrors {
		if route, _ := db.GetAgentRoute(m.RouteID); route != nil {
			_ = db.WithdrawAgentRoute(route.ID, route.PublisherAgentID, route.PublisherConvID, "federation runtime restarted")
		}
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
