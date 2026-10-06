//go:build linux

package routeadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// flowConn is a channel on which agentd granted a flow window, as
// DialUnixChannel returns one.
type flowConn struct {
	net.Conn
	window int
}

func (c flowConn) FlowWindow() int { return c.window }

// TestFlowControlCarriesBulkPastSlowReaders sends more than every buffer on
// the path can hold, in both directions, to readers that stall first. With
// flow control the senders are held back instead: nothing overflows, the
// broker never blocks on a helper long enough to drop its channel, and the
// bytes arrive intact.
func TestFlowControlCarriesBulkPastSlowReaders(t *testing.T) {
	const size = 12 << 20
	download := make([]byte, size)
	upload := make([]byte, size)
	_, _ = rand.Read(download)
	_, _ = rand.Read(upload)

	targetLn, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = targetLn.Close() })
	uploaded := make(chan []byte, 1)
	go func() {
		conn, err := targetLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			_, _ = conn.Write(download)
			_ = conn.(*net.TCPConn).CloseWrite()
		}()
		time.Sleep(400 * time.Millisecond) // a target slow to start reading
		got, _ := io.ReadAll(conn)
		uploaded <- got
	}()

	// A short broker write timeout: a helper that stops reading its channel
	// for longer than this has its channel dropped.
	b, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}, WriteTimeout: 150 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })
	pub := routebroker.PublisherAuth{RouteID: "r", AgentID: "p", ConvID: "pc", LaunchGeneration: "g", FlowWindow: routebroker.InitialWindow}
	con := routebroker.ConsumerAuth{LeaseID: "l", RouteID: "r", AgentID: "c", ConvID: "cc", LaunchGeneration: "g", FlowWindow: routebroker.InitialWindow}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pubBroker, pubHelper := net.Pipe()
	ready := make(chan error, 1)
	go func() { _ = b.AttachPublisherReady(ctx, pub, pubBroker, func(err error) { ready <- err }) }()
	require.NoError(t, <-ready)
	go func() {
		_ = RunPublisher(ctx, flowConn{pubHelper, routebroker.InitialWindow}, "tcp://"+targetLn.Addr().String())
	}()

	conBroker, conHelper := net.Pipe()
	go func() { _ = b.AttachConsumer(ctx, con, conBroker) }()
	consumerLn, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = RunConsumer(ctx, flowConn{conHelper, routebroker.InitialWindow}, consumerLn) }()

	client, err := net.Dial("tcp4", consumerLn.Addr().String())
	require.NoError(t, err)
	defer client.Close()
	go func() {
		_, _ = client.Write(upload)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	time.Sleep(400 * time.Millisecond) // a client slow to start reading
	_ = client.SetReadDeadline(time.Now().Add(30 * time.Second))
	got, err := io.ReadAll(client)
	require.NoError(t, err)
	require.True(t, bytes.Equal(download, got), "download corrupted: got %d of %d bytes", len(got), size)
	select {
	case up := <-uploaded:
		require.True(t, bytes.Equal(upload, up), "upload corrupted: got %d of %d bytes", len(up), size)
	case <-time.After(30 * time.Second):
		t.Fatal("upload did not complete")
	}
}
