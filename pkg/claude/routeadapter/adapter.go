// Package routeadapter contains the Darwin raw-TCP endpoint bridge for the
// platform-neutral group-route broker. It owns listeners and slot leases, but
// receives all route authority through routebroker.Authorizer.
package routeadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

const maxPayload = routebroker.MaxFramePayload

// attachBarrierTimeout bounds how long Publish waits for the broker to take
// ownership of the route. It only has to cover the authority's local checks;
// anything slower is a stalled authority, not a slow publish.
const attachBarrierTimeout = 10 * time.Second

type Publisher struct {
	RouteID          string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupGeneration  int64
	Target           string
}

type Consumer struct {
	LeaseID          string
	RouteID          string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupGeneration  int64
}

type Adapter struct {
	broker          *routebroker.Broker
	mu              sync.Mutex
	ports           []int
	used            map[string]int
	routes          map[string]*publisherState
	leases          map[string]*consumerState
	consumerRefused func(Consumer, error)
	closed          bool
}

type publisherState struct {
	port   int
	conn   net.Conn
	cancel context.CancelFunc
}

type consumerState struct {
	port     int
	routeID  string
	agentID  string
	listener net.Listener
	cancel   context.CancelFunc
}

func New(broker *routebroker.Broker, ports []int) (*Adapter, error) {
	if broker == nil {
		return nil, errors.New("route adapter requires a broker")
	}
	if len(ports) > 0 {
		if err := validatePorts(ports); err != nil {
			return nil, err
		}
	}
	return &Adapter{
		broker: broker,
		ports:  append([]int(nil), ports...),
		used:   make(map[string]int, len(ports)),
		routes: make(map[string]*publisherState),
		leases: make(map[string]*consumerState),
	}, nil
}

func validatePorts(ports []int) error {
	if len(ports) == 0 || len(ports) > 16 {
		return fmt.Errorf("route adapter requires 1–16 exact ports")
	}
	seen := make(map[int]struct{}, len(ports))
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("route adapter port %d is outside TCP range", port)
		}
		if _, ok := seen[port]; ok {
			return fmt.Errorf("route adapter port %d is duplicated", port)
		}
		seen[port] = struct{}{}
	}
	return nil
}

func targetPort(target string) (int, error) {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u.Scheme != "tcp" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return 0, fmt.Errorf("route target must be tcp://127.0.0.1:<port>")
	}
	host, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, fmt.Errorf("route target must include a TCP port: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.Is4() || !ip.IsLoopback() || ip.IsUnspecified() {
		return 0, fmt.Errorf("route target must use an IPv4 loopback address")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("route target has invalid TCP port %q", portText)
	}
	return port, nil
}

func (a *Adapter) acquire(key string, requested int, pool []int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return 0, errors.New("route adapter is closed")
	}
	if old, ok := a.used[key]; ok {
		if requested != 0 && requested != old {
			return 0, fmt.Errorf("route key %q already owns slot %d", key, old)
		}
		return old, nil
	}
	if requested != 0 {
		for _, port := range pool {
			if port == requested {
				for owner, used := range a.used {
					if used == requested {
						return 0, fmt.Errorf("route slot %d is already leased by %s", requested, owner)
					}
				}
				a.used[key] = requested
				return requested, nil
			}
		}
		return 0, fmt.Errorf("route slot %d is not in the pre-authorized pool", requested)
	}
	for _, port := range pool {
		occupied := false
		for _, used := range a.used {
			if used == port {
				occupied = true
				break
			}
		}
		if !occupied {
			a.used[key] = port
			return port, nil
		}
	}
	return 0, errors.New("route adapter slot pool exhausted")
}

func (a *Adapter) release(key string) {
	a.mu.Lock()
	delete(a.used, key)
	a.mu.Unlock()
}

// SetConsumerRefusalObserver registers the seam that receives the reason a
// consumer stream was never admitted. Consumer streams have no caller waiting
// on them — each one is an accepted local TCP connection, which can carry no
// structured reason back — so without this seam a broker refusal is
// indistinguishable from an ordinary peer disconnect. The daemon uses it to
// move the refusal onto the durable, agent-visible lease state, the same place
// the Linux channel handler records its own refusals.
//
// The observer runs on the refused stream's goroutine with no adapter lock
// held, so it may call back into CloseLease.
func (a *Adapter) SetConsumerRefusalObserver(observe func(Consumer, error)) {
	a.mu.Lock()
	a.consumerRefused = observe
	a.mu.Unlock()
}

