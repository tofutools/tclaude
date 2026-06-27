package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSudoGrant_AgentIDPopulated verifies that the sudo-grant reads surface
// the stable agent_id the grant is keyed on (PR3c) — so `sudo ls` can group
// and label its blocks by the rotation-immune handle instead of a conv-id.
func TestSudoGrant_AgentIDPopulated(t *testing.T) {
	setupTestDB(t)

	// Local time (not UTC): ListActiveSudoGrants compares expires_at against a
	// local-tz cutoff string, and RFC3339Nano TEXT only orders correctly when
	// the offsets match (the known DB sort hazard).
	now := time.Now()
	_, err := InsertSudoGrant(&SudoGrant{
		ConvID: "worker", Slug: "groups.spawn", GrantedAt: now,
		ExpiresAt: now.Add(time.Hour), GrantedBy: "human",
	})
	require.NoError(t, err, "InsertSudoGrant")

	wantAgent, err := AgentIDForConv("worker")
	require.NoError(t, err, "AgentIDForConv(worker)")
	require.NotEmpty(t, wantAgent, "a granted conv should be minted as an actor")

	// Self-scoped list.
	self, err := ListActiveSudoGrants("worker")
	require.NoError(t, err, "ListActiveSudoGrants")
	require.Len(t, self, 1, "expected one active grant")
	assert.Equal(t, wantAgent, self[0].AgentID, "self list should carry the stable agent_id")
	assert.Equal(t, "worker", self[0].ConvID, "self list should resolve the current conv")

	// Daemon-wide list (the `sudo ls --all` path).
	all, err := ListAllActiveSudoGrants()
	require.NoError(t, err, "ListAllActiveSudoGrants")
	require.Len(t, all, 1, "expected one active grant daemon-wide")
	assert.Equal(t, wantAgent, all[0].AgentID, "--all list should carry the stable agent_id")
}
