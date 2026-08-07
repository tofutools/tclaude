package copilotapi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MaxFrameBytes caps a single inbound message. Copilot event payloads are
// large — a turn's worth of streaming deltas and tool output runs well past a
// megabyte — but an unbounded Content-Length lets a wedged or hostile peer
// make us allocate arbitrarily, so the limit is generous rather than absent.
const MaxFrameBytes = 64 << 20

// maxHeaderBytes caps a frame's whole header block. Real headers are a single
// short Content-Length line, so this is far above anything legitimate; it
// exists to stop a peer streaming header lines forever, which the body limit
// says nothing about.
const maxHeaderBytes = 16 << 10

// readerBufferBytes sizes the read buffer, and with it the longest header line
// we will accept. It must stay comfortably above one Content-Length line.
const readerBufferBytes = 4 << 10

// ErrFrameTooLarge reports a frame whose declared length exceeds
// [MaxFrameBytes]. It is terminal: the stream cannot be resynchronised,
// because skipping the body would require reading the bytes we refused to
// allocate for.
var ErrFrameTooLarge = errors.New("copilotapi: frame exceeds maximum size")

// ErrHeaderTooLarge reports a frame header that overruns either the per-line
// buffer or [maxHeaderBytes]. Also terminal, for the same reason.
var ErrHeaderTooLarge = errors.New("copilotapi: frame header exceeds maximum size")

// newFrameReader wraps a connection with the buffer size readFrame expects.
func newFrameReader(reader io.Reader) *bufio.Reader {
	return bufio.NewReaderSize(reader, readerBufferBytes)
}

// readFrame reads one Content-Length-framed message body.
//
// Headers are terminated by a blank line and matched case-insensitively;
// anything other than Content-Length (Copilot sends none today, but the LSP
// framing allows Content-Type) is skipped rather than rejected.
//
// Header reads go through ReadSlice rather than ReadString so a peer that
// never sends a newline is bounded by the reader's existing buffer. ReadString
// would grow its own buffer to hold the line first and only report the
// oversize afterwards, which is too late to have prevented the allocation.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	length := -1
	headerBytes := 0
	for {
		line, err := reader.ReadSlice('\n')
		headerBytes += len(line)
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return nil, fmt.Errorf("%w: header line exceeds %d bytes", ErrHeaderTooLarge, reader.Size())
			}
			// A clean EOF exactly at a frame boundary is the peer hanging up
			// between messages, which callers treat as server-gone rather
			// than corruption. Mid-header it is a truncated frame.
			if errors.Is(err, io.EOF) && len(line) == 0 && length < 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("copilotapi: read frame header: %w", err)
		}
		if headerBytes > maxHeaderBytes {
			return nil, fmt.Errorf("%w: header block exceeds %d bytes", ErrHeaderTooLarge, maxHeaderBytes)
		}
		trimmed := strings.TrimRight(string(line), "\r\n")
		if trimmed == "" {
			break
		}
		name, value, found := strings.Cut(trimmed, ":")
		if !found {
			return nil, fmt.Errorf("copilotapi: malformed frame header %q", trimmed)
		}
		if !strings.EqualFold(strings.TrimSpace(name), "content-length") {
			continue
		}
		length, err = strconv.Atoi(strings.TrimSpace(value))
		if err != nil || length < 0 {
			return nil, fmt.Errorf("copilotapi: invalid Content-Length %q", strings.TrimSpace(value))
		}
	}
	if length < 0 {
		return nil, errors.New("copilotapi: frame header missing Content-Length")
	}
	if length > MaxFrameBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("copilotapi: read frame body: %w", err)
	}
	return body, nil
}

// writeFrame writes one Content-Length-framed message body.
func writeFrame(writer io.Writer, body []byte) error {
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(writer, header); err != nil {
		return fmt.Errorf("copilotapi: write frame header: %w", err)
	}
	if _, err := writer.Write(body); err != nil {
		return fmt.Errorf("copilotapi: write frame body: %w", err)
	}
	return nil
}
