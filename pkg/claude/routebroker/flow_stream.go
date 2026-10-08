package routebroker

import (
	"errors"
	"io"
	"sync"
)

// FlowStream applies the route data plane's bounded credit protocol to one
// authenticated stream. Both ends must explicitly negotiate this protocol
// before constructing it. Credit is returned only when the application reads.
// One fixed byte ring also bounds memory when a peer sends tiny DATA frames.
type FlowStream struct {
	raw         io.ReadWriteCloser
	send        *SendWindow
	recv        *RecvWindow
	mu          sync.Mutex
	ready       *sync.Cond
	ring        []byte
	start, size int
	end         error
	closed      bool
	grant       int
	wake        chan struct{}
	done        chan struct{}
	writeMu     sync.Mutex
	dataMu      sync.Mutex
	writeClosed bool
}

func NewFlowStream(raw io.ReadWriteCloser) *FlowStream {
	recv, _ := NewRecvWindow(InitialWindow)
	c := &FlowStream{raw: raw, send: NewSendWindow(), recv: recv, ring: make([]byte, InitialWindow), wake: make(chan struct{}, 1), done: make(chan struct{})}
	c.ready = sync.NewCond(&c.mu)
	go c.receive()
	go c.returnCredit()
	return c
}

func (c *FlowStream) fail(err error) {
	c.mu.Lock()
	if c.end == nil {
		c.end = err
	}
	c.ready.Broadcast()
	c.mu.Unlock()
	c.send.Close()
}
func (c *FlowStream) frame(f Frame) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteFrame(c.raw, f, MaxFramePayload)
}
func (c *FlowStream) receive() {
	halfClosed := false
	for {
		f, err := ReadFrame(c.raw, MaxFramePayload)
		if err != nil {
			c.fail(err)
			_ = c.Close()
			return
		}
		if f.Stream != 1 {
			c.fail(ErrProtocol)
			_ = c.Close()
			return
		}
		switch f.Kind {
		case KindWindow:
			n, e := ParseWindow(f.Payload)
			if e == nil {
				e = c.send.Grant(n)
			}
			if e != nil {
				c.fail(e)
				_ = c.Close()
				return
			}
		case KindData:
			if halfClosed || len(f.Payload) == 0 {
				c.fail(ErrProtocol)
				_ = c.Close()
				return
			}
			if err := c.recv.Received(len(f.Payload)); err != nil {
				c.fail(err)
				_ = c.Close()
				return
			}
			c.mu.Lock()
			if c.size+len(f.Payload) > len(c.ring) {
				c.mu.Unlock()
				c.fail(ErrFlowViolation)
				_ = c.Close()
				return
			}
			at := (c.start + c.size) % len(c.ring)
			n := copy(c.ring[at:], f.Payload)
			copy(c.ring, f.Payload[n:])
			c.size += len(f.Payload)
			c.ready.Broadcast()
			c.mu.Unlock()
		case KindHalfClose:
			if halfClosed || len(f.Payload) != 0 {
				c.fail(ErrProtocol)
				_ = c.Close()
				return
			}
			halfClosed = true
			c.mu.Lock()
			if c.end == nil {
				c.end = io.EOF
			}
			c.ready.Broadcast()
			c.mu.Unlock()
		default:
			c.fail(ErrProtocol)
			_ = c.Close()
			return
		}
	}
}
func (c *FlowStream) returnCredit() {
	for {
		select {
		case <-c.done:
			return
		case <-c.wake:
			c.mu.Lock()
			n := c.grant
			c.grant = 0
			c.mu.Unlock()
			if n > 0 {
				if err := c.frame(WindowFrame(1, n)); err != nil {
					c.fail(err)
					_ = c.Close()
					return
				}
			}
		}
	}
}
func (c *FlowStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	for c.size == 0 && c.end == nil {
		c.ready.Wait()
	}
	if c.size == 0 {
		err := c.end
		c.mu.Unlock()
		return 0, err
	}
	n := min(len(p), c.size, len(c.ring)-c.start)
	copy(p, c.ring[c.start:c.start+n])
	c.start = (c.start + n) % len(c.ring)
	c.size -= n
	if grant := c.recv.Consumed(n); grant > 0 {
		c.grant += grant
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	c.mu.Unlock()
	return n, nil
}
func (c *FlowStream) Write(p []byte) (int, error) {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	if c.writeClosed {
		return 0, io.ErrClosedPipe
	}
	total := 0
	for len(p) > 0 {
		n, err := c.send.Wait(min(len(p), MaxFramePayload))
		if err != nil {
			return total, err
		}
		c.send.Spend(n)
		if err := c.frame(Frame{Kind: KindData, Stream: 1, Payload: p[:n]}); err != nil {
			c.fail(err)
			_ = c.Close()
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
func (c *FlowStream) CloseWrite() error {
	c.dataMu.Lock()
	defer c.dataMu.Unlock()
	if c.writeClosed {
		return nil
	}
	c.writeClosed = true
	return c.frame(Frame{Kind: KindHalfClose, Stream: 1})
}

// Done closes on full disconnect, including a failed underlying stream.
// A negotiated read half-close does not signal disconnect.
func (c *FlowStream) Done() <-chan struct{} { return c.done }

func (c *FlowStream) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	if c.end == nil || errors.Is(c.end, io.EOF) {
		c.end = io.ErrClosedPipe
	}
	c.ready.Broadcast()
	close(c.done)
	c.mu.Unlock()
	c.send.Close()
	return c.raw.Close()
}
