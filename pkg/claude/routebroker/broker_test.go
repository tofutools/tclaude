package routebroker_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

type testAuthorizer struct {
	mu             sync.Mutex
	deadPublishers map[string]bool
	deadConsumers  map[string]bool
}

func newTestAuthorizer() *testAuthorizer {
	return &testAuthorizer{deadPublishers: make(map[string]bool), deadConsumers: make(map[string]bool)}
}

func (a *testAuthorizer) AuthorizePublisher(_ context.Context, auth routebroker.PublisherAuth) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deadPublishers[auth.RouteID] {
		return errors.New("publisher withdrawn")
	}
	return nil
}

func (a *testAuthorizer) AuthorizeConsumer(_ context.Context, auth routebroker.ConsumerAuth) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deadConsumers[auth.LeaseID] {
		return errors.New("lease closed")
	}
	return nil
}

func (a *testAuthorizer) revokeConsumer(lease string) {
	a.mu.Lock()
	a.deadConsumers[lease] = true
	a.mu.Unlock()
}

func (a *testAuthorizer) revokePublisher(route string) {
	a.mu.Lock()
	a.deadPublishers[route] = true
	a.mu.Unlock()
}

func newBroker(t *testing.T, auth *testAuthorizer, cfg routebroker.Config) *routebroker.Broker {
	t.Helper()
	cfg.Authorizer = auth
	if cfg.AuthorityCheckInterval == 0 {
		cfg.AuthorityCheckInterval = 10 * time.Millisecond
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 100 * time.Millisecond
	}
	b, err := routebroker.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b
}

type attached struct {
	pubPeer net.Conn
	conPeer net.Conn
	pubDone chan error
	conDone chan error
}

func attachPair(t *testing.T, b *routebroker.Broker, publisher routebroker.PublisherAuth, consumer routebroker.ConsumerAuth) attached {
	t.Helper()
	pubBroker, pubPeer := net.Pipe()
	conBroker, conPeer := net.Pipe()
	pair := attached{pubPeer: pubPeer, conPeer: conPeer, pubDone: make(chan error, 1), conDone: make(chan error, 1)}
	go func() { pair.pubDone <- b.AttachPublisher(context.Background(), publisher, pubBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().PublisherChannels == 1 }, time.Second, time.Millisecond)
	go func() { pair.conDone <- b.AttachConsumer(context.Background(), consumer, conBroker) }()
	t.Cleanup(func() {
		_ = pubPeer.Close()
		_ = conPeer.Close()
	})
	return pair
}

func auths() (routebroker.PublisherAuth, routebroker.ConsumerAuth) {
	return routebroker.PublisherAuth{RouteID: "route-a", AgentID: "publisher", ConvID: "publisher-conv", LaunchGeneration: "pub-launch", GroupGeneration: 7}, routebroker.ConsumerAuth{LeaseID: "lease-a", RouteID: "route-a", AgentID: "consumer", ConvID: "consumer-conv", LaunchGeneration: "con-launch", GroupGeneration: 7}
}

func readFrame(t *testing.T, conn net.Conn) routebroker.Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	frame, err := routebroker.ReadFrame(conn, routebroker.MaxFramePayload)
	require.NoError(t, err)
	return frame
}

func writeFrame(t *testing.T, conn net.Conn, frame routebroker.Frame) {
	t.Helper()
	require.NoError(t, routebroker.WriteFrame(conn, frame, routebroker.MaxFramePayload))
}

func TestBrokerForwardsOpaqueConcurrentStreamsWithSafeIDs(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 41})
	openedA := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindOpen, openedA.Kind)
	require.NotEqual(t, uint64(0), openedA.Stream)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: openedA.Stream})
	ackA := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindOpenOK, ackA.Kind)
	require.Equal(t, uint64(41), ackA.Stream)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 41, Payload: []byte("consumer-stream-a")})
	dataA := readFrame(t, pair.pubPeer)
	require.Equal(t, openedA.Stream, dataA.Stream)
	require.Equal(t, []byte("consumer-stream-a"), dataA.Payload)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: openedA.Stream, Payload: []byte("publisher-stream-a")})
	backA := readFrame(t, pair.conPeer)
	require.Equal(t, uint64(41), backA.Stream)
	require.Equal(t, []byte("publisher-stream-a"), backA.Payload)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 42})
	openedB := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindOpen, openedB.Kind)
	require.NotEqual(t, openedA.Stream, openedB.Stream, "broker IDs must discriminate concurrent streams")
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: openedB.Stream})
	require.Equal(t, routebroker.KindOpenOK, readFrame(t, pair.conPeer).Kind)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindClose, Stream: 41})
	closeA := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindClose, closeA.Kind)
	require.Equal(t, openedA.Stream, closeA.Stream)
}

