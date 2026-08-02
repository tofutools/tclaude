package routebroker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// PublisherAuth is the M1 identity presented by a publisher-side helper when
// it attaches its route channel. Every field is checked by the authority;
// route IDs and display names are never treated as interchangeable.
type PublisherAuth struct {
	RouteID          string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupGeneration  int64
}

// ConsumerAuth is the M1 lease identity presented by a consumer-side helper.
// A lease is intentionally distinct from the publisher route identity.
type ConsumerAuth struct {
	LeaseID          string
	RouteID          string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupGeneration  int64
}

// Authorizer is the narrow seam to M1's route/lease authority. Authorize is
// called before a channel is accepted and again periodically while it is
// attached. Implementations must fail closed for withdrawn routes, closed or
// stale leases, publisher loss, membership changes, and launch-generation
// changes. No route payload is supplied to this interface.
type Authorizer interface {
	AuthorizePublisher(context.Context, PublisherAuth) error
	AuthorizeConsumer(context.Context, ConsumerAuth) error
}

// Config bounds each resource the broker can retain. The first engine stays
// in agentd rather than adding a subprocess: frames are synchronously bounded
// at the protocol edge and each stream has a small finite outbound queue. A
// slow attached channel eventually hits WriteTimeout and is closed; it never
// creates an unbounded agentd queue.
type Config struct {
	Authorizer Authorizer

	MaxFramePayload          int
	MaxConnectionsPerRoute   int
	MaxConnectionsPerAgent   int
	MaxConsumersPerRoute     int
	MaxQueuedFramesPerStream int
	WriteTimeout             time.Duration
	AuthorityCheckInterval   time.Duration

	Logger  *slog.Logger
	OnEvent func(Event)
}

const (
	defaultMaxConnectionsPerRoute = 64
	defaultMaxConnectionsPerAgent = 128
	defaultMaxConsumersPerRoute   = 32
	defaultQueuedFramesPerStream  = 4
	defaultWriteTimeout           = 5 * time.Second
	defaultAuthorityCheckInterval = 250 * time.Millisecond
	tombstoneTTL                  = 30 * time.Second
)

var (
	ErrClosed            = errors.New("route broker is closed")
	ErrUnauthorized      = errors.New("route broker channel is unauthorized")
	ErrPublisherAttached = errors.New("route broker publisher channel already attached")
	ErrRouteLimit        = errors.New("route broker route connection limit reached")
	ErrAgentLimit        = errors.New("route broker agent connection limit reached")
	ErrConsumerLimit     = errors.New("route broker consumer limit reached")
	ErrBackpressure      = errors.New("route broker stream backpressure limit reached")
	ErrAuthorityRevoked  = errors.New("route broker authority was revoked")
)

// Event is metadata-only observability. Bytes counts are useful for capacity
// diagnosis; Payload is deliberately absent so route bytes cannot enter logs,
// callbacks, or persistence through this package.
type Event struct {
	Kind        string
	Role        string
	RouteID     string
	AgentID     string
	LeaseID     string
	StreamID    uint64
	Connections int
	Bytes       int
	Error       string
}

// Metrics is a point-in-time view of broker counters. Counters contain no
// payload contents and are reset when the daemon restarts.
type Metrics struct {
	Routes              uint64
	PublisherChannels   uint64
	ConsumerChannels    uint64
	Streams             uint64
	BytesForwarded      uint64
	FramesForwarded     uint64
	RejectedConnections uint64
	RejectedStreams     uint64
}

// Broker is an in-process, route-scoped multiplexing engine. It has no
// listener and does not persist route state; platform endpoint adapters own
// the authenticated channel that they pass to AttachPublisher/AttachConsumer.
type Broker struct {
	authorizer               Authorizer
	maxFramePayload          int
	maxConnectionsPerRoute   int
	maxConnectionsPerAgent   int
	maxConsumersPerRoute     int
	maxQueuedFramesPerStream int
	writeTimeout             time.Duration
	authorityCheckInterval   time.Duration
	logger                   *slog.Logger
	onEvent                  func(Event)

	mu     sync.Mutex
	closed bool
	routes map[string]*routeState
	// Broker-wide accounting prevents one agent from multiplying its budget
	// by opening streams on several routes. All changes happen under b.mu.
	agentStreams map[string]int
	wg           sync.WaitGroup

	routesMetric              atomic.Uint64
	publishersMetric          atomic.Uint64
	consumersMetric           atomic.Uint64
	streamsMetric             atomic.Uint64
	bytesMetric               atomic.Uint64
	framesMetric              atomic.Uint64
	rejectedConnectionsMetric atomic.Uint64
	rejectedStreamsMetric     atomic.Uint64
}

