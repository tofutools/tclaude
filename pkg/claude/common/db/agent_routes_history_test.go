package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDashboardRouteProjectionBoundsTerminalHistoryAndKeepsOpenLease(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("route-history", "")
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("route-history-conv", "test")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "route-history-conv"}))
	group, err := GetAgentGroupByName("route-history")
	require.NoError(t, err)

	// A long terminal route history must not widen a dashboard snapshot.
	for i := 0; i < DashboardRouteHistoryPerGroup+17; i++ {
		route, createErr := CreateAgentRoute(groupID, agentID, "route-history-conv", "generation", group.RouteGeneration,
			fmt.Sprintf("old-route-%d", i), "tcp", "tcp://127.0.0.1:40000")
		require.NoError(t, createErr)
		require.NoError(t, WithdrawAgentRoute(route.ID, agentID, "route-history-conv", "history-test"))
	}
	current, err := CreateAgentRoute(groupID, agentID, "route-history-conv", "generation", group.RouteGeneration,
		"current-route", "tcp", "tcp://127.0.0.1:40001")
	require.NoError(t, err)

	// Repeated close/open cycles on one route stay bounded while its current
	// open lease remains in the projection.
	for i := 0; i < DashboardRouteLeaseHistoryPerRoute+5; i++ {
		lease, openErr := OpenAgentRouteLease(current.ID, agentID, "route-history-conv", "generation", group.RouteGeneration)
		require.NoError(t, openErr)
		require.NoError(t, CloseAgentRouteLease(lease.ID, agentID, "route-history-conv"))
	}
	open, err := OpenAgentRouteLease(current.ID, agentID, "route-history-conv", "generation", group.RouteGeneration)
	require.NoError(t, err)

	routes, err := ListAgentRoutesBatch([]int64{groupID})
	require.NoError(t, err)
	require.LessOrEqual(t, len(routes[groupID]), DashboardRouteHistoryPerGroup)
	require.Contains(t, routeIDs(routes[groupID]), current.ID)

	leases, err := ListAgentRouteLeasesBatch([]int64{groupID})
	require.NoError(t, err)
	require.LessOrEqual(t, len(leases[groupID]), DashboardRouteLeaseHistoryPerRoute+1)
	require.Contains(t, leaseIDs(leases[groupID]), open.ID)
}

func routeIDs(routes []*AgentRoute) []string {
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		ids = append(ids, route.ID)
	}
	return ids
}

func leaseIDs(leases []*AgentRouteLease) []string {
	ids := make([]string, 0, len(leases))
	for _, lease := range leases {
		ids = append(ids, lease.ID)
	}
	return ids
}