func TestBrokerPreservesHalfCloseAndConsumerExit(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{WriteTimeout: 5 * time.Second})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	opened := readFrame(t, pair.pubPeer)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: opened.Stream})
	_ = readFrame(t, pair.conPeer)

	// One direction ending is not the stream ending: the reverse direction
	// still carries the publisher's reply.
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: 1})
	finToPublisher := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindHalfClose, finToPublisher.Kind)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: opened.Stream, Payload: []byte("final publisher bytes")})
	require.Equal(t, []byte("final publisher bytes"), readFrame(t, pair.conPeer).Payload)
	require.Equal(t, uint64(1), b.Metrics().Streams, "a one-sided half-close must not retire the stream")

	// The second half-close does end it. Both endpoints are told, after the
	// orderly shutdown they were owed.
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: opened.Stream})
	require.Equal(t, routebroker.KindHalfClose, readFrame(t, pair.conPeer).Kind)
	consumerReclaim := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindClose, consumerReclaim.Kind)
	require.Equal(t, uint64(1), consumerReclaim.Stream)
	publisherReclaim := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindClose, publisherReclaim.Kind)
	require.Equal(t, opened.Stream, publisherReclaim.Stream)
	require.Equal(t, uint64(0), b.Metrics().Streams)

	// A consumer process disappearing without a CLOSE still informs the
	// publisher, allowing the endpoint adapter to close its local target.
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 2})
	live := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindOpen, live.Kind)
	_ = pair.conPeer.Close()
	consumerClose := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindClose, consumerClose.Kind)
	require.Equal(t, live.Stream, consumerClose.Stream)
}

// dualHalfClose ends one stream the way an ordinary request/response does:
// both directions half-close and neither endpoint sends CLOSE. It returns once
// the broker has told both endpoints the stream was reclaimed.
func dualHalfClose(t *testing.T, pair attached, localID, globalID uint64) {
	t.Helper()
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: localID})
	fin := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindHalfClose, fin.Kind)
	require.Equal(t, globalID, fin.Stream)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: globalID})
	require.Equal(t, routebroker.KindHalfClose, readFrame(t, pair.conPeer).Kind)
	require.Equal(t, routebroker.KindClose, readFrame(t, pair.conPeer).Kind)
	require.Equal(t, routebroker.KindClose, readFrame(t, pair.pubPeer).Kind)
}

func openStream(t *testing.T, pair attached, localID uint64) uint64 {
	t.Helper()
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: localID})
	opened := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindOpen, opened.Kind)
	return opened.Stream
}

// TestBrokerDualHalfCloseReleasesOneRouteSlot pins the accounting: a completed
// stream frees exactly the budget it took. Freeing none exhausts the route one
// short-lived exchange at a time; freeing more than one would let a route carry
// more concurrent streams than its limit allows.
func TestBrokerDualHalfCloseReleasesOneRouteSlot(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxConnectionsPerRoute: 2, MaxConnectionsPerAgent: 16, WriteTimeout: 5 * time.Second})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	first := openStream(t, pair, 1)
	_ = openStream(t, pair, 2)
	require.Equal(t, uint64(2), b.Metrics().Streams)

	dualHalfClose(t, pair, 1, first)
	require.Equal(t, uint64(1), b.Metrics().Streams)

	// The freed slot is usable again...
	_ = openStream(t, pair, 3)
	// ...and only that one slot was freed: the still-live stream keeps the
	// route at its bound.
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 4})
	refused := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindOpenError, refused.Kind)
	require.Equal(t, uint64(4), refused.Stream)
	require.Contains(t, string(refused.Payload), "route connection limit")
}

