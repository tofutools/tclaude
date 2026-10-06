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
	"net/netip"
	"net/url"
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
	// publisherDialTimeout keeps a target that silently drops SYNs from
	// holding a stream open for the kernel's connect timeout.
	publisherDialTimeout = 5 * time.Second
	// openAnswerTimeout bounds one unanswered OPEN. It is comfortably longer
	// than the publisher's own dial timeout, so it only fires when the answer
	// is genuinely lost rather than merely slow.
	openAnswerTimeout = 10 * time.Second
)

// openAnswerChannelGone is a local, never-transmitted refusal used to release
// openers when the route channel itself goes away.
const openAnswerChannelGone = "route channel closed"

var (
	ErrInvalidTarget  = errors.New("route publisher target is not a namespace-local loopback endpoint")
	ErrChannelRefused = errors.New("route broker channel refused")
)

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

// ValidatePublisherTarget accepts only an explicit TCP loopback address. A
// hostname is intentionally refused: resolving it here would make the
// publisher-local claim depend on mutable DNS and could turn a route into a
// host or Internet relay.
func ValidatePublisherTarget(raw string) (string, error) {
	return validateLoopbackEndpoint(raw, ErrInvalidTarget)
}

// ValidateConsumerEndpoint applies the same namespace-local address contract
// to the ephemeral listener returned by a consumer helper.
func ValidateConsumerEndpoint(raw string) (string, error) {
	return validateLoopbackEndpoint(raw, errors.New("route consumer endpoint is not a namespace-local loopback listener"))
}

func validateLoopbackEndpoint(raw string, invalid error) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "tcp" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: address must be tcp://<loopback-ip>:<port>", invalid)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: address is missing a host", invalid)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		return "", fmt.Errorf("%w: address is invalid: %v", invalid, err)
	}
	if strings.TrimSpace(host) == "" {
		return "", fmt.Errorf("%w: host is empty", invalid)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return "", fmt.Errorf("%w: host %q is not a loopback IP", invalid, host)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", fmt.Errorf("%w: port %q is invalid", invalid, port)
	}
	return net.JoinHostPort(addr.String(), strconv.Itoa(portNumber)), nil
}

// RunPublisher serves one authenticated publisher channel. Every target dial
// occurs in the caller's namespace, so the broker never dials the target.
func RunPublisher(ctx context.Context, channel net.Conn, rawTarget string) error {
	target, err := ValidatePublisherTarget(rawTarget)
	if err != nil {
		return err
	}
	if channel == nil {
		return errors.New("route publisher channel is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopOnContext(ctx, channel)
	w := &connWriter{conn: channel}
	window := channelFlowWindow(channel)
	streams := &publisherStreams{items: make(map[uint64]*helperPublisherStream)}
	defer streams.closeAll()
	defer channel.Close()

	for {
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
		case routebroker.KindOpen:
			if frame.Stream == 0 {
				return routebroker.ErrInvalidStreamID
			}
			// The stream is admitted here, synchronously, so data that a client
			// wrote immediately after connecting cannot arrive before the stream
			// this channel already accepted exists. Only the dial is deferred.
			streamID := frame.Stream
			stream := newHelperPublisherStream(func() {
				streams.removeAndClose(streamID)
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: streamID})
			})
			grant := 0
			if window > 0 && routebroker.IsFlowOpen(frame.Payload) {
				grant = stream.enableFlow(window, streamID, w)
			}
			if !streams.add(streamID, stream) {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: streamID, Payload: []byte(routebroker.OpenErrorDuplicatePublisherStream)})
				continue
			}
			go openPublisherStream(ctx, target, streamID, stream, streams, w, grant)
		case routebroker.KindData:
			stream, ok := streams.get(frame.Stream)
			if !ok {
				// Data for a stream this channel never admitted, or has already
				// closed, is a single-stream condition. Failing the channel here
				// would take every other route stream down with it.
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
				continue
			}
			if err := stream.write(frame.Payload); err != nil {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
				streams.removeAndClose(frame.Stream)
			}
		case routebroker.KindHalfClose:
			stream, ok := streams.get(frame.Stream)
			if !ok {
				continue
			}
			stream.closeWrite()
		case routebroker.KindClose:
			if stream, ok := streams.remove(frame.Stream); ok {
				stream.finishOrClose()
			}
		case routebroker.KindWindow:
			if stream, ok := streams.get(frame.Stream); ok && stream.flow != nil {
				if err := stream.flow.grant(frame.Payload); err != nil {
					_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
					streams.removeAndClose(frame.Stream)
				}
			}
		default:
			return fmt.Errorf("publisher received invalid frame kind %d", frame.Kind)
		}
	}
}

