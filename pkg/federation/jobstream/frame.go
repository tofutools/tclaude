// Package jobstream frames bounded stdout/stderr bytes inside an authenticated
// federation stream. Offsets are per channel and survive follow reconnects;
// worker exit status remains in the separately verified completed-log result.
package jobstream

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
)

const (
	Stdout     byte = 1
	Stderr     byte = 2
	MaxChunk        = 32 << 10
	MaxChannel      = 4 << 20
	headerSize      = 13
	// One-byte writes are valid: account for their worst-case header overhead.
	MaxEncodedBytes = 2 * MaxChannel * (headerSize + 1)
)

var ErrFrame = errors.New("invalid job output frame")
var ErrGap = errors.New("job output offset gap or partial replay")

type Frame struct {
	Channel byte
	Offset  uint64
	Data    []byte
}

func validChannel(c byte) bool { return c == Stdout || c == Stderr }

// Encoder serializes the two subprocess output pipes and caps retained bytes.
// A full channel still consumes writes so pipe draining and normal cancellation
// remain owned by the subprocess runner, which reports output-limit exit 125.
type Encoder struct {
	mu      sync.Mutex
	out     io.Writer
	offsets [3]uint64
}

func NewEncoder(out io.Writer) *Encoder     { return &Encoder{out: out} }
func (e *Encoder) Channel(c byte) io.Writer { return channelWriter{encoder: e, channel: c} }

type channelWriter struct {
	encoder *Encoder
	channel byte
}

func (w channelWriter) Write(p []byte) (int, error) { return w.encoder.Write(w.channel, p) }
func (e *Encoder) Write(channel byte, p []byte) (int, error) {
	if !validChannel(channel) {
		return 0, ErrFrame
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	original := len(p)
	remaining := MaxChannel - int(e.offsets[channel])
	if len(p) > remaining {
		p = p[:remaining]
	}
	for len(p) > 0 {
		n := min(len(p), MaxChunk)
		var head [headerSize]byte
		head[0] = channel
		binary.BigEndian.PutUint64(head[1:9], e.offsets[channel])
		binary.BigEndian.PutUint32(head[9:], uint32(n))
		if err := writeAll(e.out, head[:]); err != nil {
			return 0, err
		}
		if err := writeAll(e.out, p[:n]); err != nil {
			return 0, err
		}
		e.offsets[channel] += uint64(n)
		p = p[n:]
	}
	return original, nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// Read allocates only after checking the declared channel, offset and length.
// EOF is valid only at a frame boundary; the outer stream requires authenticated
// FIN, so a partial frame or unauthenticated transport end remains an error.
func Read(r io.Reader) (Frame, error) {
	var f Frame
	var head [headerSize]byte
	if _, e := io.ReadFull(r, head[:]); e != nil {
		return f, e
	}
	f.Channel = head[0]
	f.Offset = binary.BigEndian.Uint64(head[1:9])
	n := uint64(binary.BigEndian.Uint32(head[9:]))
	if !validChannel(f.Channel) || n == 0 || n > MaxChunk || f.Offset > MaxChannel || n > MaxChannel-f.Offset {
		return f, ErrFrame
	}
	f.Data = make([]byte, n)
	_, e := io.ReadFull(r, f.Data)
	return f, e
}

// Cursor accepts complete duplicate frames after reconnect, never overlapping
// bytes or gaps. Advance only after the local consumer accepts the bytes.
type Cursor struct {
	Stdout uint64 `json:"stdout"`
	Stderr uint64 `json:"stderr"`
}

func (c *Cursor) Apply(f Frame, out, errOut io.Writer) error {
	if !validChannel(f.Channel) || len(f.Data) == 0 || len(f.Data) > MaxChunk || f.Offset > MaxChannel || uint64(len(f.Data)) > MaxChannel-f.Offset {
		return ErrFrame
	}
	offset := &c.Stdout
	dest := out
	if f.Channel == Stderr {
		offset = &c.Stderr
		dest = errOut
	}
	end := f.Offset + uint64(len(f.Data))
	if end <= *offset {
		return nil
	}
	if f.Offset != *offset {
		return ErrGap
	}
	if e := writeAll(dest, f.Data); e != nil {
		return e
	}
	*offset = end
	return nil
}
