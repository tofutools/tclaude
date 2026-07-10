package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateAgentPRState_DoesNotResurrectHandled pins the interleaving behind
// a flaky dashboard test and a real UX bug: a background PR-state poll is
// scheduled from an unhandled snapshot, the operator (or agent) marks the PR
// handled while the slow `gh` resolve is in flight, and the poll's write must
// then be a no-op instead of flipping the row back to "open".
func TestUpdateAgentPRState_DoesNotResurrectHandled(t *testing.T) {
	setupTestDB(t)
	const url = "https://github.com/tofutools/tclaude/pull/124"

	agent, _, err := EnsureAgentForConv("prst-aaaa-bbbb-cccc-000000000001", "test")
	require.NoError(t, err)

	_, err = UpsertAgentPR(agent, url, "ready", "")
	require.NoError(t, err)

	// Ordinary poll on an unhandled row still lands.
	n, err := UpdateAgentPRState(agent, url, "open")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "unhandled row accepts a state refresh")

	_, err = MarkAgentPRHandled(agent, url)
	require.NoError(t, err)

	// The in-flight poll completes after the handled write: must be a no-op.
	n, err = UpdateAgentPRState(agent, url, "open")
	require.NoError(t, err)
	assert.Zero(t, n, "stale poll must not resurrect a handled row")
	row, err := GetAgentPR(agent, url)
	require.NoError(t, err)
	assert.Equal(t, "handled", row.State)

	// An explicit re-present is the sanctioned way back.
	row, err = UpsertAgentPR(agent, url, "reopened", "open")
	require.NoError(t, err)
	assert.Equal(t, "open", row.State)
}
