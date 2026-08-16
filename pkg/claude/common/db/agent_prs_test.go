package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUnhandledAgentPRsValidationModes(t *testing.T) {
	setupTestDB(t)
	agent, _, err := EnsureAgentForConv("prmode-aaaa-bbbb-cccc-000000000001", "test")
	require.NoError(t, err)
	const legacyURL = "https://gitlab.example.com/acme/app/-/merge_requests/1"
	_, err = UpsertAgentPR(agent, legacyURL, "legacy", "open")
	require.NoError(t, err)

	all, err := ListUnhandledAgentPRs()
	require.NoError(t, err)
	require.Len(t, all[agent], 1, "proxy-disabled callers retain legacy rows")

	validated, err := ListValidatedUnhandledAgentPRs()
	require.NoError(t, err)
	assert.Empty(t, validated[agent], "credentialed callers quarantine unproven rows")
}

func TestUpdateAgentPRState_RefreshesSameStateObservationTime(t *testing.T) {
	setupTestDB(t)
	const url = "https://github.com/tofutools/tclaude/pull/125"

	agent, _, err := EnsureAgentForConv("prst-aaaa-bbbb-cccc-000000000002", "test")
	require.NoError(t, err)
	before, err := UpsertAgentPR(agent, url, "ready", "open")
	require.NoError(t, err)

	time.Sleep(time.Millisecond)
	n, err := UpdateAgentPRState(agent, url, "open")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "same-state refresh is still a fresh observation")

	after, err := GetAgentPR(agent, url)
	require.NoError(t, err)
	assert.True(t, after.UpdatedAt.After(before.UpdatedAt),
		"same-state observation must advance the freshness clock")
}

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

// A merged PR is terminal on GitHub. This also pins the race between the
// daemon-wide recently-merged search and an older per-PR refresh: the bulk
// search may write merged while the slower refresh is still in flight, and
// that late open result must not undo the terminal observation.
func TestUpdateAgentPRState_DoesNotRegressMerged(t *testing.T) {
	setupTestDB(t)
	const url = "https://github.com/tofutools/tclaude/pull/126"

	agent, _, err := EnsureAgentForConv("prst-aaaa-bbbb-cccc-000000000003", "test")
	require.NoError(t, err)
	merged, err := UpsertAgentPR(agent, url, "ready", "merged")
	require.NoError(t, err)

	time.Sleep(time.Millisecond)
	n, err := UpdateAgentPRState(agent, url, "open")
	require.NoError(t, err)
	assert.Zero(t, n, "late open observation must not regress merged")

	afterOpen, err := GetAgentPR(agent, url)
	require.NoError(t, err)
	assert.Equal(t, "merged", afterOpen.State)
	assert.Equal(t, merged.UpdatedAt, afterOpen.UpdatedAt,
		"rejected observation must not acquire a newer freshness timestamp")

	n, err = UpdateAgentPRState(agent, url, "closed")
	require.NoError(t, err)
	assert.Zero(t, n, "late closed observation must not regress merged")

	time.Sleep(time.Millisecond)
	n, err = UpdateAgentPRState(agent, url, "merged")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "same-state merged refresh remains valid")
	refreshed, err := GetAgentPR(agent, url)
	require.NoError(t, err)
	assert.True(t, refreshed.UpdatedAt.After(merged.UpdatedAt))
}
