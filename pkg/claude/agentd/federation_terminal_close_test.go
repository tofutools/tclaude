package agentd

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTerminalClosedInterruptsStalledOutput(t *testing.T) {
	conn, peer := net.Pipe()
	defer conn.Close()
	defer peer.Close()
	var mu sync.Mutex
	writing := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		mu.Lock()
		close(writing)
		_, _ = conn.Write([]byte("stalled output"))
		mu.Unlock()
		close(writerDone)
	}()
	<-writing
	done := make(chan struct{})
	go func() { writeTerminalClosed(conn, &mu, "kicked"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closed notification held viewer cleanup behind stalled output")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("stalled output writer survived abort")
	}
	require.NoError(t, peer.Close())
}
