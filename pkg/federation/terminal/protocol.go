// Package terminal frames a bounded, flow-controlled terminal over an encrypted
// federation stream. Credits are returned only after delivery to the terminal
// or pane, rather than after an intermediate relay buffers the bytes.
package terminal

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	Output byte = iota + 1
	Input
	Resize
	Credit
	Closed
	MaxPayload = 16 << 10
	WindowSize = 256 << 10
)

var ErrProtocol = errors.New("invalid terminal frame or flow-control credit")

type Frame struct {
	Kind byte
	Data []byte
}

func Encode(f Frame) ([]byte, error) {
	if f.Kind < Output || f.Kind > Closed || len(f.Data) > MaxPayload {
		return nil, ErrProtocol
	}
	out := make([]byte, 5+len(f.Data))
	out[0] = f.Kind
	binary.BigEndian.PutUint32(out[1:], uint32(len(f.Data)))
	copy(out[5:], f.Data)
	return out, nil
}
func Decode(p []byte) (Frame, error) {
	if len(p) < 5 || int(binary.BigEndian.Uint32(p[1:5])) != len(p)-5 || len(p) > MaxPayload+5 || p[0] < Output || p[0] > Closed {
		return Frame{}, ErrProtocol
	}
	return Frame{Kind: p[0], Data: p[5:]}, nil
}
func Read(r io.Reader) (Frame, error) {
	h := make([]byte, 5)
	if _, err := io.ReadFull(r, h); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxPayload {
		return Frame{}, ErrProtocol
	}
	p := make([]byte, 5+int(n))
	copy(p, h)
	if _, err := io.ReadFull(r, p[5:]); err != nil {
		return Frame{}, err
	}
	return Decode(p)
}
func Write(w io.Writer, f Frame) error {
	p, err := Encode(f)
	if err != nil {
		return err
	}
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
func Number(n int) []byte { p := make([]byte, 4); binary.BigEndian.PutUint32(p, uint32(n)); return p }
func ParseNumber(p []byte) (int, error) {
	if len(p) != 4 {
		return 0, ErrProtocol
	}
	n := binary.BigEndian.Uint32(p)
	if n == 0 || n > WindowSize {
		return 0, ErrProtocol
	}
	return int(n), nil
}
func Size(cols, rows int) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint16(p, uint16(cols))
	binary.BigEndian.PutUint16(p[2:], uint16(rows))
	return p
}
func ParseSize(p []byte) (int, int, error) {
	if len(p) != 4 {
		return 0, 0, ErrProtocol
	}
	c, r := int(binary.BigEndian.Uint16(p)), int(binary.BigEndian.Uint16(p[2:]))
	if c < 1 || r < 1 || c > 1000 || r > 1000 {
		return 0, 0, ErrProtocol
	}
	return c, r, nil
}

// Window bounds a sender's outstanding data. Only the producer takes credit;
// a concurrent reader returns credit after the destination consumed the data.
type Window struct {
	mu     sync.Mutex
	cond   *sync.Cond
	credit int
	closed bool
}

func NewWindow() *Window { w := &Window{credit: WindowSize}; w.cond = sync.NewCond(&w.mu); return w }
func (w *Window) Take(limit int) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for w.credit == 0 && !w.closed {
		w.cond.Wait()
	}
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	n := min(limit, w.credit)
	w.credit -= n
	return n, nil
}
func (w *Window) Grant(n int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n < 1 || w.credit+n > WindowSize {
		return ErrProtocol
	}
	w.credit += n
	w.cond.Broadcast()
	return nil
}
func (w *Window) Close() { w.mu.Lock(); w.closed = true; w.cond.Broadcast(); w.mu.Unlock() }