// TestBrokerDualHalfCloseReleasesOneAgentSlot is the same claim for the
// broker-wide per-agent counter, which a route-limit assertion cannot see.
func TestBrokerDualHalfCloseReleasesOneAgentSlot(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxConnectionsPerRoute: 16, MaxConnectionsPerAgent: 2, WriteTimeout: 5 * time.Second})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	first := openStream(t, pair, 1)
	_ = openStream(t, pair, 2)
	dualHalfClose(t, pair, 1, first)

	_ = openStream(t, pair, 3)
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 4})
	refused := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindOpenError, refused.Kind)
	require.Contains(t, string(refused.Payload), "agent connection limit")
}

// TestBrokerSequentialShortLivedStreamsStayWithinTheRouteLimit runs far more
// completed exchanges than one route may hold at once, on a single pair of
// channels, as a short-lived-connection workload does.
func TestBrokerSequentialShortLivedStreamsStayWithinTheRouteLimit(t *testing.T) {
	const exchanges = 96

	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{WriteTimeout: 5 * time.Second})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	for i := range uint64(exchanges) {
		localID := i + 1
		globalID := openStream(t, pair, localID)
		writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: globalID})
		require.Equal(t, routebroker.KindOpenOK, readFrame(t, pair.conPeer).Kind)
		writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: localID, Payload: []byte("request")})
		require.Equal(t, []byte("request"), readFrame(t, pair.pubPeer).Payload)
		writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: globalID, Payload: []byte("reply")})
		require.Equal(t, []byte("reply"), readFrame(t, pair.conPeer).Payload)
		dualHalfClose(t, pair, localID, globalID)
	}

	require.Equal(t, uint64(0), b.Metrics().Streams)
	require.Equal(t, uint64(0), b.Metrics().RejectedStreams, "no exchange may be refused for capacity")
}

// TestBrokerLateFramesAfterDualHalfCloseStayStreamLocal proves reclamation
// keeps the bounded-tombstone contract: an endpoint that sends its own CLOSE
// after the broker already retired the stream is absorbed rather than failing
// the channel it shares with live streams.
func TestBrokerLateFramesAfterDualHalfCloseStayStreamLocal(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{WriteTimeout: 5 * time.Second})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	retired := openStream(t, pair, 1)
	live := openStream(t, pair, 2)
	dualHalfClose(t, pair, 1, retired)

	// Terminal frames both endpoints may still have in flight for the retired
	// stream are dropped, along with a late DATA from the publisher.
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindClose, Stream: 1})
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindClose, Stream: retired})
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: retired, Payload: []byte("late")})

	// The unrelated stream on the same channels is untouched.
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: live, Payload: []byte("unrelated")})
	forwarded := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindData, forwarded.Kind)
	require.Equal(t, uint64(2), forwarded.Stream)
	require.Equal(t, []byte("unrelated"), forwarded.Payload)
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 2, Payload: []byte("still open")})
	require.Equal(t, []byte("still open"), readFrame(t, pair.pubPeer).Payload)

	select {
	case err := <-pair.pubDone:
		t.Fatalf("publisher exited after late frames: %v", err)
	case err := <-pair.conDone:
		t.Fatalf("consumer exited after late frames: %v", err)
	default:
	}
	require.Equal(t, uint64(1), b.Metrics().Streams)
}

func TestBrokerSlowConsumerIsBoundedAndDoesNotBlockAnotherConsumer(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxQueuedFramesPerStream: 1, WriteTimeout: time.Second})
	publisher, slowConsumer := auths()
	pubBroker, pubPeer := net.Pipe()
	slowBroker, slowPeer := net.Pipe()
	fastBroker, fastPeer := net.Pipe()
	pubDone := make(chan error, 1)
	slowDone := make(chan error, 1)
	fastDone := make(chan error, 1)
	go func() { pubDone <- b.AttachPublisher(context.Background(), publisher, pubBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().PublisherChannels == 1 }, time.Second, time.Millisecond)
	go func() { slowDone <- b.AttachConsumer(context.Background(), slowConsumer, slowBroker) }()
	fastAuth := slowConsumer
	fastAuth.LeaseID = "lease-fast"
	fastAuth.AgentID = "fast-consumer"
	go func() { fastDone <- b.AttachConsumer(context.Background(), fastAuth, fastBroker) }()
	t.Cleanup(func() {
		_ = pubPeer.Close()
		_ = slowPeer.Close()
		_ = fastPeer.Close()
	})

	writeFrame(t, slowPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	slowOpen := readFrame(t, pubPeer)
	writeFrame(t, fastPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	fastOpen := readFrame(t, pubPeer)
	require.NotEqual(t, slowOpen.Stream, fastOpen.Stream)

	// The slow peer never reads its OPEN_OK. Its stream writer is therefore
	// blocked in a deadline-bounded socket write, but the broker's publisher
	// reader and the other consumer writer continue independently.
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: slowOpen.Stream})
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: fastOpen.Stream})
	fastOK := readFrame(t, fastPeer)
	require.Equal(t, routebroker.KindOpenOK, fastOK.Kind, "slow consumer must not block an unrelated consumer")
	require.Equal(t, uint64(1), fastOK.Stream)

	// A second frame exceeds the one-frame queue while the slow write remains
	// blocked. The slow channel is failed closed; the publisher channel stays
	// usable and its route memory remains bounded.
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: slowOpen.Stream, Payload: []byte("bounded-1")})
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: slowOpen.Stream, Payload: []byte("bounded-2")})
	require.Eventually(t, func() bool { return b.Metrics().RejectedStreams > 0 }, time.Second, 5*time.Millisecond)
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: fastOpen.Stream, Payload: []byte("fast-after-slow")})
	fastData := readFrame(t, fastPeer)
	require.Equal(t, []byte("fast-after-slow"), fastData.Payload)
}

