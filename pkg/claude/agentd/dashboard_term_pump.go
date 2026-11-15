package agentd

import (
	"bytes"
	"io"
	"time"
)

// PTY output reaches the browser in frames, not in reads. A wheel notch over
// Claude Code's fullscreen view makes tmux rewrite the whole screen, about 9 KB
// on a 160x50 terminal, which a 4 KiB read loop sent as five WebSocket
// messages. Each message is its own trip through cloudflared on a phone, and
// xterm.js could paint between them. pumpPTYFrames reads in larger pieces and
// sends what is already waiting as one message, without waiting for anything
// more, so a keystroke's echo goes out as soon as it is read.
//
// tmux brackets each redraw in synchronized output (DEC 2026) for a client
// with the sync feature (webTerminalAttachCmd asks for it), and the end of a
// frame is then known: output that opened a frame and has not closed it is
// held until it does, so a frame is one message. A message is held at most
// ptyFrameHold in all, from its first read: output that keeps trickling into
// a frame never closed goes out on time instead of waiting on itself.
const (
	ptyReadSize  = 32 << 10
	ptyFrameMax  = 256 << 10
	ptyFrameHold = 50 * time.Millisecond
)

var (
	syncStart = []byte("\x1b[?2026h")
	syncEnd   = []byte("\x1b[?2026l")
)

// syncOpen reports whether out has opened a synchronized frame it has not
// closed yet.
func syncOpen(out []byte) bool {
	start := bytes.LastIndex(out, syncStart)
	return start >= 0 && start > bytes.LastIndex(out, syncEnd)
}

// pumpPTYFrames copies r to send, one call per frame, until r fails or send
// does. It returns r's error, or nil when send failed first.
func pumpPTYFrames(r io.Reader, send func([]byte) error) error {
	type chunk struct {
		data []byte
		err  error
	}
	chunks := make(chan chunk, 16)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			buf := make([]byte, ptyReadSize)
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case chunks <- chunk{data: buf[:n]}:
				case <-done:
					return
				}
			}
			if err != nil {
				select {
				case chunks <- chunk{err: err}:
				case <-done:
				}
				return
			}
		}
	}()

	var hold *time.Timer
	defer func() {
		if hold != nil {
			hold.Stop()
		}
	}()
	for {
		first := <-chunks
		if first.err != nil {
			return first.err
		}
		out := first.data
		var readErr error
		deadline := time.Now().Add(ptyFrameHold)
	gather:
		for len(out) < ptyFrameMax {
			// What is already read joins this message at no cost.
			select {
			case next := <-chunks:
				if next.err != nil {
					readErr = next.err
					break gather
				}
				out = append(out, next.data...)
				continue
			default:
			}
			if !syncOpen(out) {
				break
			}
			// A frame is still open: wait for its end, until the deadline.
			wait := time.Until(deadline)
			if wait <= 0 {
				break
			}
			if hold == nil {
				hold = time.NewTimer(wait)
			} else {
				hold.Reset(wait)
			}
			select {
			case next := <-chunks:
				hold.Stop()
				if next.err != nil {
					readErr = next.err
					break gather
				}
				out = append(out, next.data...)
			case <-hold.C:
				break gather
			}
		}
		if err := send(out); err != nil {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
