package agentd

import (
	"context"
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

func TestKickedTerminalWaitIsBoundedAndDropsRegistry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	v := &fedTerminalView{ID: "viewer", ctx: ctx, cancel: cancel, done: make(chan struct{})}
	rt := &fedRuntime{}
	rt.terminalsLocked().views[v.ID] = v
	blocked, release := make(chan struct{}), make(chan struct{})
	v.addCleanup(func() { close(blocked); <-release })
	go v.close()
	<-blocked
	finished := make(chan struct{})
	go func() { rt.waitKickedTerminal(context.Background(), v); close(finished) }()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("kick waited indefinitely for blocked cleanup")
	}
	rt.terminalsMu.Lock()
	_, present := rt.terminalsLocked().views[v.ID]
	rt.terminalsMu.Unlock()
	require.False(t, present, "timed-out kick must release registry admission")
	close(release)
	<-v.done
}
