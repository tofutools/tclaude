package copilotapi

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	bodies := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1}`),
		[]byte(``),
		[]byte(`{"unicode":"pång — ✓"}`),
	}
	for _, body := range bodies {
		if err := writeFrame(&buffer, body); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	reader := bufio.NewReader(&buffer)
	for i, want := range bodies {
		got, err := readFrame(reader)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d = %q, want %q", i, got, want)
		}
	}
	if _, err := readFrame(reader); !errors.Is(err, io.EOF) {
		t.Errorf("after last frame err = %v, want io.EOF", err)
	}
}

func TestReadFrameHeaderTolerance(t *testing.T) {
	// Header names are case-insensitive and unrelated headers are skipped
	// rather than rejected, so a server that starts sending Content-Type does
	// not break us.
	raw := "content-length: 2\r\nContent-Type: application/vscode-jsonrpc\r\n\r\nhi"
	got, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("body = %q, want %q", got, "hi")
	}
}

func TestReadFrameRejectsBadHeaders(t *testing.T) {
	for name, raw := range map[string]string{
		"missing content-length": "Content-Type: x\r\n\r\nhi",
		"non-numeric length":     "Content-Length: abc\r\n\r\nhi",
		"negative length":        "Content-Length: -1\r\n\r\nhi",
		"no colon":               "ContentLength 2\r\n\r\nhi",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readFrame(bufio.NewReader(strings.NewReader(raw))); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	// The length is refused before any allocation, so a peer cannot make us
	// reserve an arbitrary buffer.
	raw := "Content-Length: 68719476736\r\n\r\n"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
}

func TestReadFrameTruncatedBody(t *testing.T) {
	raw := "Content-Length: 10\r\n\r\nshort"
	_, err := readFrame(bufio.NewReader(strings.NewReader(raw)))
	if err == nil {
		t.Fatal("expected an error for a truncated body")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
}
