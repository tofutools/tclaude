//go:build linux

package routeadapter

import (
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

// TestSequentialShortLivedConnectionsOutliveTheRouteLimit drives the whole
// chain — consumer helper, real broker, publisher helper, echo target — through
// far more short-lived connections than one route may hold open at once. Each
// exchange ends the way an ordinary request/response does: both directions
// half-close and neither endpoint sends CLOSE. The broker must reclaim those
// streams, otherwise the route budget is consumed one exchange at a time and
// healthy connections start being refused at the limit.
func TestSequentialShortLivedConnectionsOutliveTheRouteLimit(t *testing.T) {
	const exchanges = 96

	target, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	go func() {
		for {
			conn, acceptErr := target.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				defer conn.Close()
				request, readErr := io.ReadAll(conn)
				if readErr != nil {
					return
				}
				_, _ = conn.Write([]byte("reply:" + string(request)))
			}()
		}
	}()

	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = broker.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)

	pubBroker, pubPeer := net.Pipe()
	go func() {
		_ = broker.AttachPublisher(ctx, routebroker.PublisherAuth{
			RouteID: "route-reclaim", AgentID: "publisher", ConvID: "publisher-conv",
			LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		}, pubBroker)
	}()
	go func() { _ = RunPublisher(ctx, pubPeer, "tcp://"+target.Addr().String()) }()
	waitForPublisherChannel(t, broker)

	conBroker, conPeer := net.Pipe()
	go func() {
		_ = broker.AttachConsumer(ctx, routebroker.ConsumerAuth{
			LeaseID: "lease-reclaim", RouteID: "route-reclaim", AgentID: "consumer",
			ConvID: "consumer-conv", LaunchGeneration: "consumer-launch", GroupGeneration: 1,
		}, conBroker)
	}()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunConsumer(ctx, conPeer, listener) }()

	for i := range exchanges {
		request := fmt.Sprintf("exchange-%02d", i)
		got, err := exchangeOnce(t, listener.Addr().String(), request)
		if err != nil {
			t.Fatalf("exchange %d of %d failed: %v", i+1, exchanges, err)
		}
		if got != "reply:"+request {
			t.Fatalf("exchange %d response = %q, want %q", i+1, got, "reply:"+request)
		}
	}

	// The route budget is the resource under test: no exchange may be refused
	// for capacity, and the streams they used must all be back.
	if refused := broker.Metrics().RejectedStreams; refused != 0 {
		t.Fatalf("rejected streams = %d, want 0 across %d short-lived exchanges", refused, exchanges)
	}
	// The last exchange's client sees EOF from the half-close the broker
	// forwards just before it reclaims, so give that final reclamation a
	// moment rather than racing it.
	deadline := time.Now().Add(5 * time.Second)
	for broker.Metrics().Streams != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if live := broker.Metrics().Streams; live != 0 {
		t.Fatalf("live streams = %d after %d completed exchanges, want 0", live, exchanges)
	}
}

// exchangeOnce performs one ordinary short-lived request/response: send the
// request, half-close so the target sees EOF, read the reply to EOF, close.
func exchangeOnce(t *testing.T, address, request string) (string, error) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", address, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte(request)); err != nil {
		return "", err
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		if err := tcp.CloseWrite(); err != nil {
			return "", err
		}
	}
	response, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(response), nil
}

func waitForPublisherChannel(t *testing.T, broker *routebroker.Broker) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if broker.Metrics().PublisherChannels == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("publisher channel never attached")
}