func TestBrokerAuthorityRevocationAndShutdownFailClosed(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{AuthorityCheckInterval: 5 * time.Millisecond})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)
	auth.revokeConsumer(consumer.LeaseID)
	require.Eventually(t, func() bool {
		_ = pair.conPeer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := pair.conPeer.Read(make([]byte, 1))
		return err != nil
	}, time.Second, 5*time.Millisecond)
	select {
	case <-pair.conDone:
	case <-time.After(time.Second):
		t.Fatal("revoked consumer did not leave")
	}

	// Publisher withdrawal closes all remaining route channels, even when
	// the consumer's own lease has not yet been revoked.
	auth2 := newTestAuthorizer()
	b2 := newBroker(t, auth2, routebroker.Config{AuthorityCheckInterval: 5 * time.Millisecond})
	publisher2, consumer2 := auths()
	pair2 := attachPair(t, b2, publisher2, consumer2)
	auth2.revokePublisher(publisher2.RouteID)
	require.Eventually(t, func() bool {
		_ = pair2.conPeer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		_, err := pair2.conPeer.Read(make([]byte, 1))
		return err != nil
	}, time.Second, 5*time.Millisecond)

	// Close is the daemon shutdown seam and is idempotent.
	require.NoError(t, b2.Close())
	require.NoError(t, b2.Close())
	select {
	case <-pair2.pubDone:
	case <-time.After(time.Second):
		t.Fatal("publisher did not leave on broker shutdown")
	}
}

func TestConsumerCapacityAdmissionPrecedesReadyCallback(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxConsumersPerRoute: 1})
	firstBroker, firstChannel := net.Pipe()
	firstReady := make(chan struct{})
	firstAuth := routebroker.ConsumerAuth{LeaseID: "lease-first", RouteID: "route-capacity", AgentID: "consumer-first", ConvID: "consumer-first-conv", LaunchGeneration: "launch-first", GroupGeneration: 1}
	go func() {
		_ = b.AttachConsumerWithReady(context.Background(), firstAuth, firstBroker, func() error {
			close(firstReady)
			return nil
		})
	}()
	require.Eventually(t, func() bool { return b.Metrics().ConsumerChannels == 1 }, time.Second, time.Millisecond)
	select {
	case <-firstReady:
	case <-time.After(time.Second):
		t.Fatal("first consumer was not admitted")
	}

	secondBroker, secondPeer := net.Pipe()
	defer secondPeer.Close()
	secondReady := false
	secondAuth := firstAuth
	secondAuth.LeaseID = "lease-second"
	secondAuth.AgentID = "consumer-second"
	err := b.AttachConsumerWithReady(context.Background(), secondAuth, secondBroker, func() error {
		secondReady = true
		return nil
	})
	require.ErrorIs(t, err, routebroker.ErrConsumerLimit)
	require.False(t, secondReady, "capacity refusal must happen before the ready callback")
	require.Equal(t, uint64(1), b.Metrics().ConsumerChannels)
	_ = firstChannel.Close()
}