func (a *Adapter) reportConsumerRefusal(consumer Consumer, err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	observe := a.consumerRefused
	a.mu.Unlock()
	if observe != nil {
		observe(consumer, err)
	}
}

// Publish attaches a daemon-owned publisher channel. The target application
// remains responsible for binding the exact pre-authorized target slot.
func (a *Adapter) Publish(ctx context.Context, publisher Publisher) (int, error) {
	return a.publish(ctx, publisher, a.ports)
}

// PublishWithSlots attaches a publisher only when its target is in the exact
// slot pool registered for that launch generation.
func (a *Adapter) PublishWithSlots(ctx context.Context, publisher Publisher, slots []int) (int, error) {
	if err := validatePorts(slots); err != nil {
		return 0, err
	}
	return a.publish(ctx, publisher, slots)
}

func (a *Adapter) publish(ctx context.Context, publisher Publisher, pool []int) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(publisher.RouteID) == "" {
		return 0, errors.New("publisher route ID is required")
	}
	port, err := targetPort(publisher.Target)
	if err != nil {
		return 0, err
	}
	if _, err := a.acquire(publisher.RouteID, port, pool); err != nil {
		return 0, err
	}
	channel, peer := net.Pipe()
	channelCtx, cancel := context.WithCancel(ctx)
	state := &publisherState{port: port, conn: channel, cancel: cancel}
	a.mu.Lock()
	if _, exists := a.routes[publisher.RouteID]; exists {
		a.mu.Unlock()
		cancel()
		_ = channel.Close()
		a.release(publisher.RouteID)
		return 0, fmt.Errorf("route %q publisher already attached", publisher.RouteID)
	}
	a.routes[publisher.RouteID] = state
	a.mu.Unlock()
	ready := make(chan error, 1)
	go func() {
		_ = a.broker.AttachPublisherReady(channelCtx, routebroker.PublisherAuth{
			RouteID: publisher.RouteID, AgentID: publisher.AgentID, ConvID: publisher.ConvID,
			LaunchGeneration: publisher.LaunchGeneration, GroupGeneration: publisher.GroupGeneration,
		}, peer, func(err error) { ready <- err })
		// A publisher channel ending is authoritative for the endpoint
		// lifetime. Close idle listeners as well as any active streams; no M2
		// consumer event is required for this cleanup.
		a.CloseRoute(publisher.RouteID)
	}()
	go a.publisherLoop(channelCtx, channel, publisher.Target)
	// Publish only reports success once the broker owns the route. Returning
	// earlier lets a consumer that opens immediately race the attach goroutine
	// and be refused with "publisher unavailable". The wait is bounded on its
	// own timer: callers deliberately pass a channel-lifetime context, so a
	// stalled authority must fail this call rather than hold it open.
	timer := time.NewTimer(attachBarrierTimeout)
	defer timer.Stop()
	if err := awaitAttach(channelCtx, ready, timer.C); err != nil {
		a.CloseRoute(publisher.RouteID)
		return 0, fmt.Errorf("attach publisher route %q: %w", publisher.RouteID, err)
	}
	return port, nil
}

// awaitAttach resolves the publish barrier, returning nil once the broker owns
// the route. The attach goroutine cancels the channel context itself as soon
// as AttachPublisherReady returns, so a failed attach and the cancellation it
// triggers become observable at the same instant and a plain select would pick
// between them at random. The ready send happens before that cancel, so a
// result already queued when the context fires is the real reason and wins
// over the bare context error: a caller told "context canceled" cannot tell an
// authority refusal apart from an ordinary cancellation, and those two warrant
// opposite handling — retrying a cancelled attach is reasonable, retrying a
// refused one is not.
//
// The two fallback arms deliberately disagree about a queued success and must
// not be merged back into one drain: a cancelled context means the route is
// already being torn down, while a fired timer tears nothing down and can find
// a perfectly healthy route on the other side of it.
func awaitAttach(ctx context.Context, ready <-chan error, timeout <-chan time.Time) error {
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		// Only a queued failure beats the context error here. A queued success
		// does not rescue the route: it is going away regardless, so the
		// publish fails either way.
		if err := queuedAttachFailure(ready); err != nil {
			return err
		}
		return ctx.Err()
	case <-timeout:
		// Any queued result is the whole truth here — including a success,
		// which means the broker owns a live route and reporting a stalled
		// authority would both lie and withdraw a working route.
		select {
		case err := <-ready:
			return err
		default:
			return context.DeadlineExceeded
		}
	}
}

