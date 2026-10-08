package terminal

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFramesRejectMalformedAndOversized(t *testing.T) {
	f := Frame{Kind: Input, Data: []byte("\r\x1b[A\x03")}
	var b bytes.Buffer
	require.NoError(t, Write(&b, f))
	got, err := Read(&b)
	require.NoError(t, err)
	require.Equal(t, f, got)
	_, err = Encode(Frame{Kind: Output, Data: make([]byte, MaxPayload+1)})
	require.ErrorIs(t, err, ErrProtocol)
	for _, raw := range [][]byte{{}, {Input, 0, 0, 0, 1}, {255, 0, 0, 0, 0}, {Input, 0, 0, 0, 0, 1}} {
		_, err := Decode(raw)
		require.ErrorIs(t, err, ErrProtocol)
	}
	header := []byte{Output, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], MaxPayload+1)
	_, err = Read(bytes.NewReader(header))
	require.ErrorIs(t, err, ErrProtocol)
	_, _, err = ParseSize(Size(0, 24))
	require.ErrorIs(t, err, ErrProtocol)
	_, err = ParseNumber(Number(WindowSize + 1))
	require.ErrorIs(t, err, ErrProtocol)
}

func TestWindowBackpressureAndClose(t *testing.T) {
	w := NewWindow()
	n, err := w.Take(WindowSize)
	require.NoError(t, err)
	require.Equal(t, WindowSize, n)
	result := make(chan int, 1)
	go func() { n, _ := w.Take(1024); result <- n }()
	select {
	case <-result:
		t.Fatal("producer bypassed exhausted credit")
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, w.Grant(17))
	select {
	case n := <-result:
		require.Equal(t, 17, n)
	case <-time.After(time.Second):
		t.Fatal("producer did not resume")
	}
	blocked := make(chan error, 1)
	go func() { _, err := w.Take(1); blocked <- err }()
	w.Close()
	require.ErrorIs(t, <-blocked, io.ErrClosedPipe)
	require.ErrorIs(t, NewWindow().Grant(1), ErrProtocol)
}

func TestTerminalReportsCannotCarryKeyboardOrTmuxBindings(t *testing.T) {
	reports := []string{"\x1b[?1;2c", "\x1b[>0;136;0c", "\x1b[12;3R", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\", "\x1bP>|tmux 3.4\x1b\\"}
	for _, s := range reports {
		require.True(t, ValidReport([]byte(s)), "%q", s)
	}
	for _, s := range []string{"\x02:kill-server\r", "\x1b", "\x1b[A", "\x1b[200~yes\r\x1b[201~", "\x03", "\x1b]52;c;YWJj\a", "\x1b[?1;2c\x02", "\x1bP>|tmux\x02\x1b\\"} {
		require.False(t, ValidReport([]byte(s)), "%q", s)
	}
	raw := []byte("yes\r\x1b[A" + reports[0] + "\x03")
	frames := SplitReports(raw)
	require.Equal(t, []Frame{{Input, []byte("yes\r\x1b[A")}, {Report, []byte(reports[0])}, {Input, []byte("\x03")}}, frames)
}
