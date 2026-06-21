package ask

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestStreamSpinner_Render drives the frames synchronously (no goroutine) so the
// output is deterministic: it shows the waiting message, switches to the
// buffering message after FirstChunk, refuses to draw once Stopped, and the very
// last thing it writes is a line erase — so stdout (the answer) is never overlapped.
func TestStreamSpinner_Render(t *testing.T) {
	var buf bytes.Buffer
	s := newStreamSpinner(&buf)

	if !s.drawFrame() {
		t.Fatal("first frame should draw")
	}
	s.FirstChunk()
	if !s.drawFrame() {
		t.Fatal("buffering frame should draw")
	}
	s.Stop()
	if s.drawFrame() {
		t.Fatal("drawFrame after Stop must report stopped and not draw")
	}

	got := buf.String()
	if !strings.Contains(got, "waiting for response") {
		t.Fatalf("missing the waiting message: %q", got)
	}
	if !strings.Contains(got, "receiving") {
		t.Fatalf("missing the buffering message after FirstChunk: %q", got)
	}
	if !strings.HasSuffix(got, "\r\033[K") {
		t.Fatalf("spinner must end by erasing its line, got %q", got)
	}
	// Stop is idempotent — a second call (runAsk's end-of-run cleanup) is a no-op.
	s.Stop()
}

// TestStreamSpinner_Lifecycle exercises the real animation goroutine: start →
// FirstChunk → Stop → wait must not hang or race (run under -race in CI).
func TestStreamSpinner_Lifecycle(t *testing.T) {
	s := newStreamSpinner(io.Discard) // discard: the goroutine and Stop both write
	s.start()
	s.FirstChunk()
	s.Stop()
	s.wait() // must return promptly once Stop halts the animation
}
