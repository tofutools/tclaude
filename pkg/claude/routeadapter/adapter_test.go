package routeadapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

// slowAuthorizer delays publisher authorization so an immediate consumer open
// deterministically reproduces the window in which the broker had not yet
// registered the route.
type slowAuthorizer struct{ delay time.Duration }

func (s slowAuthorizer) AuthorizePublisher(context.Context, routebroker.PublisherAuth) error {
	time.Sleep(s.delay)
	return nil
}
func (slowAuthorizer) AuthorizeConsumer(context.Context, routebroker.ConsumerAuth) error { return nil }

type refusePublisher struct{ allowAll }

func (refusePublisher) AuthorizePublisher(context.Context, routebroker.PublisherAuth) error {
	return errors.New("publisher refused")
}

func TestAdapterPublishWaitsForBrokerRouteRegistration(t *testing.T) {
	targetPort, consumerPort := freePort(t), freePort(t)
	target, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)))
	require.NoError(t, err)
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

	broker, err := routebroker.New(routebroker.Config{Authorizer: slowAuthorizer{delay: 100 * time.Millisecond}})
	require.NoError(t, err)
	defer broker.Close()
	adapter, err := New(broker, []int{targetPort, consumerPort})
	require.NoError(t, err)
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = adapter.Publish(ctx, Publisher{
		RouteID: "route-ready", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(targetPort),
	})
	require.NoError(t, err)
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-ready", RouteID: "route-ready", AgentID: "consumer",
		ConvID: "consumer-conv", LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	require.NoError(t, err)
	// No settling delay: a consumer that connects the instant Publish returns
	// must still reach the publisher target.
	conn, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("opaque"))
	require.NoError(t, err)
	response := make([]byte, 12)
	_, err = io.ReadFull(conn, response)
	require.NoError(t, err)
	require.Equal(t, "reply:opaque", string(response))
}

func TestAdapterPublishReportsRefusedAuthority(t *testing.T) {
	port := freePort(t)
	broker, err := routebroker.New(routebroker.Config{Authorizer: refusePublisher{}})
	require.NoError(t, err)
	defer broker.Close()
	adapter, err := New(broker, []int{port})
	require.NoError(t, err)
	defer adapter.Close()
	_, err = adapter.Publish(context.Background(), Publisher{
		RouteID: "route-refused", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(port),
	})
	require.ErrorIs(t, err, routebroker.ErrUnauthorized)
	require.Empty(t, adapter.RouteIDs())
}

