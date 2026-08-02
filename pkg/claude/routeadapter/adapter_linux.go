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
)

// Bounds on reopening a stream whose publisher channel is absent. The window
// is deliberately short: it should cover a helper reattach — a poll tick or a
// few — without turning a route whose publisher is simply gone into a long
// hang for every client that connects.
const (
	openRetryWindow         = 2 * time.Second
	openRetryInitialBackoff = 25 * time.Millisecond
	openRetryMaxBackoff     = 250 * time.Millisecond
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
	streams := &publisherStreams{items: make(map[uint64]net.Conn)}
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
			go openPublisherStream(ctx, target, frame.Stream, streams, w)
		case routebroker.KindData:
			conn, ok := streams.get(frame.Stream)
			if !ok {
				return fmt.Errorf("publisher received data for unknown stream %d", frame.Stream)
			}
			if _, err := conn.Write(frame.Payload); err != nil {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
				streams.remove(frame.Stream)
			}
		case routebroker.KindHalfClose:
			conn, ok := streams.get(frame.Stream)
			if !ok {
				continue
			}
			if tcp, ok := conn.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		case routebroker.KindClose:
			if conn, ok := streams.remove(frame.Stream); ok {
				_ = conn.Close()
			}
		default:
			return fmt.Errorf("publisher received invalid frame kind %d", frame.Kind)
		}
	}
}

func openPublisherStream(ctx context.Context, target string, streamID uint64, streams *publisherStreams, w *connWriter) {
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		_ = w.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: streamID, Payload: []byte(routebroker.OpenErrorTargetUnavailable)})
		return
	}
	if !streams.add(streamID, conn) {
		_ = conn.Close()
		_ = w.write(routebroker.Frame{Kind: routebroker.KindOpenError, Stream: streamID, Payload: []byte(routebroker.OpenErrorDuplicatePublisherStream)})
		return
	}
	if err := w.write(routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: streamID}); err != nil {
		streams.remove(streamID)
		return
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, readErr := conn.Read(buf)
			if n > 0 {
				if writeErr := w.write(routebroker.Frame{Kind: routebroker.KindData, Stream: streamID, Payload: append([]byte(nil), buf[:n]...)}); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
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
			if conn, ok := streams.remove(frame.Stream); ok {
				_ = conn.Close()
			}
		case routebroker.KindData:
			conn, ok := streams.get(frame.Stream)
			if !ok {
				continue
			}
			if _, err := conn.Write(frame.Payload); err != nil {
				_ = w.write(routebroker.Frame{Kind: routebroker.KindClose, Stream: frame.Stream})
				streams.removeAndClose(frame.Stream)
			}
		case routebroker.KindHalfClose:
			if conn, ok := streams.get(frame.Stream); ok {
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
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
		var refusal []byte
		select {
		case refusal = <-answer:
		case <-ctx.Done():
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
	buf := make([]byte, 32<<10)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if writeErr := w.write(routebroker.Frame{Kind: routebroker.KindData, Stream: id, Payload: append([]byte(nil), buf[:n]...)}); writeErr != nil {
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
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
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

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

type publisherStreams struct {
	mu    sync.Mutex
	items map[uint64]net.Conn
}

func (s *publisherStreams) add(id uint64, conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[id]; exists {
		return false
	}
	s.items[id] = conn
	return true
}
func (s *publisherStreams) get(id uint64) (net.Conn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.items[id]
	return c, ok
}
func (s *publisherStreams) remove(id uint64) (net.Conn, bool) {
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
		_ = c.Close()
	}
}
func (s *publisherStreams) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.items {
		_ = c.Close()
		delete(s.items, id)
	}
}

type consumerStreams struct {
	mu     sync.Mutex
	items  map[uint64]net.Conn
	opens  map[uint64]chan []byte
	nextID uint64
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
	answer, ok := s.opens[id]
	if ok {
		delete(s.opens, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	answer <- payload
	return true
}

func (s *consumerStreams) dropPending(id uint64) {
	s.mu.Lock()
	delete(s.opens, id)
	delete(s.items, id)
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
	for id, answer := range s.opens {
		answer <- []byte(openAnswerChannelGone)
		delete(s.opens, id)
	}
	for id, c := range s.items {
		_ = c.Close()
		delete(s.items, id)
	}
}