// queuedAttachFailure reports a failure the attach goroutine has already
// delivered, and nil when it has delivered nothing or delivered success.
func queuedAttachFailure(ready <-chan error) error {
	select {
	case err := <-ready:
		return err
	default:
		return nil
	}
}

// Open creates the broker-held consumer listener and returns its exact local
// endpoint. The listener is outside the sandbox; the consumer process only
// needs outbound permission for this pre-authorized slot.
func (a *Adapter) Open(ctx context.Context, consumer Consumer) (string, error) {
	return a.open(ctx, consumer, a.ports)
}

// OpenWithSlots creates a consumer listener using only the exact pool owned
// by the consumer launch generation.
func (a *Adapter) OpenWithSlots(ctx context.Context, consumer Consumer, slots []int) (string, error) {
	if err := validatePorts(slots); err != nil {
		return "", err
	}
	return a.open(ctx, consumer, slots)
}

func (a *Adapter) open(ctx context.Context, consumer Consumer, pool []int) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(consumer.LeaseID) == "" || strings.TrimSpace(consumer.RouteID) == "" {
		return "", errors.New("consumer lease and route IDs are required")
	}
	port, err := a.acquire(consumer.LeaseID, 0, pool)
	if err != nil {
		return "", err
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		a.release(consumer.LeaseID)
		return "", fmt.Errorf("bind consumer route slot %d: %w", port, err)
	}
	channelCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.leases[consumer.LeaseID] = &consumerState{port: port, routeID: consumer.RouteID, agentID: consumer.AgentID, listener: listener, cancel: cancel}
	a.mu.Unlock()
	go a.acceptConsumers(channelCtx, listener, consumer)
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}

func (a *Adapter) CloseRoute(routeID string) {
	a.mu.Lock()
	state := a.routes[routeID]
	delete(a.routes, routeID)
	leaseIDs := make([]string, 0)
	for leaseID, lease := range a.leases {
		if lease.routeID == routeID {
			leaseIDs = append(leaseIDs, leaseID)
		}
	}
	a.mu.Unlock()
	if state != nil {
		state.cancel()
		_ = state.conn.Close()
	}
	for _, leaseID := range leaseIDs {
		a.CloseLease(leaseID)
	}
	a.release(routeID)
}

// CloseConsumer drops endpoint listeners owned by one consumer identity after
// the M1 authority revokes that consumer's lease. Route IDs are included so a
// stale agent event cannot tear down a same-agent lease on another route.
func (a *Adapter) CloseConsumer(routeID, agentID string) {
	a.mu.Lock()
	leaseIDs := make([]string, 0)
	for leaseID, lease := range a.leases {
		if lease.routeID == routeID && lease.agentID == agentID {
			leaseIDs = append(leaseIDs, leaseID)
		}
	}
	a.mu.Unlock()
	for _, leaseID := range leaseIDs {
		a.CloseLease(leaseID)
	}
}

func (a *Adapter) CloseLease(leaseID string) {
	a.mu.Lock()
	state := a.leases[leaseID]
	delete(a.leases, leaseID)
	a.mu.Unlock()
	if state != nil {
		state.cancel()
		_ = state.listener.Close()
	}
	a.release(leaseID)
}

func (a *Adapter) Close() {
	a.mu.Lock()
	a.closed = true
	routes := make([]string, 0, len(a.routes))
	for id := range a.routes {
		routes = append(routes, id)
	}
	leases := make([]string, 0, len(a.leases))
	for id := range a.leases {
		leases = append(leases, id)
	}
	a.mu.Unlock()
	for _, id := range routes {
		a.CloseRoute(id)
	}
	for _, id := range leases {
		a.CloseLease(id)
	}
}

