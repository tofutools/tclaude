package codexappserver

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWriteGateExpiryDoesNotAttemptWrite(t *testing.T) {
	client := &Client{
		writeGate: make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
	client.writeGate <- struct{}{} // model another call owning the writer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	attempted, err := client.writeRaw(ctx, []byte(`{"method":"must-not-send"}`))
	if attempted {
		t.Fatal("write was marked attempted after context expired in the write queue")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v, want context deadline exceeded", err)
	}
}
