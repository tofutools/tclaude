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

// ErrFrameTooLarge reports a frame whose declared length exceeds
// [MaxFrameBytes]. It is terminal: the stream cannot be resynchronised,
// because skipping the body would require reading the bytes we refused to
// allocate for.
var ErrFrameTooLarge = errors.New("copilotapi: frame exceeds maximum size")

// readFrame reads one Content-Length-framed message body.
//
// Headers are terminated by a blank line and matched case-insensitively;
// anything other than Content-Length (Copilot sends none today, but the LSP
// framing allows Content-Type) is skipped rather than rejected.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// A clean EOF exactly at a frame boundary is the peer hanging up
			// between messages, which callers treat as server-gone rather
			// than corruption. Mid-header it is a truncated frame.
			if errors.Is(err, io.EOF) && line == "" && length < 0 {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("copilotapi: read frame header: %w", err)
		}
		trimmed := strings.TrimRight(line, "\r\n")
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
