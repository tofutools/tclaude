// Package routebroker contains the platform-neutral data plane for dynamic
// group routes. It deliberately knows nothing about endpoint adapters: a
// publisher-side helper and a consumer-side helper attach authenticated
// channels and exchange opaque TCP stream frames through this package.
package routebroker

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	protocolMagic      = "TCR1"
	protocolVersion    = 1
	frameHeaderLen     = 20
	defaultFrameLength = 64 << 10
)

// Kind identifies one broker protocol frame. Payload bytes are never decoded
// by the broker. OPEN/OPEN_OK/OPEN_ERROR carry stream lifecycle metadata;
// DATA is forwarded byte-for-byte; HALF_CLOSE and CLOSE preserve TCP
// directionality and orderly shutdown.
type Kind uint8

const (
	KindOpen      Kind = 1
	KindOpenOK    Kind = 2
	KindOpenError Kind = 3
	KindData      Kind = 4
	KindHalfClose Kind = 5
	KindClose     Kind = 6
	KindPing      Kind = 7
	KindPong      Kind = 8
)

func (k Kind) valid() bool {
	switch k {
	case KindOpen, KindOpenOK, KindOpenError, KindData, KindHalfClose,
		KindClose, KindPing, KindPong:
		return true
	default:
		return false
	}
}

// Frame is one bounded wire message. The broker does not retain DATA frames
// after forwarding them, so callers must treat Payload as owned by the frame.
type Frame struct {
	Kind    Kind
	Stream  uint64
	Payload []byte
}

var (
	ErrProtocol        = errors.New("route broker protocol error")
	ErrFrameTooLarge   = errors.New("route broker frame is too large")
	ErrInvalidStreamID = errors.New("route broker stream id is invalid")
)

// OPEN_ERROR payloads are a closed set of stable tokens rather than free text,
// so a consumer can tell a condition that a later reopen clears from one that
// it never will. They are part of the wire contract; do not reword them.
const (
	OpenErrorPublisherUnavailable     = "publisher unavailable"
	OpenErrorTargetUnavailable        = "publisher target unavailable"
	OpenErrorDuplicateStream          = "duplicate stream id"
	OpenErrorDuplicatePublisherStream = "duplicate publisher stream"
	OpenErrorRouteLimit               = "route connection limit"
	OpenErrorAgentLimit               = "agent connection limit"
)

// OpenErrorIsTransient reports whether reopening the stream can succeed later
// without anything else changing. Only the publisher channel's absence
// qualifies: that is a daemon/helper lifecycle state which resolves on its own
// when the publisher reattaches. A refused open — limits, duplicate IDs — will
// be refused again, and an unreachable target is the publishing application's
// own state, which the consumer's client should see as a failed connect rather
// than a hang.
func OpenErrorIsTransient(payload []byte) bool {
	return string(payload) == OpenErrorPublisherUnavailable
}

// MaxFramePayload is the default upper bound for one frame's opaque payload.
// It is intentionally small enough that one malicious peer cannot allocate a
// large object per stream. Configured brokers may choose a lower bound.
const MaxFramePayload = defaultFrameLength

// ReadFrame reads one frame and rejects unknown versions, kinds, invalid
// stream identifiers and lengths above maxPayload. A maxPayload <= 0 uses the
// package default.
func ReadFrame(r io.Reader, maxPayload int) (Frame, error) {
	if maxPayload <= 0 {
		maxPayload = defaultFrameLength
	}
	var header [frameHeaderLen]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	if string(header[:4]) != protocolMagic || header[4] != protocolVersion {
		return Frame{}, fmt.Errorf("%w: unsupported header", ErrProtocol)
	}
	kind := Kind(header[5])
	if !kind.valid() {
		return Frame{}, fmt.Errorf("%w: unknown frame kind %d", ErrProtocol, kind)
	}
	if binary.BigEndian.Uint16(header[6:8]) != 0 {
		return Frame{}, fmt.Errorf("%w: non-zero reserved bits", ErrProtocol)
	}
	stream := binary.BigEndian.Uint64(header[8:16])
	length := binary.BigEndian.Uint32(header[16:20])
	if int64(length) > int64(maxPayload) {
		return Frame{}, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, length, maxPayload)
	}
	if stream == 0 && kind != KindPing && kind != KindPong {
		return Frame{}, ErrInvalidStreamID
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, err
	}
	return Frame{Kind: kind, Stream: stream, Payload: payload}, nil
}

// WriteFrame writes one complete frame. It does not queue or retain the
// payload. Callers that share a net.Conn must serialize calls themselves.
func WriteFrame(w io.Writer, frame Frame, maxPayload int) error {
	if maxPayload <= 0 {
		maxPayload = defaultFrameLength
	}
	if !frame.Kind.valid() {
		return fmt.Errorf("%w: unknown frame kind %d", ErrProtocol, frame.Kind)
	}
	if frame.Stream == 0 && frame.Kind != KindPing && frame.Kind != KindPong {
		return ErrInvalidStreamID
	}
	if len(frame.Payload) > maxPayload {
		return fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(frame.Payload), maxPayload)
	}
	var header [frameHeaderLen]byte
	copy(header[:4], protocolMagic)
	header[4] = protocolVersion
	header[5] = byte(frame.Kind)
	binary.BigEndian.PutUint64(header[8:16], frame.Stream)
	binary.BigEndian.PutUint32(header[16:20], uint32(len(frame.Payload)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, frame.Payload)
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if n > 0 {
			p = p[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
