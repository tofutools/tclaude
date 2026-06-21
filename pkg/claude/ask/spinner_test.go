package ask

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// TestStreamSpinner_Render drives the frames synchronously with a fake clock, so
// the full lifecycle is deterministic: it shows the waiting message at column 0,
// erases it before the first character, stays hidden while output flows, then —
// after a stall past the threshold — re-appears INLINE (cursor-saved) with the
// working message and erases back to where the answer left off when output resumes.
func TestStreamSpinner_Render(t *testing.T) {
	var buf bytes.Buffer
	now := time.Unix(0, 0)
	s := newStreamSpinner(&buf)
	s.clock = func() time.Time { return now }

	// 1. Initial wait: drawn at column 0 with the waiting message.
	s.tick()
	if got := buf.String(); !strings.Contains(got, "waiting for response") || !strings.HasPrefix(got, "\r") {
		t.Fatalf("initial frame should draw the waiting message at column 0, got %q", got)
	}

	// 2. First visible byte imminent: BeforeOutput erases the column-0 frame.
	buf.Reset()
	s.BeforeOutput()
	if got := buf.String(); got != "\r\033[K" {
		t.Fatalf("BeforeOutput should erase the initial frame, got %q", got)
	}

	// 3. Output is recent: tick keeps the spinner hidden (no draw).
	buf.Reset()
	s.tick()
	if got := buf.String(); got != "" {
		t.Fatalf("while output is recent the spinner must stay hidden, got %q", got)
	}

	// 4. Stall: advance past the threshold → an INLINE working frame, preceded by
	//    a cursor save (ESC 7) so it can later erase back to the answer's end.
	buf.Reset()
	now = now.Add(stallThreshold)
	s.tick()
	if got := buf.String(); !strings.Contains(got, "working") || !strings.HasPrefix(got, "\0337") {
		t.Fatalf("a mid-stream stall should save the cursor (ESC 7) and show the working message, got %q", got)
	}

	// 5. A second stall frame redraws inline via cursor restore (ESC 8), not re-save.
	buf.Reset()
	s.tick()
	if got := buf.String(); !strings.HasPrefix(got, "\0338") {
		t.Fatalf("subsequent inline frames must restore the cursor (ESC 8), got %q", got)
	}

	// 6. Output resumes: BeforeOutput erases the inline frame via restore + clear,
	//    leaving the cursor exactly where the answer left off.
	buf.Reset()
	s.BeforeOutput()
	if got := buf.String(); got != "\0338\033[K" {
		t.Fatalf("resuming should erase the inline frame via ESC 8 + clear, got %q", got)
	}

	// 7. Done is idempotent.
	s.Done()
	s.Done()
}

// TestStreamSpinner_Lifecycle exercises the real animation goroutine: start →
// BeforeOutput → Done → wait must not hang or race (run under -race in CI).
func TestStreamSpinner_Lifecycle(t *testing.T) {
	s := newStreamSpinner(io.Discard) // discard: the goroutine and Done both write
	s.start()
	s.BeforeOutput()
	s.Done()
	s.wait() // must return promptly once Done halts the animation
}
