package ask

import (
	"io"
	"sync"
	"time"
)

// streamSpinner is the transient "working…" indicator `tclaude ask` shows on
// stderr while a streamed answer is still on its way — so a human watching a TTY
// sees the harness is alive during the model's "thinking" pause before any text
// arrives, and (briefly, under smoothing) while the first token buffers.
//
// It implements harness.StreamStatus: the stream filter calls FirstChunk on the
// first token and Stop immediately before the first character prints; runAsk
// start()s it before the run and stop()s it (idempotently) as end-of-run
// cleanup. It draws a single line with a carriage return + clear-to-end-of-line
// and erases that line completely on Stop, so stdout (the answer itself) is never
// touched — the indicator lives entirely on stderr.
//
// Its methods are safe for concurrent use: the filter drives it from its
// stdout-parsing and pacing goroutines while the animation runs on its own.
type streamSpinner struct {
	w    io.Writer     // the stderr TTY
	tick time.Duration // frame interval

	mu      sync.Mutex
	phase   spinnerPhase
	frame   int
	stopped bool
	quit    chan struct{} // closed by Stop to end the animation
	done    chan struct{} // closed by the run goroutine when it exits
}

type spinnerPhase int

const (
	phaseWaiting   spinnerPhase = iota // no token yet — the model is thinking / using tools
	phaseBuffering                     // first token received, about to print
)

// spinnerFrames is a braille dot cycle — compact, smooth, and widely supported.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

func newStreamSpinner(w io.Writer) *streamSpinner {
	return &streamSpinner{
		w:    w,
		tick: 100 * time.Millisecond,
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// start launches the animation: it draws the first frame immediately (so the
// indicator appears at once, not a tick later) and then refreshes until Stop.
func (s *streamSpinner) start() { go s.run() }

func (s *streamSpinner) run() {
	defer close(s.done)
	t := time.NewTicker(s.tick)
	defer t.Stop()
	if !s.drawFrame() {
		return
	}
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
			if !s.drawFrame() {
				return
			}
		}
	}
}

// drawFrame renders the current frame; it returns false once the spinner is
// stopped so the run loop can exit without drawing over the erase.
func (s *streamSpinner) drawFrame() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	frame := spinnerFrames[s.frame%len(spinnerFrames)]
	s.frame++
	msg := "waiting for response"
	if s.phase == phaseBuffering {
		msg = "receiving"
	}
	// \r returns to column 0; \033[K clears any residue from a longer prior
	// message. No newline — the line is reused each frame and erased on Stop.
	_, _ = io.WriteString(s.w, "\r"+string(frame)+" "+msg+"…\033[K")
	return true
}

// FirstChunk switches the indicator to the "buffering" phase — the first token
// has arrived but (under smoothing) the first character hasn't printed yet.
func (s *streamSpinner) FirstChunk() {
	s.mu.Lock()
	if !s.stopped {
		s.phase = phaseBuffering
	}
	s.mu.Unlock()
}

// Stop erases the indicator line and halts the animation. Idempotent: the filter
// calls it just before the first character prints, and runAsk calls it again as
// end-of-run cleanup (covering a turn that printed nothing at all).
func (s *streamSpinner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	_, _ = io.WriteString(s.w, "\r\033[K") // erase the whole line
	close(s.quit)
}

// wait blocks until the animation goroutine has exited (so stderr sees no more
// frames after Stop). Only valid after start.
func (s *streamSpinner) wait() { <-s.done }