type routeState struct {
	id           string
	publisher    *session
	consumers    map[*session]struct{}
	streams      map[uint64]*stream
	nextStreamID uint64
	tombstones   map[uint64]streamTombstone
}

type role uint8

const (
	rolePublisher role = iota + 1
	roleConsumer
)

func (r role) string() string {
	if r == rolePublisher {
		return "publisher"
	}
	return "consumer"
}

type session struct {
	broker        *Broker
	role          role
	route         *routeState
	publisherAuth PublisherAuth
	consumerAuth  ConsumerAuth
	conn          net.Conn
	ctx           context.Context
	cancel        context.CancelFunc
	closed        sync.Once
	detached      bool

	writeMu   sync.Mutex
	writersMu sync.Mutex
	writers   map[uint64]*streamWriter
}

type stream struct {
	globalID            uint64
	localID             uint64
	consumer            *session
	agentID             string
	consumerHalfClosed  bool
	publisherHalfClosed bool
}

type streamTombstone struct {
	consumer  *session
	localID   uint64
	expiresAt time.Time
}

type outbound struct {
	frame    Frame
	terminal bool
}

type streamWriter struct {
	session *session
	id      uint64
	ctx     context.Context
	cancel  context.CancelFunc
	queue   chan outbound
}

type pendingClose struct {
	stream    *stream
	publisher *session
}