// RouteIDs and LeaseIDs are snapshots used by the generation reconciler. The
// adapter never infers authority from these maps; agentd compares them with
// durable M1 rows before closing stale listeners.
func (a *Adapter) RouteIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.routes))
	for id := range a.routes {
		ids = append(ids, id)
	}
	return ids
}

func (a *Adapter) LeaseIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.leases))
	for id := range a.leases {
		ids = append(ids, id)
	}
	return ids
}

func (a *Adapter) acceptConsumers(ctx context.Context, listener net.Listener, consumer Consumer) {
	defer func() {
		a.mu.Lock()
		delete(a.leases, consumer.LeaseID)
		a.mu.Unlock()
		a.release(consumer.LeaseID)
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go a.consumerStream(ctx, conn, consumer)
	}
}

func (a *Adapter) consumerStream(ctx context.Context, raw net.Conn, consumer Consumer) {
	defer raw.Close()
	brokerConn, adapterConn := net.Pipe()
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	admitted := make(chan struct{})
	refused := make(chan error, 1)
	go func() {
		accepted := false
		err := a.broker.AttachConsumerWithReady(streamCtx, routebroker.ConsumerAuth{
			LeaseID: consumer.LeaseID, RouteID: consumer.RouteID, AgentID: consumer.AgentID,
			ConvID: consumer.ConvID, LaunchGeneration: consumer.LaunchGeneration,
			GroupGeneration: consumer.GroupGeneration,
		}, brokerConn, func() error {
			accepted = true
			close(admitted)
			return nil
		})
		// Only a failure before the ready callback is a refusal: it means the
		// broker never reserved a consumer slot for this stream. Anything after
		// admission is an ordinary stream ending, which the copy loops below
		// already observe. accepted is written and read on this goroutine only.
		if !accepted {
			refused <- err
		}
	}()
	// The stream must not be proxied before the broker owns it. A refused
	// attach closes the broker end of the pipe, so the old unconditional open
	// frame died of a closed pipe and reported nothing; waiting here keeps the
	// reason instead of the symptom.
	select {
	case <-admitted:
	case err := <-refused:
		a.reportConsumerRefusal(consumer, err)
		_ = adapterConn.Close()
		return
	case <-streamCtx.Done():
		// Teardown is external here — unlike the publish barrier, nothing on
		// the attach goroutine cancels this context — so a queued refusal is
		// not ordered against it and this drain is only best effort: it reports
		// a reason that already arrived rather than waiting for one.
		select {
		case err := <-refused:
			a.reportConsumerRefusal(consumer, err)
		default:
		}
		_ = adapterConn.Close()
		return
	}
	if err := routebroker.WriteFrame(adapterConn, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1}, maxPayload); err != nil {
		_ = adapterConn.Close()
		return
	}
	type directionResult struct {
		rawToBroker bool
		err         error
	}
	results := make(chan directionResult, 2)
	go func() { results <- directionResult{rawToBroker: true, err: copyRawToBroker(raw, adapterConn)} }()
	go func() { results <- directionResult{err: copyBrokerToRaw(adapterConn, raw)} }()
	first := <-results
	// A local client CloseWrite is only a read-side EOF. Keep the broker
	// channel and accepted listener alive for the publisher's reverse data.
	if first.rawToBroker && errors.Is(first.err, io.EOF) {
		<-results
		_ = raw.Close()
		_ = adapterConn.Close()
		return
	}
	_ = raw.Close()
	_ = adapterConn.Close()
	<-results
}

func copyRawToBroker(raw net.Conn, broker net.Conn) error {
	buf := make([]byte, maxPayload)
	for {
		n, err := raw.Read(buf)
		if n > 0 {
			if writeErr := routebroker.WriteFrame(broker, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: append([]byte(nil), buf[:n]...)}, maxPayload); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				_ = routebroker.WriteFrame(broker, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: 1}, maxPayload)
			}
			return err
		}
	}
}

