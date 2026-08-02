//go:build darwin

package agentd

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func darwinRouteFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())
	return port
}

func TestDarwinRouteAdapterRefusesMissingLaunchContracts(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(routeAdapterCloseAll)
	route := &db.AgentRoute{
		ID: "route-missing-contract", PublisherAgentID: "publisher-agent", PublisherConvID: "publisher-conv",
		PublisherLaunchGeneration: "publisher-generation", GroupGeneration: 1,
		Target: "tcp://127.0.0.1:43127", State: db.RouteStateReady,
	}
	enabled, err := routeAdapterPublish(context.Background(), route)
	require.True(t, enabled, "Darwin route operations must be contract-controlled")
	require.ErrorContains(t, err, "contract is missing")

	lease := &db.AgentRouteLease{
		ID: "lease-missing-contract", RouteID: route.ID, ConsumerAgentID: "consumer-agent",
		ConsumerConvID: "consumer-conv", ConsumerLaunchGeneration: "consumer-generation",
		GroupGeneration: 1, State: db.RouteLeaseOpen,
	}
	endpoint, enabled, err := routeAdapterOpen(context.Background(), route, lease)
	require.True(t, enabled)
	require.Empty(t, endpoint)
	require.ErrorContains(t, err, "contract is missing")

	// A consumer contract by itself must not manufacture a 201/endpoint when
	// the publisher launch was not route-capable.
	require.NoError(t, db.RegisterDarwinRouteLaunch("consumer-agent", "consumer-conv", "consumer-generation", []int{43129}))
	require.NoError(t, db.ActivateDarwinRouteLaunch("consumer-agent", "consumer-conv", "consumer-generation"))
	lease.ConsumerLaunchGeneration = "consumer-generation"
	endpoint, enabled, err = routeAdapterOpen(context.Background(), route, lease)
	require.True(t, enabled)
	require.Empty(t, endpoint)
	require.ErrorContains(t, err, "publisher launch contract")
}

func TestDarwinRouteAdapterProductionBridgeUsesExactContracts(t *testing.T) {
	setupTestDB(t)
	t.Cleanup(routeAdapterCloseAll)
	const publisherConv, consumerConv = "darwin-route-publisher", "darwin-route-consumer"
	publisherAgent, _, err := db.EnsureAgentForConv(publisherConv, "darwin-route-evidence")
	require.NoError(t, err)
	consumerAgent, _, err := db.EnsureAgentForConv(consumerConv, "darwin-route-evidence")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup("darwin-route-evidence-group", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: publisherConv}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: consumerConv}))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)

	targetPort := darwinRouteFreePort(t)
	consumerPort := darwinRouteFreePort(t)
	target, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(targetPort)))
	require.NoError(t, err)
	defer target.Close()
	go func() {
		conn, acceptErr := target.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		payload := make([]byte, len("opaque"))
		if _, err := io.ReadFull(conn, payload); err == nil {
			_, _ = conn.Write([]byte("reply:" + string(payload)))
		}
	}()

	const publisherGeneration, consumerGeneration = "darwin-publisher-generation", "darwin-consumer-generation"
	require.NoError(t, db.RegisterDarwinRouteLaunch(publisherAgent, publisherConv, publisherGeneration, []int{targetPort}))
	require.NoError(t, db.ActivateDarwinRouteLaunch(publisherAgent, publisherConv, publisherGeneration))
	require.NoError(t, db.RegisterDarwinRouteLaunch(consumerAgent, consumerConv, consumerGeneration, []int{consumerPort}))
	require.NoError(t, db.ActivateDarwinRouteLaunch(consumerAgent, consumerConv, consumerGeneration))
	route, err := db.CreateAgentRoute(groupID, publisherAgent, publisherConv, publisherGeneration, group.RouteGeneration, "api", "tcp", "tcp://127.0.0.1:"+strconv.Itoa(targetPort))
	require.NoError(t, err)
	lease, err := db.OpenAgentRouteLease(route.ID, consumerAgent, consumerConv, consumerGeneration, group.RouteGeneration)
	require.NoError(t, err)

	enabled, err := routeAdapterPublish(context.Background(), route)
	require.True(t, enabled)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return GroupRouteBroker().Metrics().PublisherChannels == 1 }, time.Second, time.Millisecond)
	endpoint, enabled, err := routeAdapterOpen(context.Background(), route, lease)
	require.True(t, enabled)
	require.NoError(t, err)
	conn, err := net.DialTimeout("tcp4", endpoint, time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("opaque"))
	require.NoError(t, err)
	response := make([]byte, len("reply:opaque"))
	_, err = io.ReadFull(conn, response)
	require.NoError(t, err)
	require.Equal(t, "reply:opaque", string(response))

	// The same production cleanup seam used by route withdrawal closes the
	// endpoint and the publisher channel without touching unrelated launches.
	require.NoError(t, db.WithdrawAgentRoute(route.ID, publisherAgent, publisherConv, "evidence withdrawal"))
	routeAdapterCloseRoute(route.ID)
	require.Eventually(t, func() bool { return len(routeAdapterLeaseIDs()) == 0 }, time.Second, time.Millisecond)
}

func routeAdapterLeaseIDs() []string {
	darwinRouteAdapterState.Lock()
	adapter := darwinRouteAdapterState.adapter
	darwinRouteAdapterState.Unlock()
	if adapter == nil {
		return nil
	}
	return adapter.LeaseIDs()
}
