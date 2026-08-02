//go:build linux

package routeadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// wedgedConn is a target that has stopped reading: a write reaches it and then
// never completes, which is the state a peer whose receive buffer is full
// leaves the connection in. Closing it releases the in-flight write, the way
// closing a real socket does.
type wedgedConn struct {
	closed   chan struct{}
	once     sync.Once
	inflight chan struct{}
}

func newWedgedConn() *wedgedConn {
	return &wedgedConn{closed: make(chan struct{}), inflight: make(chan struct{}, 64)}
}

func (c *wedgedConn) Write(p []byte) (int, error) {
	select {
	case c.inflight <- struct{}{}:
	default:
	}
	<-c.closed
	return 0, errors.New("target stopped reading")
}

func (c *wedgedConn) Read([]byte) (int, error) { <-c.closed; return 0, io.EOF }

func (c *wedgedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *wedgedConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *wedgedConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *wedgedConn) SetDeadline(time.Time) error      { return nil }
func (c *wedgedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *wedgedConn) SetWriteDeadline(time.Time) error { return nil }

// awaitInflightWrite returns once the stream's pump is parked inside a target
// write, which is the precondition every test below needs.
func (c *wedgedConn) awaitInflightWrite(t *testing.T) {
	t.Helper()
	select {
	case <-c.inflight:
	case <-time.After(5 * time.Second):
		t.Fatal("target write never started")
	}
}

// completesWithin runs fn and fails unless it returns inside the budget. That
// is the whole point of these tests: the operations it guards used to block for
// as long as the target stayed wedged.
func completesWithin(t *testing.T, budget time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(budget):
		t.Fatalf("%s did not complete within %s while the target was wedged", what, budget)
	}
}

// TestPublisherStreamWriteNeverBlocksOnAWedgedTarget is the direct regression
// for the shared-channel wedge. write() is called from the single channel read
// loop, so if it can block on target I/O then one target that stopped reading
// stalls every other stream and every control frame on that channel.
func TestPublisherStreamWriteNeverBlocksOnAWedgedTarget(t *testing.T) {
	target := newWedgedConn()
	t.Cleanup(func() { _ = target.Close() })
	stream := newHelperPublisherStream(func() {})
	require.NoError(t, stream.attach(target))
	require.NoError(t, stream.write([]byte("first")))
	target.awaitInflightWrite(t)

	completesWithin(t, 5*time.Second, "writes to a wedged stream", func() {
		for i := range 8 {
			require.NoError(t, stream.write([]byte(fmt.Sprintf("queued-%d", i))))
		}
	})
	// A half-close queued behind a wedged write must not block the loop either.
	completesWithin(t, 5*time.Second, "closeWrite on a wedged stream", stream.closeWrite)
}

// TestPublisherStreamCloseInterruptsAnInFlightTargetWrite pins the cleanup half
// of the same defect: cancellation has to be able to close the target while a
// write to it is already in flight, or teardown waits out the wedge.
func TestPublisherStreamCloseInterruptsAnInFlightTargetWrite(t *testing.T) {
	target := newWedgedConn()
	stream := newHelperPublisherStream(func() {})
	require.NoError(t, stream.attach(target))
	require.NoError(t, stream.write([]byte("parked")))
	target.awaitInflightWrite(t)

	completesWithin(t, 5*time.Second, "close of a wedged stream", stream.close)
	select {
	case <-target.closed:
	default:
		t.Fatal("close must have closed the target connection")
	}
	// Once closed the stream refuses further work rather than queueing it.
	require.ErrorIs(t, stream.write([]byte("after close")), errPublisherStreamClosed)
}

// TestPublisherStreamsCloseAllDoesNotHoldTheRegistryMutex proves the registry
// stays usable while a wedged stream is being closed, so teardown of one stream
// cannot block admission or lookup of the others.
func TestPublisherStreamsCloseAllDoesNotHoldTheRegistryMutex(t *testing.T) {
	registry := &publisherStreams{items: make(map[uint64]*helperPublisherStream)}
	target := newWedgedConn()
	t.Cleanup(func() { _ = target.Close() })
	wedged := newHelperPublisherStream(func() {})
	require.True(t, registry.add(1, wedged))
	require.NoError(t, wedged.attach(target))
	require.NoError(t, wedged.write([]byte("parked")))
	target.awaitInflightWrite(t)

	completesWithin(t, 5*time.Second, "closeAll with a wedged target", registry.closeAll)
	completesWithin(t, 5*time.Second, "registry reuse after closeAll", func() {
		require.True(t, registry.add(2, newHelperPublisherStream(func() {})))
	})
}