func TestBrokerEnforcesRouteAndAgentConnectionBounds(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxConnectionsPerRoute: 1, MaxConnectionsPerAgent: 1})
	publisher, consumer := auths()
	pair := attachPair(t, b, publisher, consumer)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	opened := readFrame(t, pair.pubPeer)
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 2})
	rejected := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindOpenError, rejected.Kind)
	require.Equal(t, uint64(2), rejected.Stream)
	require.Contains(t, string(rejected.Payload), "route connection limit")

	// The first connection is still active, so the bound remains visible in
	// metadata and no second publisher OPEN was emitted.
	require.Equal(t, uint64(1), b.Metrics().Streams)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindClose, Stream: opened.Stream})
	require.Equal(t, routebroker.KindClose, readFrame(t, pair.conPeer).Kind)
}

func TestBrokerLateFramesAfterCloseStayStreamLocal(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{})
	publisher, consumerA := auths()
	consumerB := consumerA
	consumerB.LeaseID = "lease-b"
	consumerB.AgentID = "consumer-b"

	pubBroker, pubPeer := net.Pipe()
	consumerABroker, consumerAPeer := net.Pipe()
	consumerBBroker, consumerBPeer := net.Pipe()
	pubDone := make(chan error, 1)
	consumerADone := make(chan error, 1)
	consumerBDone := make(chan error, 1)
	go func() { pubDone <- b.AttachPublisher(context.Background(), publisher, pubBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().PublisherChannels == 1 }, time.Second, time.Millisecond)
	go func() { consumerADone <- b.AttachConsumer(context.Background(), consumerA, consumerABroker) }()
	go func() { consumerBDone <- b.AttachConsumer(context.Background(), consumerB, consumerBBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().ConsumerChannels == 2 }, time.Second, time.Millisecond)
	t.Cleanup(func() {
		_ = pubPeer.Close()
		_ = consumerAPeer.Close()
		_ = consumerBPeer.Close()
	})

	writeFrame(t, consumerAPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	openedA := readFrame(t, pubPeer)
	writeFrame(t, consumerBPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	openedB := readFrame(t, pubPeer)
	require.NotEqual(t, openedA.Stream, openedB.Stream)

	// Closing A removes its live stream immediately, but its bounded tombstone
	// absorbs a DATA frame already in flight from the opposite direction.
	writeFrame(t, consumerAPeer, routebroker.Frame{Kind: routebroker.KindClose, Stream: 1})
	require.Equal(t, routebroker.KindClose, readFrame(t, pubPeer).Kind)
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: openedA.Stream, Payload: []byte("late")})
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: openedB.Stream, Payload: []byte("unrelated")})
	forwarded := readFrame(t, consumerBPeer)
	require.Equal(t, routebroker.KindData, forwarded.Kind)
	require.Equal(t, uint64(1), forwarded.Stream)
	require.Equal(t, []byte("unrelated"), forwarded.Payload)

	// The publisher reader is still healthy after the late frame, and the
	// unrelated consumer remains attached.
	writeFrame(t, pubPeer, routebroker.Frame{Kind: routebroker.KindPing})
	require.Equal(t, routebroker.KindPong, readFrame(t, pubPeer).Kind)
	select {
	case err := <-pubDone:
		t.Fatalf("publisher exited after late frame: %v", err)
	default:
	}
	select {
	case err := <-consumerBDone:
		t.Fatalf("unrelated consumer exited after late frame: %v", err)
	default:
	}
}