// New constructs an in-process broker. An Authorizer is required: transport
// channels must not be accepted without M1 authority proof.
func New(cfg Config) (*Broker, error) {
	if cfg.Authorizer == nil {
		return nil, fmt.Errorf("route broker: %w", ErrUnauthorized)
	}
	if cfg.MaxFramePayload <= 0 {
		cfg.MaxFramePayload = defaultFrameLength
	}
	if cfg.MaxConnectionsPerRoute <= 0 {
		cfg.MaxConnectionsPerRoute = defaultMaxConnectionsPerRoute
	}
	if cfg.MaxConnectionsPerAgent <= 0 {
		cfg.MaxConnectionsPerAgent = defaultMaxConnectionsPerAgent
	}
	if cfg.MaxConsumersPerRoute <= 0 {
		cfg.MaxConsumersPerRoute = defaultMaxConsumersPerRoute
	}
	if cfg.MaxQueuedFramesPerStream <= 0 {
		cfg.MaxQueuedFramesPerStream = defaultQueuedFramesPerStream
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = defaultWriteTimeout
	}
	if cfg.AuthorityCheckInterval <= 0 {
		cfg.AuthorityCheckInterval = defaultAuthorityCheckInterval
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Broker{
		authorizer:               cfg.Authorizer,
		maxFramePayload:          cfg.MaxFramePayload,
		maxConnectionsPerRoute:   cfg.MaxConnectionsPerRoute,
		maxConnectionsPerAgent:   cfg.MaxConnectionsPerAgent,
		maxConsumersPerRoute:     cfg.MaxConsumersPerRoute,
		maxQueuedFramesPerStream: cfg.MaxQueuedFramesPerStream,
		writeTimeout:             cfg.WriteTimeout,
		authorityCheckInterval:   cfg.AuthorityCheckInterval,
		logger:                   logger,
		onEvent:                  cfg.OnEvent,
		routes:                   make(map[string]*routeState),
		agentStreams:             make(map[string]int),
	}, nil
}

// AttachPublisher authenticates and serves one publisher channel until its
// peer exits, authority is revoked, or the broker is closed. The call is
// intended to run in the adapter's goroutine and returns after cleanup.
func (b *Broker) AttachPublisher(ctx context.Context, auth PublisherAuth, conn net.Conn) error {
	if conn == nil {
		return fmt.Errorf("route broker: nil publisher channel")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := b.authorizer.AuthorizePublisher(ctx, auth); err != nil {
		b.rejectedConnectionsMetric.Add(1)
		b.emit(Event{Kind: "publisher-rejected", Role: rolePublisher.string(), RouteID: auth.RouteID, AgentID: auth.AgentID, Error: "authority refused"})
		_ = conn.Close()
		return fmt.Errorf("%w: publisher: %v", ErrUnauthorized, err)
	}
	s := b.newSession(ctx, rolePublisher, auth, ConsumerAuth{}, conn)
	// Claim the wait-group slot before publishing the session under b.mu so
	// Close cannot return between registration and Add.
	b.wg.Add(1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		s.close()
		b.wg.Done()
		return ErrClosed
	}
	r := b.getRouteLocked(auth.RouteID)
	if r.publisher != nil {
		b.mu.Unlock()
		s.close()
		b.wg.Done()
		b.rejectedConnectionsMetric.Add(1)
		return ErrPublisherAttached
	}
	r.publisher = s
	s.route = r
	b.publishersMetric.Add(1)
	b.mu.Unlock()
	b.emit(Event{Kind: "publisher-attached", Role: rolePublisher.string(), RouteID: auth.RouteID, AgentID: auth.AgentID})
	return b.serveSession(s, ctx)
}

// AttachConsumer authenticates and serves one consumer lease channel until
// its peer exits, authority is revoked, or the broker is closed. Multiple
// consumer channels may attach to one route, subject to the configured
// bounded count.
func (b *Broker) AttachConsumer(ctx context.Context, auth ConsumerAuth, conn net.Conn) error {
	if conn == nil {
		return fmt.Errorf("route broker: nil consumer channel")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := b.authorizer.AuthorizeConsumer(ctx, auth); err != nil {
		b.rejectedConnectionsMetric.Add(1)
		b.emit(Event{Kind: "consumer-rejected", Role: roleConsumer.string(), RouteID: auth.RouteID, AgentID: auth.AgentID, LeaseID: auth.LeaseID, Error: "authority refused"})
		_ = conn.Close()
		return fmt.Errorf("%w: consumer: %v", ErrUnauthorized, err)
	}
	s := b.newSession(ctx, roleConsumer, PublisherAuth{}, auth, conn)
	b.wg.Add(1)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		s.close()
		b.wg.Done()
		return ErrClosed
	}
	r := b.getRouteLocked(auth.RouteID)
	if len(r.consumers) >= b.maxConsumersPerRoute {
		b.mu.Unlock()
		s.close()
		b.wg.Done()
		b.rejectedConnectionsMetric.Add(1)
		return ErrConsumerLimit
	}
	r.consumers[s] = struct{}{}
	s.route = r
	consumerCount := len(r.consumers)
	b.consumersMetric.Add(1)
	b.mu.Unlock()
	b.emit(Event{Kind: "consumer-attached", Role: roleConsumer.string(), RouteID: auth.RouteID, AgentID: auth.AgentID, LeaseID: auth.LeaseID, Connections: consumerCount})
	return b.serveSession(s, ctx)
}

// Close closes every attached channel and waits for their forwarding loops to
// leave. It is safe to call more than once and is the daemon shutdown seam.
func (b *Broker) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	var sessions []*session
	for _, r := range b.routes {
		if r.publisher != nil {
			sessions = append(sessions, r.publisher)
		}
		for s := range r.consumers {
			sessions = append(sessions, s)
		}
	}
	b.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	b.wg.Wait()
	b.emit(Event{Kind: "broker-closed"})
	return nil
}

// Metrics returns bounded in-memory counters for diagnostics. It does not
// expose route payloads or connection contents.
func (b *Broker) Metrics() Metrics {
	return Metrics{
		Routes:              b.routesMetric.Load(),
		PublisherChannels:   b.publishersMetric.Load(),
		ConsumerChannels:    b.consumersMetric.Load(),
		Streams:             b.streamsMetric.Load(),
		BytesForwarded:      b.bytesMetric.Load(),
		FramesForwarded:     b.framesMetric.Load(),
		RejectedConnections: b.rejectedConnectionsMetric.Load(),
		RejectedStreams:     b.rejectedStreamsMetric.Load(),
	}
}

func (b *Broker) newSession(ctx context.Context, r role, publisher PublisherAuth, consumer ConsumerAuth, conn net.Conn) *session {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	return &session{broker: b, role: r, publisherAuth: publisher, consumerAuth: consumer, conn: conn, ctx: sessionCtx, cancel: cancel, writers: make(map[uint64]*streamWriter)}
}

func (b *Broker) getRouteLocked(id string) *routeState {
	r := b.routes[id]
	if r == nil {
		r = &routeState{id: id, consumers: make(map[*session]struct{}), streams: make(map[uint64]*stream), tombstones: make(map[uint64]streamTombstone), nextStreamID: 1}
		b.routes[id] = r
		b.routesMetric.Add(1)
	}
	return r
}

func (b *Broker) serveSession(s *session, callerCtx context.Context) error {
	defer b.wg.Done()
	go s.watchAuthority()
	defer func() {
		s.cancel()
		b.detach(s)
	}()

	for {
		frame, err := ReadFrame(s.conn, b.maxFramePayload)
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			b.emit(Event{Kind: "channel-error", Role: s.role.string(), RouteID: s.routeID(), AgentID: s.agentID(), LeaseID: s.leaseID(), Error: "frame read failed"})
			return err
		}
		if err := b.handleFrame(s, frame); err != nil {
			if errors.Is(err, ErrBackpressure) {
				b.emit(Event{Kind: "channel-backpressure", Role: s.role.string(), RouteID: s.routeID(), AgentID: s.agentID(), LeaseID: s.leaseID(), StreamID: frame.Stream, Error: "bounded queue full"})
			}
			s.close()
			return err
		}
	}
}

