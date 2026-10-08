//go:build linux

// Package routeadapter contains the Linux endpoint side of group routes.
//
// A route adapter deliberately has no authority of its own. It receives an
// already-authenticated routebroker channel and only supplies the endpoint
// that belongs to its own network namespace. The agentd endpoint is the
// caller that authenticates the channel before handing it here.
package routeadapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

const (
	RolePublisher = "publisher"
	RoleConsumer  = "consumer"

	channelPath                  = "/v1/routes/channel"
	channelHeaderRole            = "X-Tclaude-Route-Role"
	channelHeaderID              = "X-Tclaude-Route-ID"
	channelHeaderLease           = "X-Tclaude-Route-Lease-ID"
	channelHeaderAgent           = "X-Tclaude-Route-Agent-ID"
	channelHeaderConv            = "X-Tclaude-Route-Conv-ID"
	channelHeaderLaunch          = "X-Tclaude-Route-Launch-Generation"
	channelHeaderGroupGeneration = "X-Tclaude-Route-Group-Generation"
	channelHeaderEndpoint        = "X-Tclaude-Route-Consumer-Endpoint"
	// ChannelHeaderFlow is sent by a helper that implements flow control;
	// ChannelHeaderFlowWindow is agentd's answer on the upgrade, naming the
	// receive window the helper should use. Without the answer the helper
	// runs every stream without flow control.
	ChannelHeaderFlow       = "X-Tclaude-Route-Flow"
	ChannelHeaderFlowWindow = "X-Tclaude-Route-Flow-Window"
)

// Bounds on reopening a stream whose publisher channel is absent. The window
// is deliberately short: it should cover a helper reattach — a poll tick or a
// few — without turning a route whose publisher is simply gone into a long
// hang for every client that connects.
const (
	openRetryWindow         = 2 * time.Second
	openRetryInitialBackoff = 25 * time.Millisecond
	openRetryMaxBackoff     = 250 * time.Millisecond
	// openAnswerTimeout bounds one unanswered OPEN. It is comfortably longer
	// than the publisher's own dial timeout, so it only fires when the answer
	// is genuinely lost rather than merely slow.
	openAnswerTimeout = 10 * time.Second
)

// openAnswerChannelGone is a local, never-transmitted refusal used to release
// openers when the route channel itself goes away.
const openAnswerChannelGone = "route channel closed"

var ErrChannelRefused = errors.New("route broker channel refused")

// ChannelAuth is the generation-bound identity supplied to agentd when a
// helper attaches its channel. The adapter never treats these values as
// authority; agentd checks them against the durable M1 records and the
// connecting peer identity.
type ChannelAuth struct {
	Role             string
	RouteID          string
	LeaseID          string
	AgentID          string
	ConvID           string
	LaunchGeneration string
	GroupGeneration  int64
	ConsumerEndpoint string
	Credential       string
}