// TestPublisherStreamBoundsAWedgedTargetsBacklog pins the bound: a target that
// never drains cannot make one stream consume memory without limit, and the
// stream that owns it is the one that fails.
func TestPublisherStreamBoundsAWedgedTargetsBacklog(t *testing.T) {
	target := newWedgedConn()
	t.Cleanup(func() { _ = target.Close() })
	stream := newHelperPublisherStream(func() {})
	require.NoError(t, stream.attach(target))
	require.NoError(t, stream.write([]byte("parked")))
	target.awaitInflightWrite(t)

	chunk := make([]byte, 64<<10)
	var queueErr error
	completesWithin(t, 10*time.Second, "filling a wedged stream's bound", func() {
		for range (publisherPendingLimit / len(chunk)) + 2 {
			if err := stream.write(chunk); err != nil {
				queueErr = err
				return
			}
		}
	})
	require.ErrorIs(t, queueErr, errPublisherStreamBacklog, "a wedged target must be bounded, not unbounded")
}

// startWedgingTarget serves a target whose first accepted connection is parked
// and never read, while every later connection echoes. One wedged peer on an
// otherwise healthy route.
func startWedgingTarget(t *testing.T) (net.Listener, <-chan net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	parked := make(chan net.Conn, 1)
	go func() {
		first := true
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if first {
				first = false
				parked <- conn
				continue
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				for {
					n, readErr := c.Read(buf)
					if n > 0 {
						if _, writeErr := c.Write(append([]byte("reply:"), buf[:n]...)); writeErr != nil {
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
	return listener, parked
}

// floodWedgedStream pushes more at one stream than ordinary socket buffers can
// absorb, so its bytes genuinely have nowhere to go, while staying under the
// stream's own bound so the assertion is about isolation, not about the bound.
func floodWedgedStream(t *testing.T, h *publisherHarness, stream uint64) {
	t.Helper()
	chunk := make([]byte, 64<<10)
	for range 12 {
		h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: stream, Payload: chunk})
	}
}

// TestRunPublisherServesOtherStreamsWhileOneTargetIsWedged is the end-to-end
// claim on the production read loop: one route connection whose peer stopped
// reading must not stop the channel from opening and serving another.
func TestRunPublisherServesOtherStreamsWhileOneTargetIsWedged(t *testing.T) {
	listener, parked := startWedgingTarget(t)
	h := startPublisherHarness(t, "tcp://"+listener.Addr().String())

	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	select {
	case conn := <-parked:
		t.Cleanup(func() { _ = conn.Close() })
	case <-time.After(10 * time.Second):
		t.Fatal("wedged stream never reached the target")
	}
	floodWedgedStream(t, h, 1)

	// The read loop must still be serving the channel.
	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 2})
	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 2, Payload: []byte("healthy")})
	payload, _ := h.awaitData(t, 2, 0)
	require.Equal(t, "reply:healthy", payload, "a wedged target must not stall an unrelated stream")

	// Control frames for other streams keep being honoured too.
	h.send(t, routebroker.Frame{Kind: routebroker.KindClose, Stream: 2})
	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 3})
	h.send(t, routebroker.Frame{Kind: routebroker.KindData, Stream: 3, Payload: []byte("after-close")})
	payload, _ = h.awaitData(t, 3, 0)
	require.Equal(t, "reply:after-close", payload, "control frames must keep flowing past a wedged target")
	h.requireChannelAlive(t)
}

// TestRunPublisherCancellationClosesAWedgedTargetPromptly proves teardown is not
// hostage to the wedge: cancelling the context must end RunPublisher and close
// the target rather than waiting for the target to start reading again.
func TestRunPublisherCancellationClosesAWedgedTargetPromptly(t *testing.T) {
	listener, parked := startWedgingTarget(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h := startPublisherHarnessWithContext(t, ctx, "tcp://"+listener.Addr().String())

	h.send(t, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	var target net.Conn
	select {
	case target = <-parked:
		t.Cleanup(func() { _ = target.Close() })
	case <-time.After(10 * time.Second):
		t.Fatal("stream never reached the target")
	}
	floodWedgedStream(t, h, 1)

	cancel()
	select {
	case <-h.done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not end RunPublisher while its target was wedged")
	}

	// The parked peer sees the target connection actually closed, rather than
	// left open with nobody serving it.
	require.NoError(t, target.SetReadDeadline(time.Now().Add(10*time.Second)))
	buf := make([]byte, 64<<10)
	var readErr error
	for readErr == nil {
		_, readErr = target.Read(buf)
	}
	require.Error(t, readErr, "publisher teardown must close the wedged target connection")
	require.False(t, errors.Is(readErr, os.ErrDeadlineExceeded), "target close must not have to wait out a deadline: %v", readErr)
}
