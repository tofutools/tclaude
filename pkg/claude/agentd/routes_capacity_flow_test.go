//go:build linux

package agentd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/routeadapter"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func TestRouteChannelCapacityRefusalClosesLeaseBeforeHandshake(t *testing.T) {
	setupTestDB(t)
	const (
		publisher = "route-capacity-publisher"
		consumer  = "route-capacity-consumer"
		groupName = "route-capacity-group"
	)
	publisherAgent, _, err := db.EnsureAgentForConv(publisher, "publisher")
	require.NoError(t, err)
	consumerAgent, _, err := db.EnsureAgentForConv(consumer, "consumer")
	require.NoError(t, err)
	groupID, err := db.CreateAgentGroup(groupName, "")
	require.NoError(t, err)
	require.NoError(t, db.ReplaceAgentGroupPermissions(groupID, []string{PermRoutesConsume}, "test"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: publisher, Role: "worker"}))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: groupID, ConvID: consumer, Role: "worker"}))
	group, err := db.GetAgentGroupByID(groupID)
	require.NoError(t, err)
	credential, launchGeneration, err := mintRouteHelperCredential(consumerAgent, consumer)
	require.NoError(t, err)
	t.Cleanup(func() { revokeRouteHelperCredentials(consumer, "") })
	route, err := db.CreateAgentRoute(groupID, publisherAgent, publisher, "publisher-launch", group.RouteGeneration, "api", "tcp", "tcp://127.0.0.1:43127")
	require.NoError(t, err)
	firstLease, err := db.OpenAgentRouteLease(route.ID, consumerAgent, consumer, launchGeneration, group.RouteGeneration)
	require.NoError(t, err)
	secondLease, err := db.OpenAgentRouteLease(route.ID, consumerAgent, consumer, launchGeneration, group.RouteGeneration)
	require.NoError(t, err)

	previousBroker := groupRouteBroker
	capacityBroker, err := routebroker.New(routebroker.Config{Authorizer: databaseRouteAuthority{}, MaxConsumersPerRoute: 1})
	require.NoError(t, err)
	groupRouteBroker = capacityBroker
	t.Cleanup(func() {
		_ = capacityBroker.Close()
		groupRouteBroker = previousBroker
		revokeRouteHelperCredentials(consumer, "")
	})
	firstBroker, firstPeer := net.Pipe()
	firstAuth := routebroker.ConsumerAuth{LeaseID: firstLease.ID, RouteID: route.ID, AgentID: consumerAgent, ConvID: consumer, LaunchGeneration: launchGeneration, GroupGeneration: group.RouteGeneration}
	go func() { _ = capacityBroker.AttachConsumer(context.Background(), firstAuth, firstBroker) }()
	require.Eventually(t, func() bool { return capacityBroker.Metrics().ConsumerChannels == 1 }, time.Second, time.Millisecond)
	t.Cleanup(func() { _ = firstPeer.Close() })

	server := httptest.NewServer(buildMux())
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodPost, server.URL+routeChannelPath, nil)
	require.NoError(t, err)
	req.Header.Set(routeHelperCredentialHeader, credential)
	req.Header.Set("X-Tclaude-Route-Role", routeadapter.RoleConsumer)
	req.Header.Set("X-Tclaude-Route-ID", route.ID)
	req.Header.Set("X-Tclaude-Route-Lease-ID", secondLease.ID)
	req.Header.Set("X-Tclaude-Route-Agent-ID", consumerAgent)
	req.Header.Set("X-Tclaude-Route-Conv-ID", consumer)
	req.Header.Set("X-Tclaude-Route-Launch-Generation", launchGeneration)
	req.Header.Set("X-Tclaude-Route-Group-Generation", formatRouteGeneration(group.RouteGeneration))
	req.Header.Set("X-Tclaude-Route-Consumer-Endpoint", "tcp://127.0.0.1:45810")
	client := &http.Client{Timeout: time.Second}
	response, requestErr := client.Do(req)
	if response != nil {
		defer response.Body.Close()
		require.NotEqual(t, http.StatusSwitchingProtocols, response.StatusCode)
	}
	require.Error(t, requestErr, "capacity refusal must close the channel before emitting HTTP 101")
	require.Eventually(t, func() bool {
		lease, getErr := db.GetAgentRouteLease(secondLease.ID)
		return getErr == nil && lease != nil && lease.State == db.RouteLeaseClosed
	}, time.Second, time.Millisecond)
	status := routeConsumerEndpointStatusForLease(secondLease.ID)
	require.Equal(t, "refused", status.state)
	require.Empty(t, status.endpoint)
}

func formatRouteGeneration(generation int64) string {
	return strconv.FormatInt(generation, 10)
}
