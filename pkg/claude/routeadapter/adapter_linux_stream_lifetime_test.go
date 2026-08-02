//go:build linux

package routeadapter

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// A publisher target that answers every request on every connection, for as
// long as the connection lives. A stream lifetime defect shows up as a missing
// or truncated reply, never as a target that stopped serving.
func startEchoTarget(t *testing.T, reply func(request []byte) []byte) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				for {
					n, readErr := c.Read(buf)
					if n > 0 {
						if _, writeErr := c.Write(reply(buf[:n])); writeErr != nil {
							return
						}
					}
					if readErr != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return listener
}

// publisherHarness drives RunPublisher over an in-memory channel, standing in
// for the broker so the stream lifetime can be exercised without a namespace.
type publisherHarness struct {
	brokerSide net.Conn
	done       chan error
	mu         sync.Mutex
	frames     []routebroker.Frame
	cond       *sync.Cond
	readErr    error
}

func startPublisherHarness(t *testing.T, target string) *publisherHarness {
	t.Helper()
	return startPublisherHarnessWithContext(t, context.Background(), target)
}

// startPublisherHarnessWithContext is the same harness under a caller-owned
// context, so a test can prove what cancellation tears down.
func startPublisherHarnessWithContext(t *testing.T, ctx context.Context, target string) *publisherHarness {
	t.Helper()
	brokerSide, publisherSide := net.Pipe()
	h := &publisherHarness{brokerSide: brokerSide, done: make(chan error, 1)}
	h.cond = sync.NewCond(&h.mu)
	go func() { h.done <- RunPublisher(ctx, publisherSide, target) }()
	go func() {
		for {
			frame, err := routebroker.ReadFrame(brokerSide, routebroker.MaxFramePayload)
			h.mu.Lock()
			if err != nil {
				h.readErr = err
				h.cond.Broadcast()
				h.mu.Unlock()
				return
			}
			h.frames = append(h.frames, frame)
			h.cond.Broadcast()
			h.mu.Unlock()
		}
	}()
	t.Cleanup(func() { _ = brokerSide.Close() })
	return h
}

func (h *publisherHarness) send(t *testing.T, frame routebroker.Frame) {
	t.Helper()
	_ = h.brokerSide.SetWriteDeadline(time.Now().Add(5 * time.Second))
	require.NoError(t, routebroker.WriteFrame(h.brokerSide, frame, routebroker.MaxFramePayload))
}

// awaitData waits for the next DATA frame on one stream, so a test asserts on
// the reply that actually came back rather than on a sleep.
func (h *publisherHarness) awaitData(t *testing.T, stream uint64, from int) (string, int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	go func() {
		time.Sleep(time.Until(deadline))
		h.mu.Lock()
		h.cond.Broadcast()
		h.mu.Unlock()
	}()
	h.mu.Lock()
	defer h.mu.Unlock()
	for {
		for i := from; i < len(h.frames); i++ {
			if h.frames[i].Stream == stream && h.frames[i].Kind == routebroker.KindData {
				return string(h.frames[i].Payload), i + 1
			}
		}
		if h.readErr != nil {
			t.Fatalf("publisher channel died while stream %d awaited its reply: %v", stream, h.readErr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream %d never received its reply", stream)
		}
		h.cond.Wait()
	}
}

func (h *publisherHarness) requireChannelAlive(t *testing.T) {
	t.Helper()
	select {
	case err := <-h.done:
		t.Fatalf("publisher tore down the whole channel: %v", err)
	default:
	}
}

// TestRunPublisherAdmitsStreamBeforeTargetDialCompletes is the regression guard
// for the TCL-960 stream lifetime boundary. A client that writes immediately
// after connecting produces OPEN followed at once by DATA. The target dial runs
// off the read loop, so the stream must already be admitted when that DATA
// arrives; otherwise the publisher used to fail the entire channel and every
// other route stream on it died with it.
func TestRunPublisherAdmitsStreamBeforeTargetDialCompletes(t *testing.T) {
	target := startEchoTarget(t, func(request []byte) []byte { return append([]byte("reply:"), request...) })
	h := startPublisherHarness(t, "tcp://"+target.Addr().String())

	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: []byte("hello")})

	payload, _ := h.awaitData(t, 1, 0)
	require.Equal(t, "reply:hello", payload, "data that arrived during the dial must reach the target")
	h.requireChannelAlive(t)
}

// TestRunPublisherSustainsManyShortStreams covers the consumer contract the
// Linux activation cell asserts: many short-lived connections, each opening its
// own stream and expecting its own reply. It fails if the publisher or its
// target stops serving after the first exchange, and it fails if one stream's
// teardown disturbs a later one.
func TestRunPublisherSustainsManyShortStreams(t *testing.T) {
	target := startEchoTarget(t, func(request []byte) []byte { return append([]byte("reply:"), request...) })
	h := startPublisherHarness(t, "tcp://"+target.Addr().String())

	const exchanges = 96
	cursor := 0
	for i := 1; i <= exchanges; i++ {
		stream := uint64(i)
		request := fmt.Sprintf("opaque-%d", i)
		h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: stream})
		h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: stream, Payload: []byte(request)})
		payload, next := h.awaitData(t, stream, cursor)
		cursor = next
		require.Equal(t, "reply:"+request, payload, "exchange %d must be answered", i)
		// The consumer closes its socket as soon as it has the reply.
		h.send(t, routebroker.Frame{Kind: routebroker.KindClose, Stream: stream})
	}
	h.requireChannelAlive(t)
}

// TestRunPublisherCarriesHalfCloseAcrossTheDialWindow keeps the production
// half-close contract intact for a stream that half-closes before its target
// connection exists. The target only answers once it has seen EOF, so a lost
// half-close shows up as a missing reply.
func TestRunPublisherCarriesHalfCloseAcrossTheDialWindow(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		request, readErr := io.ReadAll(conn)
		if readErr != nil {
			return
		}
		_, _ = conn.Write(append([]byte("eof:"), request...))
	}()

	h := startPublisherHarness(t, "tcp://"+listener.Addr().String())
	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: []byte("body")})
	h.send(t, routebroker.Frame{Kind: routebroker.KindHalfClose, Stream: 1})

	payload, _ := h.awaitData(t, 1, 0)
	require.Equal(t, "eof:body", payload, "an early half-close must still reach the target")
	h.requireChannelAlive(t)
}

// TestRunPublisherKeepsChannelAliveOnUnknownStreamData pins the blast radius:
// a frame for a stream the channel does not know closes that stream only.
func TestRunPublisherKeepsChannelAliveOnUnknownStreamData(t *testing.T) {
	target := startEchoTarget(t, func(request []byte) []byte { return append([]byte("reply:"), request...) })
	h := startPublisherHarness(t, "tcp://"+target.Addr().String())

	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 404, Payload: []byte("orphan")})

	// A live stream opened afterwards must still work.
	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: []byte("hello")})
	payload, _ := h.awaitData(t, 1, 0)
	require.Equal(t, "reply:hello", payload)
	h.requireChannelAlive(t)
}