func TestBrokerAgentConnectionBoundIsBrokerWide(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{MaxConnectionsPerRoute: 4, MaxConnectionsPerAgent: 1})
	publisherA, consumerA := auths()
	publisherB := publisherA
	publisherB.RouteID = "route-b"
	publisherB.ConvID = "publisher-b-conv"
	consumerB := consumerA
	consumerB.RouteID = "route-b"
	consumerB.LeaseID = "lease-b"

	pubABroker, pubAPeer := net.Pipe()
	pubBBroker, pubBPeer := net.Pipe()
	consumerABroker, consumerAPeer := net.Pipe()
	consumerBBroker, consumerBPeer := net.Pipe()
	otherBroker, otherPeer := net.Pipe()
	pubADone := make(chan error, 1)
	pubBDone := make(chan error, 1)
	consumerADone := make(chan error, 1)
	consumerBDone := make(chan error, 1)
	otherDone := make(chan error, 1)
	go func() { pubADone <- b.AttachPublisher(context.Background(), publisherA, pubABroker) }()
	go func() { pubBDone <- b.AttachPublisher(context.Background(), publisherB, pubBBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().PublisherChannels == 2 }, time.Second, time.Millisecond)
	go func() { consumerADone <- b.AttachConsumer(context.Background(), consumerA, consumerABroker) }()
	go func() { consumerBDone <- b.AttachConsumer(context.Background(), consumerB, consumerBBroker) }()
	other := consumerB
	other.LeaseID = "lease-other"
	other.AgentID = "other-consumer"
	go func() { otherDone <- b.AttachConsumer(context.Background(), other, otherBroker) }()
	require.Eventually(t, func() bool { return b.Metrics().ConsumerChannels == 3 }, time.Second, time.Millisecond)
	t.Cleanup(func() {
		_ = pubAPeer.Close()
		_ = pubBPeer.Close()
		_ = consumerAPeer.Close()
		_ = consumerBPeer.Close()
		_ = otherPeer.Close()
	})

	writeFrame(t, consumerAPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	openedA := readFrame(t, pubAPeer)
	require.Equal(t, routebroker.KindOpen, openedA.Kind)

	// The same consumer agent is already at the broker-wide cap on route A.
	writeFrame(t, consumerBPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	rejected := readFrame(t, consumerBPeer)
	require.Equal(t, routebroker.KindOpenError, rejected.Kind)
	require.Contains(t, string(rejected.Payload), "agent connection limit")

	// A different agent can still open on route B, proving the counter is
	// global per agent rather than a shared global capacity.
	writeFrame(t, otherPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	openedOther := readFrame(t, pubBPeer)
	require.Equal(t, routebroker.KindOpen, openedOther.Kind)
}

func TestBrokerAttachPublisherReadySignalsEveryOutcome(t *testing.T) {
	auth := newTestAuthorizer()
	b := newBroker(t, auth, routebroker.Config{})
	publisher, _ := auths()

	ready := make(chan error, 1)
	require.Error(t, b.AttachPublisherReady(context.Background(), publisher, nil, func(err error) { ready <- err }))
	require.Error(t, <-ready)

	// Success signals nil, and only after the route is owned: a consumer that
	// opens the instant ready fires must not be told the publisher is missing.
	pubBroker, pubPeer := net.Pipe()
	t.Cleanup(func() { _ = pubPeer.Close() })
	attachDone := make(chan error, 1)
	go func() {
		attachDone <- b.AttachPublisherReady(context.Background(), publisher, pubBroker, func(err error) { ready <- err })
	}()
	require.NoError(t, <-ready)
	require.Equal(t, uint64(1), b.Metrics().PublisherChannels)

	// A second publisher on the same route is refused through ready.
	dupBroker, dupPeer := net.Pipe()
	t.Cleanup(func() { _ = dupPeer.Close() })
	require.ErrorIs(t, b.AttachPublisherReady(context.Background(), publisher, dupBroker, func(err error) { ready <- err }), routebroker.ErrPublisherAttached)
	require.ErrorIs(t, <-ready, routebroker.ErrPublisherAttached)

	// A refused authority reports through ready rather than failing silently.
	auth.revokePublisher("route-b")
	refusedBroker, refusedPeer := net.Pipe()
	t.Cleanup(func() { _ = refusedPeer.Close() })
	refused := publisher
	refused.RouteID = "route-b"
	require.ErrorIs(t, b.AttachPublisherReady(context.Background(), refused, refusedBroker, func(err error) { ready <- err }), routebroker.ErrUnauthorized)
	require.ErrorIs(t, <-ready, routebroker.ErrUnauthorized)

	_ = pubPeer.Close()
	require.NoError(t, <-attachDone)
}

func TestBrokerAttachPublisherReadyReportsClosedBroker(t *testing.T) {
	b, err := routebroker.New(routebroker.Config{Authorizer: newTestAuthorizer()})
	require.NoError(t, err)
	require.NoError(t, b.Close())
	conn, peer := net.Pipe()
	defer peer.Close()
	ready := make(chan error, 1)
	require.ErrorIs(t, b.AttachPublisherReady(context.Background(), routebroker.PublisherAuth{RouteID: "route-a"}, conn, func(err error) { ready <- err }), routebroker.ErrClosed)
	require.ErrorIs(t, <-ready, routebroker.ErrClosed)
}