func openPublisherStream(ctx context.Context, target string, streamID uint64, stream *helperPublisherStream, streams *publisherStreams, w *connWriter, grant int) {
	dialer := net.Dialer{Timeout: publisherDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		// The stream was admitted before the dial, so it has to be withdrawn
		// here rather than simply never having existed.
		streams.removeAndClose(streamID)
		_ = w.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: streamID, Payload: []byte(routebroker.OpenErrorTargetUnavailable)})
		return
	}
	// Anything buffered while the dial was in flight is queued ahead of what
	// follows it, so the peer never observes the stream out of order.
	if err := stream.attach(conn); err != nil {
		// attach only fails once the stream has already been closed on this
		// channel, so the peer has withdrawn it and CLOSE is the honest answer.
		_ = conn.Close()
		streams.removeAndClose(streamID)
		_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: streamID})
		return
	}
	if err := w.write(routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: streamID}); err != nil {
		streams.removeAndClose(streamID)
		return
	}
	if grant > 0 {
		if err := w.write(routebroker.WindowFrame(streamID, grant)); err != nil {
			streams.removeAndClose(streamID)
			return
		}
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, readErr := stream.flow.read(conn, buf)
			if n > 0 {
				if writeErr := w.write(routebroker.Frame{Kind: routebroker.KindData, Stream: streamID, Payload: append([]byte(nil), buf[:n]...)}); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					stream.markHalfCloseSent()
					_ = w.write(routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: streamID})
				} else {
					_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: streamID})
					streams.removeAndClose(streamID)
				}
				return
			}
		}
	}()
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

// channelFlowWindow is the channel's negotiated receive window; zero means
// no stream on it is flow-controlled.
func channelFlowWindow(channel net.Conn) int {
	if fw, ok := channel.(interface{ FlowWindow() int }); ok && fw.FlowWindow() > 0 {
		return routebroker.ClampWindow(fw.FlowWindow())
	}
	return 0
}

// streamFlow is one flow-controlled stream's credit in both directions. A
// nil *streamFlow is a stream without flow control.
type streamFlow struct {
	send *routebroker.SendWindow
	recv *routebroker.RecvWindow
}

// read reads the stream's source, but only as much as the peer has granted:
// out of credit, the source is simply not read, and TCP holds its sender.
func (f *streamFlow) read(conn net.Conn, buf []byte) (int, error) {
	if f == nil {
		return conn.Read(buf)
	}
	limit, err := f.send.Wait(len(buf))
	if err != nil {
		return 0, net.ErrClosed
	}
	n, err := conn.Read(buf[:limit])
	f.send.Spend(n)
	return n, err
}

func (f *streamFlow) grant(payload []byte) error {
	n, err := routebroker.ParseWindow(payload)
	if err != nil {
		return err
	}
	return f.send.Grant(n)
}

type connWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func (w *connWriter) write(frame routebroker.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return routebroker.WriteFrame(w.conn, frame, routebroker.MaxFramePayload)
}

func stopOnContext(ctx context.Context, closer io.Closer) {
	go func() {
		<-ctx.Done()
		_ = closer.Close()
	}()
}

// publisherPendingLimit bounds how much one stream may hold for its target. It
// covers the dial window and, equally, a target that has stopped reading: since
// the channel read loop never writes to a target itself, the only backpressure
// a wedged target can apply is against its own stream's bound.
const publisherPendingLimit = 1 << 20

