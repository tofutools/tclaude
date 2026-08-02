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

func TestDashboardRouteProjectionKeepsAllOpenLeasesSeparateFromTerminalHistory(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("lease-history", "")
	require.NoError(t, err)
	agentID, _, err := EnsureAgentForConv("lease-history-conv", "test")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "lease-history-conv"}))
	group, err := GetAgentGroupByName("lease-history")
	require.NoError(t, err)
	route, err := CreateAgentRoute(groupID, agentID, "lease-history-conv", "generation", group.RouteGeneration,
		"lease-history-route", "tcp", "tcp://127.0.0.1:40100")
	require.NoError(t, err)

	// Seed more terminal rows than the history budget, then leave more than
	// that budget open at the same time. Open leases must never compete with
	// terminal rows for the terminal-history window.
	for i := 0; i < DashboardRouteLeaseHistoryPerRoute+4; i++ {
		closed, openErr := OpenAgentRouteLease(route.ID, agentID, "lease-history-conv", "generation", group.RouteGeneration)
		require.NoError(t, openErr)
		require.NoError(t, CloseAgentRouteLease(closed.ID, agentID, "lease-history-conv"))
	}
	const openCount = DashboardRouteLeaseHistoryPerRoute + 1
	openIDs := make(map[string]struct{}, openCount)
	for i := 0; i < openCount; i++ {
		open, openErr := OpenAgentRouteLease(route.ID, agentID, "lease-history-conv", "generation", group.RouteGeneration)
		require.NoError(t, openErr)
		openIDs[open.ID] = struct{}{}
	}

	leases, err := ListAgentRouteLeasesBatch([]int64{groupID})
	require.NoError(t, err)
	var openSeen, terminalSeen int
	for _, lease := range leases[groupID] {
		if lease.State == RouteLeaseOpen {
			openSeen++
			if _, ok := openIDs[lease.ID]; !ok {
				t.Errorf("unexpected open lease %q in bounded projection", lease.ID)
			}
		} else {
			terminalSeen++
		}
	}
	require.Equal(t, openCount, openSeen, "every simultaneous open lease must survive")
	require.Equal(t, DashboardRouteLeaseHistoryPerRoute, terminalSeen, "only configured terminal history survives")
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
