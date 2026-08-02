package routeadapter

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

type allowAll struct{}

func (allowAll) AuthorizePublisher(context.Context, routebroker.PublisherAuth) error { return nil }
func (allowAll) AuthorizeConsumer(context.Context, routebroker.ConsumerAuth) error   { return nil }

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return port
}

func TestAdapterForwardsOpaqueTCPThroughBroker(t *testing.T) {
	targetPort := freePort(t)
	consumerPort := freePort(t)
	target, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(targetPort))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		payload := make([]byte, 6)
		if _, readErr := io.ReadFull(conn, payload); readErr == nil {
			_, _ = conn.Write([]byte("reply:" + string(payload)))
		}
	}()

	broker, err := routebroker.New(routebroker.Config{
		Authorizer:             allowAll{},
		AuthorityCheckInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	adapter, err := New(broker, []int{targetPort, consumerPort})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := adapter.Publish(ctx, Publisher{
		RouteID: "route-a", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(targetPort),
	}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-a", RouteID: "route-a", AgentID: "consumer", ConvID: "consumer-conv",
		LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("opaque")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 12)
	if _, err := io.ReadFull(conn, response); err != nil {
		t.Fatal(err)
	}
	if got, want := string(response), "reply:opaque"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestAdapterRejectsUnreservedPublisherTarget(t *testing.T) {
	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	adapter, err := New(broker, []int{41301})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if _, err := adapter.Publish(context.Background(), Publisher{
		RouteID: "route-a", AgentID: "publisher", ConvID: "publisher-conv", LaunchGeneration: "launch",
		Target: "tcp://127.0.0.1:41302",
	}); err == nil {
		t.Fatal("publisher target outside exact pool was accepted")
	}
}

func TestAdapterRefusesAnOccupiedConsumerSlot(t *testing.T) {
	port := freePort(t)
	occupied, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	b, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	adapter, err := New(b, []int{port})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	if _, err := adapter.Open(nil, Consumer{LeaseID: "lease-a", RouteID: "route-a"}); err == nil {
		t.Fatal("consumer accepted an occupied exact slot")
	}
}