// publisherTargetWriteTimeout bounds a single write to the target, so a
// connection that is open but permanently unreadable still fails its own stream
// rather than parking that stream's pump forever.
const publisherTargetWriteTimeout = 30 * time.Second

var (
	errPublisherStreamClosed  = errors.New("publisher stream is closed")
	errPublisherStreamBacklog = errors.New("publisher stream exceeded its target buffer")
)

// helperPublisherStream is one publisher-side stream. The target dial
// deliberately runs off the channel read loop, so a stream is admitted before
// its connection exists, and payloads arriving in that window are held in
// arrival order. A client that writes immediately after connecting therefore
// cannot race its own OPEN.
//
// A single pump goroutine owns every write to the target, and the mutex is
// never held across target I/O. That is what keeps one target which has stopped
// reading from stalling the other streams and the control frames that share its
// channel: the read loop only ever appends to a bounded queue. A stream whose
// target stops accepting bytes fails alone, once its own bound is reached.
type helperPublisherStream struct {
	mu           sync.Mutex
	wake         *sync.Cond
	conn         net.Conn
	pending      [][]byte
	pendingBytes int
	halfClosed   bool
	closed       bool
	// fail retires this one stream when its target write fails. It runs off the
	// read loop, so it must not assume the read loop is still running.
	fail func()
	// flow is set on a flow-controlled stream. Credit then bounds pending in
	// place of publisherPendingLimit, the pump grants credit back as the
	// target accepts bytes, and a target that reads slowly parks its sender
	// rather than failing the stream. The consumer helper uses the same type
	// for its local sockets on such streams.
	flow    *streamFlow
	granted func(n int)
	// sawHalfClose records that the peer finished its direction and
	// sentHalfClose that this side finished its own; finishing asks the
	// pump to close the stream once it has delivered everything.
	sawHalfClose, sentHalfClose, finishing bool
}

// markHalfCloseSent records this side's HALF_CLOSE; call it before sending.
func (s *helperPublisherStream) markHalfCloseSent() {
	s.mu.Lock()
	s.sentHalfClose = true
	s.mu.Unlock()
}

// enableFlow makes the stream flow-controlled with a receive window of
// window bytes, granting credit back over w. It returns the credit to grant
// once the stream is open. Call it before the stream is shared.
func (s *helperPublisherStream) enableFlow(window int, id uint64, w *connWriter) int {
	recv, grant := routebroker.NewRecvWindow(window)
	s.flow = &streamFlow{send: routebroker.NewSendWindow(), recv: recv}
	s.granted = func(n int) {
		if g := recv.Consumed(n); g > 0 {
			_ = w.write(routebroker.WindowFrame(id, g))
		}
	}
	return grant
}

func newHelperPublisherStream(fail func()) *helperPublisherStream {
	s := &helperPublisherStream{fail: fail}
	s.wake = sync.NewCond(&s.mu)
	return s
}

// write queues one payload for the target. It never performs target I/O, so the
// channel read loop that calls it cannot be parked by a target that has stopped
// reading.
func (s *helperPublisherStream) write(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errPublisherStreamClosed
	}
	if s.flow != nil {
		if err := s.flow.recv.Received(len(payload)); err != nil {
			return err
		}
	} else if s.pendingBytes+len(payload) > publisherPendingLimit {
		return errPublisherStreamBacklog
	}
	s.pending = append(s.pending, payload)
	s.pendingBytes += len(payload)
	s.wake.Signal()
	return nil
}

// attach adopts the dialed connection and starts the pump, which flushes
// whatever arrived while the dial was in flight before anything that follows it.
func (s *helperPublisherStream) attach(conn net.Conn) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errPublisherStreamClosed
	}
	s.conn = conn
	s.wake.Signal()
	s.mu.Unlock()
	go s.pump()
	return nil
}