// RunConsumer serves one authenticated consumer channel and exposes a
// consumer-local listener. The listener is never shared with the publisher or
// host; callers should bind it after entering the namespace.
func RunConsumer(ctx context.Context, channel net.Conn, listener net.Listener) error {
	if channel == nil || listener == nil {
		return errors.New("route consumer channel and listener are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopOnContext(ctx, channel)
	stopOnContext(ctx, listener)
	w := &connWriter{conn: channel}
	window := channelFlowWindow(channel)
	streams := &consumerStreams{items: make(map[uint64]net.Conn), nextID: 1}
	defer streams.closeAll()
	defer channel.Close()
	defer listener.Close()

	acceptErr := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				acceptErr <- err
				return
			}
			go openConsumerStream(ctx, conn, streams, w)
		}
	}()

	for {
		select {
		case err := <-acceptErr:
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		default:
		}
		frame, readErr := routebroker.ReadFrame(channel, routebroker.MaxFramePayload)
		if readErr != nil {
			if ctx.Err() != nil || errors.Is(readErr, net.ErrClosed) || errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		switch frame.Kind {
		case routebroker.KindPing:
			if err := w.write(routebroker.Frame{Kind: routebroker.KindPong}); err != nil {
				return err
			}
		case routebroker.KindOpenOK:
			// The local socket is already accepted; OPEN_OK only confirms the
			// publisher side has connected to its target. It also releases the
			// local reader, which must not forward bytes the publisher has no
			// stream for yet.
			//
			// A flow-controlled stream gets its pump and credit here, on the
			// read loop, so they exist before any DATA for it can arrive.
			if window > 0 && routebroker.IsFlowOpen(frame.Payload) {
				if grant, ok := streams.enableFlow(frame.Stream, window, w); ok && grant > 0 {
					_ = w.write(routebroker.WindowFrame(frame.Stream, grant))
				}
			}
			streams.resolveOpen(frame.Stream, nil)
		case routebroker.KindOpenError:
			// Hand the refusal to the opener, which decides whether reopening
			// can help. It owns the socket until then.
			if !streams.resolveOpen(frame.Stream, frame.Payload) {
				if conn, ok := streams.remove(frame.Stream); ok {
					_ = conn.Close()
				}
			}
		case routebroker.KindClose:
			streams.finish(frame.Stream)
		case routebroker.KindData:
			conn, ok := streams.get(frame.Stream)
			if !ok {
				continue
			}
			var err error
			if out := streams.flowOut(frame.Stream); out != nil {
				// Never write the local socket on this loop: a client that
				// reads slowly would stall every stream on the channel.
				err = out.write(frame.Payload)
			} else {
				_, err = conn.Write(frame.Payload)
			}
			if err != nil {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
				streams.removeAndClose(frame.Stream)
			}
		case routebroker.KindHalfClose:
			if out := streams.flowOut(frame.Stream); out != nil {
				out.closeWrite() // after what the pump still holds
			} else if conn, ok := streams.get(frame.Stream); ok {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
			}
		case routebroker.KindWindow:
			if out := streams.flowOut(frame.Stream); out != nil {
				if err := out.flow.grant(frame.Payload); err != nil {
					_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
					streams.removeAndClose(frame.Stream)
				}
			}
		default:
			return fmt.Errorf("consumer received invalid frame kind %d", frame.Kind)
		}
	}
}

// openConsumerStream owns one accepted local connection until the route has a
// stream for it. Local bytes are not forwarded before OPEN_OK: the publisher
// helper dials its target asynchronously, so data that arrives ahead of the
// confirmation has no stream to land in.
//
// A publisher channel that is merely absent — agentd restart, helper restart,
// publisher relaunch — is refused as transient, and reopening once it returns
// is what the caller's client would otherwise have to do itself, except the
// client cannot: it is already connected, and would see a reset instead.
func openConsumerStream(ctx context.Context, conn net.Conn, streams *consumerStreams, w *connWriter) {
	deadline := time.Now().Add(openRetryWindow)
	backoff := openRetryInitialBackoff
	for {
		id, answer := streams.addPending(conn)
		if err := w.write(routebroker.Frame{Kind: routebroker.KindOpen, Stream: id}); err != nil {
			streams.dropPending(id)
			_ = conn.Close()
			return
		}
		// The answer itself is bounded too. A publisher whose target neither
		// accepts nor refuses leaves OPEN unanswered, and the local client
		// would otherwise sit unread for the kernel's connect timeout.
		answerTimer := time.NewTimer(openAnswerTimeout)
		var refusal []byte
		select {
		case refusal = <-answer:
			answerTimer.Stop()
		case <-answerTimer.C:
			// The broker only leaves an OPEN unanswered once it has allocated
			// the stream, so abandoning it silently would strand that stream —
			// and its route and agent connection budget — until the whole
			// channel detaches. CLOSE is safe even with an answer in flight:
			// the broker tombstones the stream and drops the late frame.
			_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
			streams.dropPending(id)
			_ = conn.Close()
			return
		case <-ctx.Done():
			answerTimer.Stop()
			// The stream may already exist on the broker side. Nothing sends
			// CLOSE for it here because the same cancellation tears the whole
			// channel down, and the broker reclaims its streams on detach.
			streams.dropPending(id)
			_ = conn.Close()
			return
		}
		if refusal == nil {
			readConsumerStream(ctx, id, conn, streams, w)
			return
		}
		streams.dropPending(id)
		if !routebroker.OpenErrorIsTransient(refusal) || !time.Now().Before(deadline) {
			_ = conn.Close()
			return
		}
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			_ = conn.Close()
			return
		}
		if backoff *= 2; backoff > openRetryMaxBackoff {
			backoff = openRetryMaxBackoff
		}
	}
}