func (s *session) watchAuthority() {
	ticker := time.NewTicker(s.broker.authorityCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(s.ctx, s.broker.authorityCheckInterval)
			var err error
			if s.role == rolePublisher {
				err = s.broker.authorizer.AuthorizePublisher(checkCtx, s.publisherAuth)
			} else {
				err = s.broker.authorizer.AuthorizeConsumer(checkCtx, s.consumerAuth)
			}
			cancel()
			if err != nil {
				s.broker.emit(Event{Kind: "authority-revoked", Role: s.role.string(), RouteID: s.routeID(), AgentID: s.agentID(), LeaseID: s.leaseID(), Error: "authority no longer valid"})
				s.close()
				return
			}
		}
	}
}

func (s *session) close() {
	s.closed.Do(func() {
		s.cancel()
		_ = s.conn.Close()
	})
}

func (s *session) routeID() string {
	if s.role == rolePublisher {
		return s.publisherAuth.RouteID
	}
	return s.consumerAuth.RouteID
}

func (s *session) agentID() string {
	if s.role == rolePublisher {
		return s.publisherAuth.AgentID
	}
	return s.consumerAuth.AgentID
}

func (s *session) leaseID() string {
	if s.role == roleConsumer {
		return s.consumerAuth.LeaseID
	}
	return ""
}

func (b *Broker) handleFrame(s *session, frame Frame) error {
	if s.role == rolePublisher {
		return b.handlePublisherFrame(s, frame)
	}
	return b.handleConsumerFrame(s, frame)
}

func (b *Broker) handleConsumerFrame(s *session, frame Frame) error {
	switch frame.Kind {
	case KindOpen:
		if len(frame.Payload) != 0 {
			return fmt.Errorf("%w: OPEN payload must be empty", ErrProtocol)
		}
		return b.openStream(s, frame.Stream)
	case KindPing:
		return s.enqueue(0, Frame{Kind: KindPong}, false)
	case KindPong:
		return nil
	case KindData, KindHalfClose, KindClose:
		return b.forwardFromConsumer(s, frame)
	default:
		return fmt.Errorf("%w: consumer sent frame kind %d", ErrProtocol, frame.Kind)
	}
}

func (b *Broker) handlePublisherFrame(s *session, frame Frame) error {
	switch frame.Kind {
	case KindPing:
		return s.enqueue(0, Frame{Kind: KindPong}, false)
	case KindPong:
		return nil
	case KindOpenOK, KindOpenError, KindData, KindHalfClose, KindClose:
		return b.forwardFromPublisher(s, frame)
	default:
		return fmt.Errorf("%w: publisher sent frame kind %d", ErrProtocol, frame.Kind)
	}
}

