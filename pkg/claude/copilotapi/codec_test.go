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

func TestReadFrameRejectsOversizeHeaderLine(t *testing.T) {
	// A peer that never sends a newline must be stopped by the read buffer
	// rather than allowed to grow one. io.MultiReader keeps the stream endless
	// so the test fails by hanging or exhausting memory if the bound is lost.
	endless := io.MultiReader(
		strings.NewReader("Content-Length: "),
		&repeatingReader{value: '9'},
	)
	_, err := readFrame(newFrameReader(endless))
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("err = %v, want ErrHeaderTooLarge", err)
	}
}

func TestReadFrameRejectsEndlessHeaderLines(t *testing.T) {
	// Each line is short enough to fit the buffer, so only the header-block
	// budget stops a peer that sends them forever.
	var header strings.Builder
	for header.Len() <= maxHeaderBytes {
		header.WriteString("X-Filler: padding\r\n")
	}
	header.WriteString("Content-Length: 2\r\n\r\nhi")
	_, err := readFrame(newFrameReader(strings.NewReader(header.String())))
	if !errors.Is(err, ErrHeaderTooLarge) {
		t.Fatalf("err = %v, want ErrHeaderTooLarge", err)
	}
}

// repeatingReader yields the same byte forever.
type repeatingReader struct{ value byte }

func (r *repeatingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.value
	}
	return len(p), nil
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
