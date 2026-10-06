package routeadapter

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// TestAdapterFlowControlCarriesBulkPastSlowReaders is the Darwin adapter's
// counterpart of the Linux helper test: more than every buffer on the path
// holds, both ways, to readers that stall first, with a broker that drops a
// channel blocked for longer than a moment.
func TestAdapterFlowControlCarriesBulkPastSlowReaders(t *testing.T) {
	const size = 12 << 20
	download := make([]byte, size)
	upload := make([]byte, size)
	_, _ = rand.Read(download)
	_, _ = rand.Read(upload)

	targetPort, consumerPort := freePort(t), freePort(t)
	target, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(targetPort))
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	uploaded := make(chan []byte, 1)
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		go func() {
			_, _ = conn.Write(download)
			_ = conn.(*net.TCPConn).CloseWrite()
		}()
		time.Sleep(400 * time.Millisecond)
		got, _ := io.ReadAll(conn)
		uploaded <- got
	}()

	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}, WriteTimeout: 150 * time.Millisecond})
	require.NoError(t, err)
	t.Cleanup(func() { _ = broker.Close() })
	adapter, err := New(broker, []int{targetPort, consumerPort})
	require.NoError(t, err)
	t.Cleanup(adapter.Close)
	adapter.SetFlowWindow(func() int { return routebroker.InitialWindow })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	_, err = adapter.Publish(ctx, Publisher{
		RouteID: "route-a", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(targetPort),
	})
	require.NoError(t, err)
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-a", RouteID: "route-a", AgentID: "consumer", ConvID: "consumer-conv",
		LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	require.NoError(t, err)

	client, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer client.Close()
	go func() {
		_, _ = client.Write(upload)
		_ = client.(*net.TCPConn).CloseWrite()
	}()
	time.Sleep(400 * time.Millisecond)
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