func (b *Broker) openStream(consumer *session, localID uint64) error {
	if localID == 0 {
		return ErrInvalidStreamID
	}
	b.mu.Lock()
	r := consumer.route
	if r == nil || b.closed {
		b.mu.Unlock()
		return ErrClosed
	}
	b.pruneTombstonesLocked(r)
	for _, existing := range r.streams {
		if existing.consumer == consumer && existing.localID == localID {
			b.mu.Unlock()
			_ = consumer.enqueue(localID, Frame{Kind: KindOpenError, Stream: localID, Payload: []byte("duplicate stream id")}, true)
			b.rejectedStreamsMetric.Add(1)
			return nil
		}
	}
	for _, tombstone := range r.tombstones {
		if tombstone.consumer == consumer && tombstone.localID == localID {
			b.mu.Unlock()
			_ = consumer.enqueue(localID, Frame{Kind: KindOpenError, Stream: localID, Payload: []byte("duplicate stream id")}, true)
			b.rejectedStreamsMetric.Add(1)
			return nil
		}
	}
	if r.publisher == nil {
		b.mu.Unlock()
		if err := consumer.enqueue(localID, Frame{Kind: KindOpenError, Stream: localID, Payload: []byte("publisher unavailable")}, true); err != nil {
			return err
		}
		b.rejectedStreamsMetric.Add(1)
		return nil
	}
	if len(r.streams) >= b.maxConnectionsPerRoute {
		b.mu.Unlock()
		if err := consumer.enqueue(localID, Frame{Kind: KindOpenError, Stream: localID, Payload: []byte("route connection limit")}, true); err != nil {
			return err
		}
		b.rejectedStreamsMetric.Add(1)
		return nil
	}
	agentID := consumer.consumerAuth.AgentID
	if b.agentStreams[agentID] >= b.maxConnectionsPerAgent {
		b.mu.Unlock()
		if err := consumer.enqueue(localID, Frame{Kind: KindOpenError, Stream: localID, Payload: []byte("agent connection limit")}, true); err != nil {
			return err
		}
		b.rejectedStreamsMetric.Add(1)
		return nil
	}
	globalID := r.nextStreamID
	r.nextStreamID++
	stream := &stream{globalID: globalID, localID: localID, consumer: consumer, agentID: agentID}
	r.streams[globalID] = stream
	b.agentStreams[agentID]++
	publisher := r.publisher
	streamCount := len(r.streams)
	b.streamsMetric.Add(1)
	b.mu.Unlock()
	b.emit(Event{Kind: "stream-open", Role: roleConsumer.string(), RouteID: r.id, AgentID: agentID, LeaseID: consumer.consumerAuth.LeaseID, StreamID: globalID, Connections: streamCount})
	if err := publisher.enqueue(globalID, Frame{Kind: KindOpen, Stream: globalID}, false); err != nil {
		if errors.Is(err, ErrBackpressure) {
			b.rejectedStreamsMetric.Add(1)
			publisher.close()
			return nil
		}
		b.removeStream(stream, false, false)
		return err
	}
	return nil
}

func (b *Broker) forwardFromConsumer(consumer *session, frame Frame) error {
	b.mu.Lock()
	stream, tombstone := b.findConsumerStreamLocked(consumer, frame.Stream)
	if stream == nil {
		b.mu.Unlock()
		if tombstone {
			return nil
		}
		return fmt.Errorf("%w: unknown consumer stream", ErrProtocol)
	}
	publisher := stream.consumer.route.publisher
	if frame.Kind == KindHalfClose {
		stream.consumerHalfClosed = true
	}
	b.mu.Unlock()
	if publisher == nil {
		return ErrClosed
	}
	terminal := frame.Kind == KindClose
	if err := publisher.enqueue(stream.globalID, Frame{Kind: frame.Kind, Stream: stream.globalID, Payload: frame.Payload}, terminal); err != nil {
		if errors.Is(err, ErrBackpressure) {
			b.rejectedStreamsMetric.Add(1)
			publisher.close()
			return nil
		}
		return err
	}
	if frame.Kind == KindClose {
		b.removeStream(stream, true, false)
	}
	return nil
}

func (b *Broker) forwardFromPublisher(publisher *session, frame Frame) error {
	b.mu.Lock()
	r := publisher.route
	if r == nil || r.publisher != publisher {
		b.mu.Unlock()
		return ErrClosed
	}
	b.pruneTombstonesLocked(r)
	stream := r.streams[frame.Stream]
	if stream == nil {
		b.mu.Unlock()
		if _, ok := r.tombstones[frame.Stream]; ok {
			return nil
		}
		return fmt.Errorf("%w: unknown publisher stream", ErrProtocol)
	}
	if frame.Kind == KindHalfClose {
		stream.publisherHalfClosed = true
	}
	consumer := stream.consumer
	localID := stream.localID
	b.mu.Unlock()
	terminal := frame.Kind == KindClose || frame.Kind == KindOpenError
	if err := consumer.enqueue(localID, Frame{Kind: frame.Kind, Stream: localID, Payload: frame.Payload}, terminal); err != nil {
		if errors.Is(err, ErrBackpressure) {
			b.rejectedStreamsMetric.Add(1)
			consumer.close()
			return nil
		}
		return err
	}
	if frame.Kind == KindClose || frame.Kind == KindOpenError {
		b.removeStream(stream, false, true)
	}
	return nil
}

