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

// The publisher endpoint: serves one publisher channel by dialing the
// route's loopback target per stream. The Linux helper runs it inside the
// publisher's namespace; the Darwin adapter runs it in agentd.

// publisherDialTimeout keeps a target that silently drops SYNs from holding
// a stream open for the kernel's connect timeout.
const publisherDialTimeout = 5 * time.Second

var ErrInvalidTarget = errors.New("route publisher target is not a namespace-local loopback endpoint")

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

func stopOnContext(ctx context.Context, closer io.Closer) {
	go func() {
		<-ctx.Done()
		_ = closer.Close()
	}()
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
