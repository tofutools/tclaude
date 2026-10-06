package routebroker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// Flow control.
//
// Without it a route stream has only fixed bounds: a receiver slower than its
// sender eventually overflows one of them and the stream (or, worse, the
// whole channel) is torn down. With it, each direction of a stream carries
// credit in the style of HTTP/2 and SSH: a sender may have at most the
// receiver's granted window of DATA bytes outstanding, and the receiver
// grants more with WINDOW frames only as it actually delivers bytes to its
// destination. A slow destination therefore stops the sender reading its own
// source, and TCP carries the backpressure the rest of the way.
//
// Flow control is negotiated per channel and then decided per stream. An
// endpoint that implements it says so when it attaches, with its receive
// window (PublisherAuth and ConsumerAuth FlowWindow); the broker enables it on a stream only when both
// of that stream's channels do, and tells each end by putting
// OpenFlowControl in the OPEN it sends the publisher and in the OPEN_OK it
// forwards to the consumer. An endpoint that never sees the marker runs the
// stream exactly as before, so old and new helpers interoperate.
//
// Both directions start with InitialWindow of credit, implicitly. A receiver
// that wants a larger window grants the difference up front. The broker
// tracks the same credit and treats a sender that exceeds it as a protocol
// violation, and never holds more than a receiver's declared window queued
// toward it, so a misbehaving endpoint on either side fails its own channel
// instead of growing agentd's memory.

// InitialWindow is each direction's implicit starting credit on a
// flow-controlled stream.
const InitialWindow = 256 << 10

// MaxWindow bounds a receiver's window and therefore any one direction's
// outstanding credit.
const MaxWindow = 16 << 20

// OpenFlowControl is the OPEN / OPEN_OK payload that marks a stream as
// flow-controlled. It is only ever sent to an endpoint that advertised
// support, so older endpoints never see it.
const OpenFlowControl = "flow"

// ErrFlowViolation is a sender exceeding the credit it was granted.
var ErrFlowViolation = errors.New("route stream exceeded its flow-control window")

// errWindowClosed is returned by SendWindow.Wait once the stream ended.
var errWindowClosed = errors.New("route stream flow window closed")

// IsFlowOpen reports whether an OPEN or OPEN_OK payload enables flow control.
func IsFlowOpen(payload []byte) bool { return string(payload) == OpenFlowControl }

// WindowFrame grants n more bytes of credit on stream.
func WindowFrame(stream uint64, n int) Frame {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, uint32(n))
	return Frame{Kind: KindWindow, Stream: stream, Payload: p}
}

// ParseWindow decodes a WINDOW payload.
func ParseWindow(payload []byte) (int, error) {
	if len(payload) != 4 {
		return 0, fmt.Errorf("%w: WINDOW payload must be 4 bytes", ErrProtocol)
	}
	n := binary.BigEndian.Uint32(payload)
	if n == 0 || n > MaxWindow {
		return 0, fmt.Errorf("%w: WINDOW increment %d out of range", ErrProtocol, n)
	}
	return int(n), nil
}

// ClampWindow returns a usable receive window for a configured size: never
// below InitialWindow (a receiver cannot take back implicit credit) and
// never above MaxWindow.
func ClampWindow(n int) int {
	return min(max(n, InitialWindow), MaxWindow)
}

// credit is one direction's outstanding credit as the broker sees it: n is
// granted and unspent (guarded by the broker mutex), queued is spent but
// still in the broker's queue toward the receiver. Together they never
// exceed the receiver's declared window, so a receiver that grants
// without reading cannot make the broker hold more than that.
type credit struct {
	window int
	n      int
	queued atomic.Int64
}

func newCredit(window int) *credit {
	return &credit{window: ClampWindow(window), n: InitialWindow}
}

func (c *credit) spend(n int) error {
	if n > c.n {
		return ErrFlowViolation
	}
	c.n -= n
	c.queued.Add(int64(n))
	return nil
}

func (c *credit) grant(n int) error {
	if int64(c.n+n)+c.queued.Load() > int64(c.window) {
		return ErrFlowViolation
	}
	c.n += n
	return nil
}

// dequeued releases n queued bytes; nil-safe.
func (c *credit) dequeued(n int) {
	if c != nil {
		c.queued.Add(-int64(n))
	}
}

// dataCredit is the credit a frame occupies while queued: DATA on a
// flow-controlled stream only.
func dataCredit(c *credit, f Frame) *credit {
	if f.Kind == KindData {
		return c
	}
	return nil
}

// SendWindow is a sender's credit for one stream direction. One goroutine
// sends; any goroutine may grant or close.
type SendWindow struct {
	mu     sync.Mutex
	cond   *sync.Cond
	n      int
	closed bool
}

// NewSendWindow starts with InitialWindow of credit.
func NewSendWindow() *SendWindow {
	w := &SendWindow{n: InitialWindow}
	w.cond = sync.NewCond(&w.mu)
	return w
}

// Wait blocks until there is credit and returns how much, at most limit. It
// does not spend it; call Spend with what was actually sent.
func (w *SendWindow) Wait(limit int) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.n == 0 && !w.closed {
		w.cond.Wait()
	}
	if w.closed {
		return 0, errWindowClosed
	}
	return min(w.n, limit), nil
}

// Spend consumes n bytes of credit obtained from Wait.
func (w *SendWindow) Spend(n int) {
	w.mu.Lock()
	w.n -= n
	w.mu.Unlock()
}

// Grant adds credit from a WINDOW frame.
func (w *SendWindow) Grant(n int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.n+n > MaxWindow {
		return ErrFlowViolation
	}
	w.n += n
	w.cond.Broadcast()
	return nil
}

// Close releases a sender blocked in Wait.
func (w *SendWindow) Close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}

// RecvWindow is a receiver's side of one stream direction: it checks that
// the sender stays within credit and decides when to grant more.
type RecvWindow struct {
	mu       sync.Mutex
	window   int
	credit   int // granted and not yet received
	consumed int // delivered and not yet granted back
}

// NewRecvWindow returns a receive window of size (clamped) and the credit to
// grant up front beyond InitialWindow, which may be zero.
func NewRecvWindow(size int) (*RecvWindow, int) {
	size = ClampWindow(size)
	return &RecvWindow{window: size, credit: size}, size - InitialWindow
}

// Received accounts n bytes arriving from the sender.
func (r *RecvWindow) Received(n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n > r.credit {
		return ErrFlowViolation
	}
	r.credit -= n
	return nil
}

// Consumed accounts n bytes delivered to the destination and returns the
// credit to grant now, or zero. Grants are batched to a quarter window so a
// stream of small writes does not become a stream of WINDOW frames.
func (r *RecvWindow) Consumed(n int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consumed += n
	if r.consumed < r.window/4 {
		return 0
	}
	g := r.consumed
	r.consumed = 0
	r.credit += g
	return g
}
