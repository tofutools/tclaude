package routebroker

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlowStreamBackpressureAndHalfClose(t *testing.T) {
	a, b := net.Pipe()
	left, right := NewFlowStream(a), NewFlowStream(b)
	defer left.Close()
	defer right.Close()
	payload := bytes.Repeat([]byte("event: content_block_delta\ndata: abc\n\n"), 20000)
	written := make(chan error, 1)
	go func() {
		_, err := left.Write(payload)
		if err == nil {
			err = left.CloseWrite()
		}
		written <- err
	}()
	select {
	case err := <-written:
		t.Fatalf("write escaped credit window: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	got, err := io.ReadAll(right)
	require.NoError(t, err)
	require.Equal(t, payload, got)
	require.NoError(t, <-written)
	// Receiving EOF does not close the reverse direction.
	go func() {
		_, err := right.Write([]byte("response"))
		if err == nil {
			err = right.CloseWrite()
		}
		written <- err
	}()
	got, err = io.ReadAll(left)
	require.NoError(t, err)
	require.Equal(t, "response", string(got))
	require.NoError(t, <-written)
}

func TestFlowStreamRejectsCreditViolation(t *testing.T) {
	a, b := net.Pipe()
	right := NewFlowStream(b)
	defer right.Close()
	defer a.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < InitialWindow/MaxFramePayload+1; i++ {
			if WriteFrame(a, Frame{Kind: KindData, Stream: 1, Payload: make([]byte, MaxFramePayload)}, MaxFramePayload) != nil {
				return
			}
		}
	}()
	// No application read: the fifth frame exceeds the fixed ring's credit.
	select {
	case <-right.done:
	case <-time.After(time.Second):
		t.Fatal("over-credit sender not closed")
	}
	_, err := io.ReadAll(right)
	require.ErrorIs(t, err, ErrFlowViolation)
	<-done
}

func TestFlowStreamCloseUnblocksWriter(t *testing.T) {
	a, b := net.Pipe()
	left, right := NewFlowStream(a), NewFlowStream(b)
	defer right.Close()
	done := make(chan error, 1)
	go func() { _, err := left.Write(make([]byte, InitialWindow*2)); done <- err }()
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, left.Close())
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("writer stayed blocked")
	}
}
