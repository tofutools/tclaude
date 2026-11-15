package agentd

import (
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pipeReads feeds pumpPTYFrames one write per PTY read, as a pty would.
type pipeReads struct{ reads chan []byte }

func (p pipeReads) Read(b []byte) (int, error) {
	data, ok := <-p.reads
	if !ok {
		return 0, io.EOF
	}
	return copy(b, data), nil
}

func startPump(t *testing.T) (chan<- []byte, <-chan string, <-chan error) {
	t.Helper()
	reads := make(chan []byte)
	sent := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		done <- pumpPTYFrames(pipeReads{reads}, func(frame []byte) error { sent <- string(frame); return nil })
	}()
	return reads, sent, done
}

func next(t *testing.T, sent <-chan string) string {
	t.Helper()
	select {
	case frame := <-sent:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("no frame sent")
		return ""
	}
}

func TestPumpPTYFramesSendsAKeystrokeEchoAtOnce(t *testing.T) {
	reads, sent, done := startPump(t)
	reads <- []byte("a")
	assert.Equal(t, "a", next(t, sent), "an echo goes as soon as it is read")
	close(reads)
	assert.ErrorIs(t, <-done, io.EOF)
}

func TestPumpPTYFramesHoldsAnOpenFrameUntilItEnds(t *testing.T) {
	reads, sent, done := startPump(t)
	reads <- []byte("\x1b[?2026hrow 1\r\n")
	reads <- []byte("row 2\r\n")
	reads <- []byte("row 3\x1b[?2026l")
	assert.Equal(t, "\x1b[?2026hrow 1\r\nrow 2\r\nrow 3\x1b[?2026l", next(t, sent), "a synchronized frame is one message")
	reads <- []byte("after")
	assert.Equal(t, "after", next(t, sent))
	close(reads)
	require.ErrorIs(t, <-done, io.EOF)
}

func TestPumpPTYFramesLetsGoOfAFrameThatNeverEnds(t *testing.T) {
	reads, sent, done := startPump(t)
	reads <- []byte("\x1b[?2026hhalf")
	assert.Equal(t, "\x1b[?2026hhalf", next(t, sent), "an unclosed frame goes after the hold")
	close(reads)
	require.ErrorIs(t, <-done, io.EOF)
}

func TestPumpPTYFramesSendsWhatIsReadBeforeTheEnd(t *testing.T) {
	reads, sent, done := startPump(t)
	reads <- []byte("\x1b[?2026hlast")
	close(reads)
	assert.Equal(t, "\x1b[?2026hlast", next(t, sent), "output read before the exit is not lost")
	require.ErrorIs(t, <-done, io.EOF)
}

func TestPumpPTYFramesStopsWhenTheSocketFails(t *testing.T) {
	reads := make(chan []byte, 1)
	reads <- []byte("x")
	err := pumpPTYFrames(pipeReads{reads}, func([]byte) error { return errors.New("gone") })
	assert.NoError(t, err, "a failed send is not the PTY's error")
}