// The attach goroutine cancels the channel context the moment the broker
// refuses a publisher, so both barrier signals can be live at once and the
// end-to-end test above only catches a wrong choice when the scheduler happens
// to order it that way. Drive the barrier directly with both signals armed so
// the preference is asserted every run rather than under load.
func TestAwaitAttachPrefersTheAttachReasonOverCancellation(t *testing.T) {
	refused := fmt.Errorf("%w: publisher", routebroker.ErrUnauthorized)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	fired := func() chan time.Time {
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	t.Run("cancellation does not mask the refusal", func(t *testing.T) {
		ready := make(chan error, 1)
		ready <- refused
		require.ErrorIs(t, awaitAttach(canceled, ready, nil), routebroker.ErrUnauthorized)
	})
	t.Run("timeout does not mask the refusal", func(t *testing.T) {
		ready := make(chan error, 1)
		ready <- refused
		require.ErrorIs(t, awaitAttach(context.Background(), ready, fired()), routebroker.ErrUnauthorized)
	})
	t.Run("a real cancellation is still reported", func(t *testing.T) {
		require.ErrorIs(t, awaitAttach(canceled, make(chan error, 1), nil), context.Canceled)
	})
	t.Run("a stalled authority is still reported", func(t *testing.T) {
		require.ErrorIs(t, awaitAttach(context.Background(), make(chan error, 1), fired()), context.DeadlineExceeded)
	})
	t.Run("a cancelled route keeps the context error when the attach succeeded", func(t *testing.T) {
		// Asserted on the drain rather than through awaitAttach: with a queued
		// nil and a cancelled context both arms are live, so the barrier may
		// legitimately answer from either one.
		ready := make(chan error, 1)
		ready <- nil
		require.NoError(t, queuedAttachFailure(ready))
		require.NoError(t, queuedAttachFailure(make(chan error, 1)))
	})
	t.Run("a timeout yields to an attach that already succeeded", func(t *testing.T) {
		ready := make(chan error, 1)
		ready <- nil
		require.NoError(t, awaitAttach(context.Background(), ready, fired()))
	})
	t.Run("a plain attach failure is returned unwrapped", func(t *testing.T) {
		ready := make(chan error, 1)
		ready <- refused
		require.ErrorIs(t, awaitAttach(context.Background(), ready, nil), routebroker.ErrUnauthorized)
	})
	t.Run("a successful attach returns nil", func(t *testing.T) {
		ready := make(chan error, 1)
		ready <- nil
		require.NoError(t, awaitAttach(context.Background(), ready, nil))
	})
}

type refuseConsumer struct{ allowAll }

func (refuseConsumer) AuthorizeConsumer(context.Context, routebroker.ConsumerAuth) error {
	return errors.New("lease revoked")
}

// A consumer stream has no caller waiting on it: the accepted local connection
// can carry no reason and simply dies. Without the refusal seam the adapter
// therefore reports a broker refusal exactly as it reports a peer hanging up.
func TestAdapterReportsRefusedConsumerAttach(t *testing.T) {
	port := freePort(t)
	broker, err := routebroker.New(routebroker.Config{Authorizer: refuseConsumer{}})
	require.NoError(t, err)
	defer broker.Close()
	adapter, err := New(broker, []int{port})
	require.NoError(t, err)
	defer adapter.Close()
	refusals := make(chan error, 4)
	leases := make(chan string, 4)
	adapter.SetConsumerRefusalObserver(func(consumer Consumer, refusal error) {
		leases <- consumer.LeaseID
		refusals <- refusal
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-refused", RouteID: "route-refused", AgentID: "consumer",
		ConvID: "consumer-conv", LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	select {
	case refusal := <-refusals:
		require.ErrorIs(t, refusal, routebroker.ErrUnauthorized)
		require.Equal(t, "lease-refused", <-leases)
	case <-time.After(5 * time.Second):
		t.Fatal("refused consumer attach was never reported")
	}
	// The refused stream is not proxied: the local connection ends rather than
	// waiting on a route the broker never admitted.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
}

// Capacity is the other refusal the adapter used to discard, and it must reach
// the same seam so the daemon can tell it apart from an authority verdict.
func TestAdapterReportsConsumerCapacityRefusal(t *testing.T) {
	targetPort, consumerPort := freePort(t), freePort(t)
	target, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)))
	require.NoError(t, err)
	defer target.Close()
	go func() {
		for {
			conn, acceptErr := target.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(conn, conn) }()
		}
	}()
	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}, MaxConsumersPerRoute: 1})
	require.NoError(t, err)
	defer broker.Close()
	adapter, err := New(broker, []int{targetPort, consumerPort})
	require.NoError(t, err)
	defer adapter.Close()
	refusals := make(chan error, 4)
	adapter.SetConsumerRefusalObserver(func(_ Consumer, refusal error) { refusals <- refusal })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = adapter.Publish(ctx, Publisher{
		RouteID: "route-capacity", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(targetPort),
	})
	require.NoError(t, err)
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-capacity", RouteID: "route-capacity", AgentID: "consumer",
		ConvID: "consumer-conv", LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	require.NoError(t, err)
	first, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer first.Close()
	// Round-trip the admitted stream so the second dial cannot win the race for
	// the single consumer slot.
	_, err = first.Write([]byte("ping"))
	require.NoError(t, err)
	echoed := make([]byte, 4)
	require.NoError(t, first.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(first, echoed)
	require.NoError(t, err)
	second, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer second.Close()
	select {
	case refusal := <-refusals:
		require.ErrorIs(t, refusal, routebroker.ErrConsumerLimit)
	case <-time.After(5 * time.Second):
		t.Fatal("consumer capacity refusal was never reported")
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
	if _, err := adapter.Open(context.TODO(), Consumer{LeaseID: "lease-a", RouteID: "route-a"}); err == nil {
		t.Fatal("consumer accepted an occupied exact slot")
	}
}

func TestAdapterPublisherEOFPreservesReverseDirection(t *testing.T) {
	targetPort := freePort(t)
	consumerPort := freePort(t)
	target, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.Close()
		payload, readErr := io.ReadAll(conn)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		if string(payload) != "request" {
			serverDone <- fmt.Errorf("target payload = %q", payload)
			return
		}
		if _, err := conn.Write([]byte("reverse")); err != nil {
			serverDone <- err
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		serverDone <- nil
	}()

	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	adapter, err := New(broker, []int{targetPort, consumerPort})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := adapter.Publish(ctx, Publisher{
		RouteID: "route-half-close", AgentID: "publisher", ConvID: "publisher-conv",
		LaunchGeneration: "publisher-launch", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:" + strconv.Itoa(targetPort),
	}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := adapter.Open(ctx, Consumer{
		LeaseID: "lease-half-close", RouteID: "route-half-close", AgentID: "consumer",
		ConvID: "consumer-conv", LaunchGeneration: "consumer-launch", GroupGeneration: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp4", endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tcp := conn.(*net.TCPConn)
	if _, err := tcp.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	response, err := io.ReadAll(tcp)
	_ = conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(response), "reverse"; got != want {
		t.Fatalf("reverse response = %q, want %q", got, want)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestAdapterIdleLeaseCleanupReusesOnlyClosedSlot(t *testing.T) {
	firstPort, secondPort := freePort(t), freePort(t)
	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	adapter, err := New(broker, []int{firstPort, secondPort})
	if err != nil {
		t.Fatal(err)
	}
	defer adapter.Close()
	first, err := adapter.Open(context.Background(), Consumer{LeaseID: "lease-idle", RouteID: "route-a", AgentID: "agent-a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := adapter.Open(context.Background(), Consumer{LeaseID: "lease-live", RouteID: "route-b", AgentID: "agent-b"})
	if err != nil {
		t.Fatal(err)
	}
	adapter.CloseLease("lease-idle")
	if got := adapter.LeaseIDs(); len(got) != 1 || got[0] != "lease-live" {
		t.Fatalf("leases after idle cleanup = %v, want [lease-live]", got)
	}
	third, err := adapter.Open(context.Background(), Consumer{LeaseID: "lease-reuse", RouteID: "route-c", AgentID: "agent-c"})
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("reused endpoint = %q, want closed endpoint %q", third, first)
	}
	adapter.CloseLease("lease-live")
	_ = second
}

func TestAdapterCloseLeaseDoesNotCloseSiblingSameAgentRoute(t *testing.T) {
	firstPort, secondPort := freePort(t), freePort(t)
	broker, err := routebroker.New(routebroker.Config{Authorizer: allowAll{}})
	require.NoError(t, err)
	defer broker.Close()
	adapter, err := New(broker, []int{firstPort, secondPort})
	require.NoError(t, err)
	defer adapter.Close()
	first, err := adapter.Open(context.Background(), Consumer{LeaseID: "lease-sibling-a", RouteID: "route-sibling", AgentID: "same-agent"})
	require.NoError(t, err)
	second, err := adapter.Open(context.Background(), Consumer{LeaseID: "lease-sibling-b", RouteID: "route-sibling", AgentID: "same-agent"})
	require.NoError(t, err)
	adapter.CloseLease("lease-sibling-a")
	if got := adapter.LeaseIDs(); len(got) != 1 || got[0] != "lease-sibling-b" {
		t.Fatalf("leases after exact close = %v, want [lease-sibling-b]", got)
	}
	conn, err := net.DialTimeout("tcp4", second, time.Second)
	require.NoError(t, err)
	_ = conn.Close()
	if first == second {
		t.Fatal("sibling lease unexpectedly reused the closed endpoint")
	}
}
