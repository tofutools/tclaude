package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostPublicationLifecycleWithoutGroup(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := EnsureAgentForConv("host-only-db", "test")
	require.NoError(t, err)
	route, err := CreateHostAgentRoute(agentID, "host-only-db", "launch", "ui", "tcp", "tcp://127.0.0.1:8080")
	require.NoError(t, err)
	require.True(t, route.HostOnly)
	require.Zero(t, route.GroupID)
	_, err = OpenAgentRouteLease(route.ID, agentID, "host-only-db", "launch", 1)
	require.ErrorContains(t, err, "host-only")
	require.Error(t, WithdrawAgentRoute(route.ID, "other", "host-only-db", "forged"))
	d, err := Open()
	require.NoError(t, err)
	tx, err := d.Begin()
	require.NoError(t, err)
	require.NoError(t, markAgentRoutesPublisherLostTx(tx, "host-only-db", "older-launch", "old exit"))
	require.NoError(t, tx.Commit())
	route, err = GetAgentRoute(route.ID)
	require.NoError(t, err)
	require.Equal(t, RouteStateReady, route.State)
	tx, err = d.Begin()
	require.NoError(t, err)
	require.NoError(t, markAgentRoutesPublisherLostTx(tx, "host-only-db", "launch", "exited"))
	require.NoError(t, tx.Commit())
	route, err = GetAgentRoute(route.ID)
	require.NoError(t, err)
	require.Equal(t, RouteStatePublisherLost, route.State)
	rows, err := ListHostAgentRoutes(agentID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	rows, err = ListHostAgentRoutes("other")
	require.NoError(t, err)
	require.Empty(t, rows)
	replacement, err := CreateHostAgentRoute(agentID, "host-only-db", "new-launch", "ui", "tcp", "tcp://127.0.0.1:8081")
	require.NoError(t, err)
	require.NotEqual(t, route.ID, replacement.ID)
	require.Equal(t, RouteStateReady, replacement.State)
	require.NoError(t, WithdrawAgentRoute(replacement.ID, agentID, "host-only-db", "done"))
	replacement, err = GetAgentRoute(replacement.ID)
	require.NoError(t, err)
	require.Equal(t, RouteStateWithdrawn, replacement.State)
	var violations int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations))
	require.Zero(t, violations)
}