func readConsumerStream(ctx context.Context, id uint64, conn net.Conn, streams *consumerStreams, w *connWriter) {
	var flow *streamFlow
	out := streams.flowOut(id)
	if out != nil {
		flow = out.flow
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := flow.read(conn, buf)
		if n > 0 {
			if writeErr := w.write(routebroker.Frame{Kind: routebroker.KindData, Stream: id, Payload: append([]byte(nil), buf[:n]...)}); writeErr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if out != nil {
					out.markHalfCloseSent()
				}
				_ = w.write(routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: id})
			} else if ctx.Err() == nil {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
				streams.removeAndClose(id)
			}
			return
		}
	}
}

// DialUnixChannel performs the trusted launch-boundary handshake to agentd.
// It intentionally uses a Unix socket and no TCP address, so a helper cannot
// accidentally attach to a different daemon or widen the network posture.
func DialUnixChannel(ctx context.Context, socketPath string, auth ChannelAuth) (net.Conn, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, errors.New("route channel socket path is required")
	}
	if auth.Role != RolePublisher && auth.Role != RoleConsumer {
		return nil, fmt.Errorf("invalid route channel role %q", auth.Role)
	}
	if strings.TrimSpace(auth.RouteID) == "" || strings.TrimSpace(auth.AgentID) == "" || strings.TrimSpace(auth.ConvID) == "" || strings.TrimSpace(auth.LaunchGeneration) == "" {
		return nil, errors.New("route channel identity is incomplete")
	}
	if auth.Role == RoleConsumer && strings.TrimSpace(auth.LeaseID) == "" {
		return nil, errors.New("consumer route channel lease is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial agentd route socket: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://tclaude.invalid"+channelPath, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tclaude-route-v1")
	req.Header.Set(channelHeaderRole, auth.Role)
	req.Header.Set(channelHeaderID, auth.RouteID)
	req.Header.Set(channelHeaderLease, auth.LeaseID)
	req.Header.Set(channelHeaderAgent, auth.AgentID)
	req.Header.Set(channelHeaderConv, auth.ConvID)
	req.Header.Set(channelHeaderLaunch, auth.LaunchGeneration)
	req.Header.Set(channelHeaderGroupGeneration, strconv.FormatInt(auth.GroupGeneration, 10))
	req.Header.Set(ChannelHeaderFlow, "1")
	if auth.Credential != "" {
		req.Header.Set("X-Tclaude-Route-Helper-Credential", auth.Credential)
	}
	if auth.ConsumerEndpoint != "" {
		req.Header.Set(channelHeaderEndpoint, auth.ConsumerEndpoint)
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write route channel request: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read route channel response: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("%w: status=%s detail=%s", ErrChannelRefused, resp.Status, strings.TrimSpace(string(body)))
	}
	window, _ := strconv.Atoi(resp.Header.Get(ChannelHeaderFlowWindow))
	return &bufferedConn{Conn: conn, reader: reader, flowWindow: window}, nil
}

type bufferedConn struct {
	net.Conn
	reader     *bufio.Reader
	flowWindow int
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

// FlowWindow is the receive window agentd assigned this channel, or zero
// when it did not enable flow control.
func (c *bufferedConn) FlowWindow() int { return c.flowWindow }

type consumerStreams struct {
	mu     sync.Mutex
	items  map[uint64]net.Conn
	opens  map[uint64]chan []byte
	nextID uint64
	// outs holds the local-socket pump of each flow-controlled stream.
	outs map[uint64]*helperPublisherStream
}

// enableFlow gives an opened stream its pump and credit, and returns the
// credit to grant up front. False if the stream is already gone.
func (s *consumerStreams) enableFlow(id uint64, window int, w *connWriter) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conn, ok := s.items[id]
	if !ok {
		return 0, false
	}
	out := newHelperPublisherStream(func() {
		s.removeAndClose(id)
		_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: id})
	})
	grant := out.enableFlow(window, id, w)
	_ = out.attach(conn)
	if s.outs == nil {
		s.outs = make(map[uint64]*helperPublisherStream)
	}
	s.outs[id] = out
	return grant, true
}

