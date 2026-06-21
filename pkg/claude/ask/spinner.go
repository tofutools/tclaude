package ask

import (
	"io"
	"sync"
	"time"
)

const (
	// stallThreshold is how long the visible answer must be quiet — no character
	// printed while the turn is still open — before the indicator re-appears
	// mid-stream. Long enough that normal token-to-token gaps (and, in smooth
	// mode, the pacer's own ~0.55s backlog) never flicker it; short enough to
	// reassure during a tool call.
	stallThreshold = 400 * time.Millisecond
	// spinnerTick is the animation frame interval.
	spinnerTick = 100 * time.Millisecond
)

// spinnerFrames is a braille dot cycle — compact, smooth, and widely supported.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// streamSpinner is the transient "working…" indicator `tclaude ask` shows on
// stderr while a streamed answer is still on its way. It covers BOTH the initial
// wait (before the first token — the model thinking) AND mid-stream stalls
// (Claude pauses to run a tool, then continues): it re-appears whenever the
// visible answer goes quiet for stallThreshold while the turn is still open, and
// erases itself the instant a character is about to print.
//
// It draws only to stderr; the answer is on stdout. Before the first character
// it draws at column 0 (\r); once the answer is on screen it draws INLINE,
// trailing the cursor, using a cursor save/restore (ESC 7 / ESC 8) so it sits
// just after the partial answer and erases back to exactly where the answer left
// off — never altering the answer's own text or layout.
//
// It implements harness.StreamStatus (BeforeOutput), which the filter calls just
// before each visible write; start()/Done()/wait() are the ask layer's lifecycle
// handles. Safe for concurrent use: the filter calls BeforeOutput from its parse
// and pacing goroutines while the animation runs on its own.
type streamSpinner struct {
	w     io.Writer
	clock func() time.Time // time.Now; swappable in tests

	mu          sync.Mutex
	frame       int
	everOutput  bool      // a visible character has been written (or is about to be)
	lastOutput  time.Time // when BeforeOutput last ran
	shown       bool      // a frame is currently on screen
	shownInline bool      // the on-screen frame was drawn inline (needs the ESC 8 erase)
	finished    bool
	quit        chan struct{} // closed by Done to end the animation
	done        chan struct{} // closed by run when it exits
}

func newStreamSpinner(w io.Writer) *streamSpinner {
	return &streamSpinner{
		w:     w,
		clock: time.Now,
		quit:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// start launches the animation: it draws the first frame at once (not a tick
// later) and then refreshes until Done.
func (s *streamSpinner) start() { go s.run() }

func (s *streamSpinner) run() {
	defer close(s.done)
	t := time.NewTicker(spinnerTick)
	defer t.Stop()
	if !s.tick() {
		return
	}
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			if !s.tick() {
				return
			}
		}
	}
}

// tick renders one animation step and reports whether to keep going. It shows
// the indicator while the answer is quiet (initially, or stalled mid-stream) and
// hides it while text is flowing.
func (s *streamSpinner) tick() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.finished:
		return false
	case !s.everOutput:
		s.drawLocked(false) // initial wait, at column 0
	case s.clock().Sub(s.lastOutput) >= stallThreshold:
		s.drawLocked(true) // mid-stream stall, inline after the answer
	default:
		s.hideLocked() // text is flowing — stay out of the way
	}
	return true
}

// BeforeOutput hides the indicator (if shown) and resets the idle timer, so the
// caller can write a visible character without the indicator overlapping it. It
// runs on the filter's write path, immediately before each stdout write.
func (s *streamSpinner) BeforeOutput() {
	s.mu.Lock()
	s.hideLocked()
	s.everOutput = true
	s.lastOutput = s.clock()
	s.mu.Unlock()
}

// Done erases the indicator and stops the animation. Idempotent: runAsk calls it
// as end-of-run cleanup (covering a turn that printed nothing).
func (s *streamSpinner) Done() {
	s.mu.Lock()
	if !s.finished {
		s.finished = true
		s.hideLocked()
		close(s.quit)
	}
	s.mu.Unlock()
}

// wait blocks until the animation goroutine has exited. Only valid after start.
func (s *streamSpinner) wait() { <-s.done }

// drawLocked renders the current frame. inline selects mid-stream placement
// (trailing the answer via cursor save/restore) vs the initial column-0 spot.
// \033[K after the text clears any residue from a longer prior frame. Caller
// holds mu.
func (s *streamSpinner) drawLocked(inline bool) {
	frame := string(spinnerFrames[s.frame%len(spinnerFrames)])
	s.frame++
	msg := "waiting for response"
	if inline {
		msg = "working"
	}
	body := frame + " " + msg + "…\033[K"
	switch {
	case !s.shown && inline:
		// First inline frame: save the cursor at the answer's end (\0337 = ESC 7),
		// then draw after a separating space.
		_, _ = io.WriteString(s.w, "\0337 "+body)
	case !s.shown:
		// First initial frame: draw at column 0.
		_, _ = io.WriteString(s.w, "\r"+body)
	case s.shownInline:
		// Redraw inline: restore to the saved answer end (\0338 = ESC 8) first.
		_, _ = io.WriteString(s.w, "\0338 "+body)
	default:
		_, _ = io.WriteString(s.w, "\r"+body)
	}
	s.shown = true
	s.shownInline = inline
}

// hideLocked erases the on-screen frame, restoring the cursor to exactly where
// the answer left off (inline) or to column 0 (initial). Caller holds mu.
func (s *streamSpinner) hideLocked() {
	if !s.shown {
		return
	}
	if s.shownInline {
		_, _ = io.WriteString(s.w, "\0338\033[K") // restore to answer end, clear the inline spinner
	} else {
		_, _ = io.WriteString(s.w, "\r\033[K")
	}
	s.shown = false
}