func (b *Broker) findConsumerStreamLocked(consumer *session, localID uint64) (*stream, bool) {
	r := consumer.route
	if r == nil {
		return nil, false
	}
	b.pruneTombstonesLocked(r)
	for _, stream := range r.streams {
		if stream.consumer == consumer && stream.localID == localID {
			return stream, false
		}
	}
	for _, tombstone := range r.tombstones {
		if tombstone.consumer == consumer && tombstone.localID == localID {
			return nil, true
		}
	}
	return nil, false
}

func (b *Broker) pruneTombstonesLocked(r *routeState) {
	now := time.Now()
	for id, tombstone := range r.tombstones {
		if !tombstone.expiresAt.After(now) {
			delete(r.tombstones, id)
		}
	}
}

func (b *Broker) addTombstoneLocked(r *routeState, stream *stream) {
	b.pruneTombstonesLocked(r)
	maxTombstones := b.maxConnectionsPerRoute * 2
	if maxTombstones < 16 {
		maxTombstones = 16
	}
	for len(r.tombstones) >= maxTombstones {
		var oldestID uint64
		var oldest time.Time
		for id, tombstone := range r.tombstones {
			if oldestID == 0 || tombstone.expiresAt.Before(oldest) {
				oldestID = id
				oldest = tombstone.expiresAt
			}
		}
		delete(r.tombstones, oldestID)
	}
	r.tombstones[stream.globalID] = streamTombstone{
		consumer:  stream.consumer,
		localID:   stream.localID,
		expiresAt: time.Now().Add(tombstoneTTL),
	}
}

func (b *Broker) decrementAgentStreamLocked(agentID string) {
	if b.agentStreams[agentID] > 1 {
		b.agentStreams[agentID]--
	} else {
		delete(b.agentStreams, agentID)
	}
}

// removeStream removes routing state immediately. A keep*Writer flag is used
// when the terminal frame was just queued to that destination: its small
// writer queue must remain alive long enough to flush that CLOSE/OPEN_ERROR.
// The opposite writer is cancelled immediately, so a closed stream cannot
// retain a goroutine or queued payload.
func (b *Broker) removeStream(stream *stream, keepPublisherWriter, keepConsumerWriter bool) {
	b.mu.Lock()
	if stream.consumer == nil || stream.consumer.route == nil {
		b.mu.Unlock()
		return
	}
	r := stream.consumer.route
	if current := r.streams[stream.globalID]; current != stream {
		b.mu.Unlock()
		return
	}
	delete(r.streams, stream.globalID)
	b.decrementAgentStreamLocked(stream.agentID)
	b.addTombstoneLocked(r, stream)
	b.streamsMetric.Add(^uint64(0))
	publisher := r.publisher
	b.mu.Unlock()
	if !keepConsumerWriter {
		stream.consumer.removeWriter(stream.localID)
	}
	if publisher != nil && !keepPublisherWriter {
		publisher.removeWriter(stream.globalID)
	}
	b.emit(Event{Kind: "stream-closed", Role: roleConsumer.string(), RouteID: r.id, AgentID: stream.agentID, LeaseID: stream.consumer.consumerAuth.LeaseID, StreamID: stream.globalID})
}

