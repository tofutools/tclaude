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
	flowWindow      func() int
	closed          bool
}

// SetFlowWindow installs the source of the flow-control receive window for
// channels attached from now on; it returns 0 when flow control is off.
// Without it the adapter's channels do not use flow control.
func (a *Adapter) SetFlowWindow(window func() int) {
	a.mu.Lock()
	a.flowWindow = window
	a.mu.Unlock()
}

func (a *Adapter) window() int {
	a.mu.Lock()
	fn := a.flowWindow
	a.mu.Unlock()
	if fn == nil {
		return 0
	}
	if w := fn(); w > 0 {
		return routebroker.ClampWindow(w)
	}
	return 0
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
// consumer stream was never admitted.
//
// This is a seam rather than a returned error because there is no caller to
// return one to, and there cannot be one: a consumer stream is an accepted raw
// local TCP connection that can carry no structured reason back, and Open
// returned long before it was dialed. Without this seam a broker refusal is
// indistinguishable from an ordinary peer disconnect. The daemon uses it to
// move the refusal onto the durable, agent-visible lease state, the same place
// the Linux channel handler records its own refusals.
//
// Refusing the stream itself does not depend on an observer being installed:
// an unobserved refusal is still closed rather than proxied.
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
	window := a.window()
	go func() {
		_ = a.broker.AttachPublisherReady(channelCtx, routebroker.PublisherAuth{
			RouteID: publisher.RouteID, AgentID: publisher.AgentID, ConvID: publisher.ConvID,
			LaunchGeneration: publisher.LaunchGeneration, GroupGeneration: publisher.GroupGeneration,
			FlowWindow: window,
		}, peer, func(err error) { ready <- err })
		// A publisher channel ending is authoritative for the endpoint
		// lifetime. Close idle listeners as well as any active streams; no M2
		// consumer event is required for this cleanup.
		a.CloseRoute(publisher.RouteID)
	}()
	// The same publisher endpoint the Linux helper runs, here in agentd:
	// each stream dials the target off the channel's read loop and writes
	// it through its own pump, so one slow target stalls only its stream.
	go func() { _ = RunPublisher(channelCtx, flowChannel{Conn: channel, window: window}, publisher.Target) }()
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
	window := a.window()
	admitted := make(chan struct{})
	refused := make(chan error, 1)
	go func() {
		accepted := false
		err := a.broker.AttachConsumerWithReady(streamCtx, routebroker.ConsumerAuth{
			LeaseID: consumer.LeaseID, RouteID: consumer.RouteID, AgentID: consumer.AgentID,
			ConvID: consumer.ConvID, LaunchGeneration: consumer.LaunchGeneration,
			GroupGeneration: consumer.GroupGeneration, FlowWindow: window,
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
	//
	// These two arms are exhaustive, and deliberately have no context arm.
	// Cancellation is not a third outcome: it makes the attach return, and that
	// return lands on refused. Racing a context arm against it would lose the
	// reason precisely when it matters most — an authority refusal is emitted
	// synchronously as a consumer-rejected event, and the daemon's handler for
	// that event closes the lease, so this very stream's context is cancelled
	// from the attach goroutine before it reports. The refusal that fires the
	// cancel would then routinely lose to the cancel it fired.
	select {
	case <-admitted:
	case err := <-refused:
		// These two lines look like a pair and are not: the close is
		// deliberately OUTSIDE the observer. reportConsumerRefusal carries only
		// the reason and does nothing when no observer is installed, so folding
		// the close into it would make a nil observer silently proxy a stream
		// the broker refused — the original bug restored behind a seam that
		// makes it look handled. Disposal must not depend on anyone listening.
		a.reportConsumerRefusal(consumer, err)
		_ = adapterConn.Close()
		return
	}
	w := &connWriter{conn: adapterConn}
	if err := w.write(routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1}); err != nil {
		_ = adapterConn.Close()
		return
	}
	type directionResult struct {
		rawToBroker bool
		err         error
	}
	// opened carries the stream's flow-controlled pump (nil without flow
	// control) once OPEN_OK arrives, and is closed without one if the open
	// fails. Local bytes are not forwarded before it: credit only exists
	// once the broker has said whether the stream is flow-controlled.
	opened := make(chan *helperPublisherStream, 1)
	results := make(chan directionResult, 2)
	go func() { results <- directionResult{rawToBroker: true, err: copyRawToBroker(raw, w, opened)} }()
	go func() { results <- directionResult{err: copyBrokerToRaw(adapterConn, raw, w, window, opened)} }()
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

func copyRawToBroker(raw net.Conn, w *connWriter, opened <-chan *helperPublisherStream) error {
	out, ok := <-opened
	if !ok {
		return net.ErrClosed
	}
	var flow *streamFlow
	if out != nil {
		flow = out.flow
	}
	buf := make([]byte, maxPayload)
	for {
		n, err := flow.read(raw, buf)
		if n > 0 {
			if writeErr := w.write(routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: append([]byte(nil), buf[:n]...)}); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if out != nil {
					out.markHalfCloseSent()
				}
				_ = w.write(routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: 1})
			}
			return err
		}
	}
}

// copyBrokerToRaw delivers the broker's side of the stream. On a
// flow-controlled stream the local socket is written by a pump, never by
// this loop, so the broker's writes to the channel are never held up by a
// slow local reader; the pump grants credit back as the reader takes bytes.
func copyBrokerToRaw(broker net.Conn, raw net.Conn, w *connWriter, window int, opened chan<- *helperPublisherStream) (err error) {
	var out *helperPublisherStream
	answered := false
	defer func() {
		if !answered {
			close(opened)
		}
		if out != nil && err != io.EOF {
			out.close()
		}
	}()
	for {
		frame, err := routebroker.ReadFrame(broker, maxPayload)
		if err != nil {
			return err
		}
		switch frame.Kind {
		case routebroker.KindOpenOK:
			if answered {
				continue
			}
			if window > 0 && routebroker.IsFlowOpen(frame.Payload) {
				out = newHelperPublisherStream(func() { _ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: 1}) })
				grant := out.enableFlow(window, 1, w)
				_ = out.attach(raw)
				if grant > 0 {
					_ = w.write(routebroker.WindowFrame(1, grant))
				}
			}
			answered = true
			opened <- out
		case routebroker.KindData:
			if out != nil {
				if err := out.write(frame.Payload); err != nil {
					return err
				}
			} else if _, err := raw.Write(frame.Payload); err != nil {
				return err
			}
		case routebroker.KindHalfClose:
			if out != nil {
				out.closeWrite()
			} else if tcp, ok := raw.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		case routebroker.KindWindow:
			if out != nil {
				if err := out.flow.grant(frame.Payload); err != nil {
					return err
				}
			}
		case routebroker.KindClose, routebroker.KindOpenError:
			if out != nil {
				// An orderly end lets the pump deliver the tail first.
				out.finishOrClose()
				<-out.done
			}
			return io.EOF
		case routebroker.KindPong:
		default:
			return fmt.Errorf("consumer adapter received unexpected broker frame %d", frame.Kind)
		}
	}
}
