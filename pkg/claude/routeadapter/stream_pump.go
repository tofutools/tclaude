package routeadapter

import (
	"errors"
	"net"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// Platform-neutral stream plumbing shared by the Linux helpers and the
// Darwin in-process adapter: the per-stream target pump and flow-control
// credit.

// flowChannel is a channel whose flow-control window is known up front, as
// for the Darwin adapter's in-process channels.
type flowChannel struct {
	net.Conn
	window int
}

func (c flowChannel) FlowWindow() int { return c.window }

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
	// done closes once the stream is closed.
	done chan struct{}
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
	s := &helperPublisherStream{fail: fail, done: make(chan struct{})}
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
	close(s.done)
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
