package agentd

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
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
	rt := &fedRuntime{ctx: context.Background()}
	v, err := rt.addTerminal("peer", proto.SessionOpenPayload{Stream: proto.NewEnvelopeID()}, true)
	require.NoError(t, err)
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
	replacement, err := rt.addTerminal("peer", proto.SessionOpenPayload{Stream: v.ID}, true)
	require.NoError(t, err)
	// Another stale kick waiter must not remove the replacement either.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	rt.waitKickedTerminal(cancelled, v)
	rt.terminalsMu.Lock()
	require.Same(t, replacement, rt.terminalsLocked().views[v.ID])
	rt.terminalsMu.Unlock()
	close(release)
	<-v.done
	rt.terminalsMu.Lock()
	require.Same(t, replacement, rt.terminalsLocked().views[v.ID], "old cleanup must not remove a reused stream ID")
	rt.terminalsMu.Unlock()
	replacement.close()
}