func copyBrokerToRaw(broker net.Conn, raw net.Conn) error {
	for {
		frame, err := routebroker.ReadFrame(broker, maxPayload)
		if err != nil {
			return err
		}
		switch frame.Kind {
		case routebroker.KindOpenOK:
		case routebroker.KindData:
			if _, err := raw.Write(frame.Payload); err != nil {
				return err
			}
		case routebroker.KindHalfClose:
			if tcp, ok := raw.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		case routebroker.KindClose, routebroker.KindOpenError:
			return io.EOF
		default:
			return fmt.Errorf("consumer adapter received unexpected broker frame %d", frame.Kind)
		}
	}
}

type publisherStream struct {
	conn net.Conn
}

func (a *Adapter) publisherLoop(ctx context.Context, brokerConn net.Conn, target string) {
	defer brokerConn.Close()
	var writeMu sync.Mutex
	streams := make(map[uint64]*publisherStream)
	var streamsMu sync.Mutex
	closeStreams := func() {
		streamsMu.Lock()
		defer streamsMu.Unlock()
		for id, stream := range streams {
			_ = stream.conn.Close()
			delete(streams, id)
		}
	}
	defer closeStreams()
	for {
		frame, err := routebroker.ReadFrame(brokerConn, maxPayload)
		if err != nil {
			return
		}
		switch frame.Kind {
		case routebroker.KindOpen:
			dialer := net.Dialer{Timeout: 5 * time.Second}
			conn, dialErr := dialer.DialContext(ctx, "tcp4", targetAddress(target))
			if dialErr != nil {
				writeMu.Lock()
				_ = routebroker.WriteFrame(brokerConn, routebroker.Frame{Kind: routebroker.KindOpenError, Stream: frame.Stream, Payload: []byte(routebroker.OpenErrorTargetUnavailable)}, maxPayload)
				writeMu.Unlock()
				continue
			}
			streamsMu.Lock()
			streams[frame.Stream] = &publisherStream{conn: conn}
			streamsMu.Unlock()
			writeMu.Lock()
			err = routebroker.WriteFrame(brokerConn, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: frame.Stream}, maxPayload)
			writeMu.Unlock()
			if err != nil {
				return
			}
			go a.publisherStreamReader(ctx, brokerConn, &writeMu, &streamsMu, streams, frame.Stream, conn)
		case routebroker.KindData, routebroker.KindHalfClose, routebroker.KindClose:
			streamsMu.Lock()
			stream := streams[frame.Stream]
			streamsMu.Unlock()
			if stream == nil {
				continue
			}
			switch frame.Kind {
			case routebroker.KindData:
				if _, err := stream.conn.Write(frame.Payload); err != nil {
					_ = stream.conn.Close()
				}
			case routebroker.KindHalfClose:
				if tcp, ok := stream.conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			case routebroker.KindClose:
				_ = stream.conn.Close()
			}
		case routebroker.KindPing:
			writeMu.Lock()
			err = routebroker.WriteFrame(brokerConn, routebroker.Frame{Kind: routebroker.KindPong}, maxPayload)
			writeMu.Unlock()
			if err != nil {
				return
			}
		case routebroker.KindPong:
			// Keepalive responses carry no stream data.
		default:
			return
		}
	}
}

func targetAddress(target string) string {
	u, _ := url.Parse(target)
	return u.Host
}

func (a *Adapter) publisherStreamReader(ctx context.Context, brokerConn net.Conn, writeMu, streamsMu *sync.Mutex, streams map[uint64]*publisherStream, id uint64, conn net.Conn) {
	buf := make([]byte, maxPayload)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, err := conn.Read(buf)
		if n > 0 {
			writeMu.Lock()
			writeErr := routebroker.WriteFrame(brokerConn, routebroker.Frame{Kind: routebroker.KindData, Stream: id, Payload: append([]byte(nil), buf[:n]...)}, maxPayload)
			writeMu.Unlock()
			if writeErr != nil {
				return
			}
		}
		if err != nil {
			writeMu.Lock()
			// EOF on the publisher target is a read-side half-close. The
			// consumer may still send reverse-direction data until M2 closes
			// the stream explicitly.
			_ = routebroker.WriteFrame(brokerConn, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: id}, maxPayload)
			writeMu.Unlock()
			return
		}
	}
}