func (b *Broker) detach(s *session) {
	b.mu.Lock()
	if s.detached {
		b.mu.Unlock()
		return
	}
	s.detached = true
	r := s.route
	if r == nil {
		b.mu.Unlock()
		return
	}
	var notifyPublisher []pendingClose
	var closeConsumers []*session
	if s.role == rolePublisher {
		if r.publisher == s {
			r.publisher = nil
		}
		for consumer := range r.consumers {
			closeConsumers = append(closeConsumers, consumer)
		}
		for id, stream := range r.streams {
			delete(r.streams, id)
			b.decrementAgentStreamLocked(stream.agentID)
			b.addTombstoneLocked(r, stream)
			b.streamsMetric.Add(^uint64(0))
			stream.consumer.removeWriter(stream.localID)
		}
	} else {
		delete(r.consumers, s)
		for id, stream := range r.streams {
			if stream.consumer != s {
				continue
			}
			delete(r.streams, id)
			b.decrementAgentStreamLocked(stream.agentID)
			b.addTombstoneLocked(r, stream)
			b.streamsMetric.Add(^uint64(0))
			publisher := r.publisher
			notifyPublisher = append(notifyPublisher, pendingClose{stream: stream, publisher: publisher})
		}
	}
	if len(r.consumers) == 0 && r.publisher == nil && len(r.streams) == 0 {
		delete(b.routes, r.id)
		b.routesMetric.Add(^uint64(0))
	}
	b.mu.Unlock()

	if s.role == rolePublisher {
		for _, consumer := range closeConsumers {
			consumer.close()
		}
		b.emit(Event{Kind: "publisher-detached", Role: rolePublisher.string(), RouteID: r.id, AgentID: s.agentID()})
	} else {
		for _, pending := range notifyPublisher {
			if pending.publisher != nil {
				if err := pending.publisher.enqueue(pending.stream.globalID, Frame{Kind: KindClose, Stream: pending.stream.globalID}, true); err != nil && errors.Is(err, ErrBackpressure) {
					pending.publisher.close()
				}
			}
		}
		b.emit(Event{Kind: "consumer-detached", Role: roleConsumer.string(), RouteID: r.id, AgentID: s.agentID(), LeaseID: s.leaseID()})
	}
	if s.role == rolePublisher {
		b.publishersMetric.Add(^uint64(0))
	} else {
		b.consumersMetric.Add(^uint64(0))
	}
}

func (s *session) removeWriter(id uint64) {
	s.writersMu.Lock()
	writer := s.writers[id]
	if writer != nil {
		delete(s.writers, id)
		writer.cancel()
	}
	s.writersMu.Unlock()
}

func (s *session) enqueue(id uint64, frame Frame, terminal bool) error {
	s.writersMu.Lock()
	if s.ctx.Err() != nil {
		s.writersMu.Unlock()
		return ErrClosed
	}
	writer := s.writers[id]
	if writer == nil {
		ctx, cancel := context.WithCancel(s.ctx)
		writer = &streamWriter{session: s, id: id, ctx: ctx, cancel: cancel, queue: make(chan outbound, s.broker.maxQueuedFramesPerStream)}
		s.writers[id] = writer
		go writer.run()
	}
	select {
	case writer.queue <- outbound{frame: frame, terminal: terminal}:
		s.writersMu.Unlock()
		return nil
	default:
		s.writersMu.Unlock()
		return ErrBackpressure
	}
}

func (w *streamWriter) run() {
	for {
		select {
		case <-w.ctx.Done():
			return
		case item := <-w.queue:
			if err := w.session.write(item.frame); err != nil {
				w.session.close()
				return
			}
			if item.terminal {
				w.session.finishWriter(w.id, w)
				return
			}
		}
	}
}

func (s *session) finishWriter(id uint64, writer *streamWriter) {
	s.writersMu.Lock()
	if current := s.writers[id]; current == writer {
		delete(s.writers, id)
		writer.cancel()
	}
	s.writersMu.Unlock()
}

func (s *session) write(frame Frame) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.broker.writeTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.broker.writeTimeout))
		defer func() { _ = s.conn.SetWriteDeadline(time.Time{}) }()
	}
	if err := WriteFrame(s.conn, frame, s.broker.maxFramePayload); err != nil {
		return err
	}
	if frame.Kind == KindData {
		s.broker.bytesMetric.Add(uint64(len(frame.Payload)))
		s.broker.framesMetric.Add(1)
	}
	return nil
}

func (b *Broker) emit(event Event) {
	if b.onEvent != nil {
		b.onEvent(event)
	}
	if b.logger != nil && event.Kind != "stream-data" {
		attrs := []any{"role", event.Role, "route_id", event.RouteID, "agent_id", event.AgentID}
		if event.LeaseID != "" {
			attrs = append(attrs, "lease_id", event.LeaseID)
		}
		if event.StreamID != 0 {
			attrs = append(attrs, "stream_id", event.StreamID)
		}
		if event.Connections != 0 {
			attrs = append(attrs, "connections", event.Connections)
		}
		if event.Bytes != 0 {
			attrs = append(attrs, "bytes", event.Bytes)
		}
		if event.Error != "" {
			attrs = append(attrs, "error", event.Error)
		}
		b.logger.Debug("route broker event", append([]any{"event", event.Kind}, attrs...)...)
	}
}