// finish retires a stream on the broker's CLOSE, letting a flow-controlled
// stream's pump deliver what it holds when the end is orderly.
func (s *consumerStreams) finish(id uint64) {
	s.mu.Lock()
	out := s.outs[id]
	delete(s.outs, id)
	s.mu.Unlock()
	if out == nil {
		s.removeAndClose(id)
		return
	}
	// The pump owns the socket from here, so the registry forgets it
	// without closing it.
	s.remove(id)
	out.finishOrClose()
}

// flowOut is a flow-controlled stream's pump, or nil.
func (s *consumerStreams) flowOut(id uint64) *helperPublisherStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outs[id]
}

// dropOutLocked retires a stream's pump; it closes the socket as well.
func (s *consumerStreams) dropOutLocked(id uint64) {
	if out, ok := s.outs[id]; ok {
		delete(s.outs, id)
		out.close()
	}
}

// addPending registers a stream whose OPEN has not been answered yet. The
// returned channel carries nil for OPEN_OK or the OPEN_ERROR payload.
func (s *consumerStreams) addPending(conn net.Conn) (uint64, chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := s.nextID
	s.nextID++
	s.items[id] = conn
	if s.opens == nil {
		s.opens = make(map[uint64]chan []byte)
	}
	answer := make(chan []byte, 1)
	s.opens[id] = answer
	return id, answer
}

// resolveOpen delivers an open answer and reports whether one was pending. A
// late or duplicate answer is dropped rather than closing an established
// stream.
func (s *consumerStreams) resolveOpen(id uint64, payload []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolvePendingLocked(id, payload)
}

// resolvePendingLocked hands an answer to a waiting opener, if there is one.
// The send never blocks: the channel is buffered and single-use, so an answer
// whose opener has already left must not stall the registry lock.
func (s *consumerStreams) resolvePendingLocked(id uint64, payload []byte) bool {
	answer, ok := s.opens[id]
	if !ok {
		return false
	}
	delete(s.opens, id)
	select {
	case answer <- payload:
	default:
	}
	return true
}

func (s *consumerStreams) dropPending(id uint64) {
	s.mu.Lock()
	delete(s.opens, id)
	delete(s.items, id)
	s.dropOutLocked(id)
	s.mu.Unlock()
}
func (s *consumerStreams) get(id uint64) (net.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.items[id]
	return c, ok
}
func (s *consumerStreams) remove(id uint64) (net.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Tearing a stream down also ends any open still waiting on it, so an
	// opener cannot be left parked on an answer that will never arrive while
	// holding a socket that was just closed under it.
	s.resolvePendingLocked(id, []byte(openAnswerChannelGone))
	s.dropOutLocked(id)
	c, ok := s.items[id]
	if ok {
		delete(s.items, id)
	}
	return c, ok
}
func (s *consumerStreams) removeAndClose(id uint64) {
	if c, ok := s.remove(id); ok {
		_ = c.Close()
	}
}
func (s *consumerStreams) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Release openers waiting on an answer that is no longer coming. The
	// sentinel is local and never reaches the wire; it only has to be a
	// refusal that reopening cannot clear.
	for id := range s.opens {
		s.resolvePendingLocked(id, []byte(openAnswerChannelGone))
	}
	for id, c := range s.items {
		s.dropOutLocked(id)
		_ = c.Close()
		delete(s.items, id)
	}
}