// closeWrite carries the consumer's half-close through to the target. It is
// recorded rather than applied, so it lands after everything the consumer sent
// before it, whether or not the dial has completed.
func (s *helperPublisherStream) closeWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halfClosed = true
	s.sawHalfClose = true
	s.wake.Signal()
}

// finishOrClose handles the broker's CLOSE. On a flow-controlled stream
// where both sides already half-closed it is the orderly end that follows, and the pump may still hold the tail of what the peer sent:
// it delivers that, under the write deadline, before closing. Otherwise
// CLOSE is an abort and the stream closes at once.
func (s *helperPublisherStream) finishOrClose() {
	s.mu.Lock()
	if s.flow != nil && s.sawHalfClose && s.sentHalfClose && s.conn != nil && !s.closed {
		s.finishing = true
		s.wake.Signal()
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.close()
}

// pump is the only writer to the target.
func (s *helperPublisherStream) pump() {
	for {
		s.mu.Lock()
		for len(s.pending) == 0 && !s.halfClosed && !s.closed && !s.finishing {
			s.wake.Wait()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		if len(s.pending) == 0 && !s.halfClosed && s.finishing {
			s.mu.Unlock()
			s.close()
			return
		}
		if len(s.pending) > 0 {
			payload := s.pending[0]
			s.pending = s.pending[1:]
			s.pendingBytes -= len(payload)
			conn := s.conn
			deadline := s.flow == nil || s.finishing
			// Once the peer finished sending, credit can unlock nothing,
			// and a late grant could outlive the stream on the broker.
			grant := s.granted != nil && !s.sawHalfClose
			s.mu.Unlock()
			// The deadline bounds the write, and because close() closes the
			// connection outside the mutex, a close interrupts a write already
			// in flight instead of waiting it out. A flow-controlled stream
			// has no deadline while it is live: its slow reader holds only
			// its own window.
			if deadline {
				_ = conn.SetWriteDeadline(time.Now().Add(publisherTargetWriteTimeout))
			}
			if _, err := conn.Write(payload); err != nil {
				s.failStream()
				return
			}
			if grant {
				s.granted(len(payload))
			}
			continue
		}
		conn := s.conn
		s.halfClosed = false
		s.mu.Unlock()
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
}

// failStream retires a stream whose target stopped accepting bytes. The stream
// is the blast radius; the channel and its other streams keep running.
func (s *helperPublisherStream) failStream() {
	s.mu.Lock()
	alreadyClosed := s.closed
	s.mu.Unlock()
	if alreadyClosed || s.fail == nil {
		return
	}
	s.fail()
}

func (s *helperPublisherStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.pending, s.pendingBytes = nil, 0
	conn := s.conn
	s.wake.Broadcast()
	s.mu.Unlock()
	if s.flow != nil {
		s.flow.send.Close()
	}
	// Closing outside the mutex is what lets a close interrupt an in-flight
	// write to a target that stopped reading, rather than queueing behind it.
	if conn != nil {
		_ = conn.Close()
	}
}

type publisherStreams struct {
	mu    sync.Mutex
	items map[uint64]*helperPublisherStream
}

func (s *publisherStreams) add(id uint64, stream *helperPublisherStream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[id]; exists {
		return false
	}
	s.items[id] = stream
	return true
}
func (s *publisherStreams) get(id uint64) (*helperPublisherStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.items[id]
	return c, ok
}
func (s *publisherStreams) remove(id uint64) (*helperPublisherStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.items[id]
	if ok {
		delete(s.items, id)
	}
	return c, ok
}
func (s *publisherStreams) removeAndClose(id uint64) {
	if c, ok := s.remove(id); ok {
		c.close()
	}
}

// closeAll detaches the whole registry first and closes afterwards, so a target
// that is slow to close cannot hold the registry mutex against the streams that
// are still trying to make progress.
func (s *publisherStreams) closeAll() {
	s.mu.Lock()
	items := s.items
	s.items = make(map[uint64]*helperPublisherStream)
	s.mu.Unlock()
	for _, c := range items {
		c.close()
	}
}

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
