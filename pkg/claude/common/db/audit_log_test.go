package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuditLog_InsertListPrune(t *testing.T) {
	setupTestDB(t)

	// Insert a spread of rows: two successes and one denial, across the
	// two surfaces. At is set explicitly so the prune cutoff is
	// deterministic (old rows fall away, recent ones stay).
	old := time.Now().Add(-90 * 24 * time.Hour)
	recent := time.Now().Add(-1 * time.Hour)

	_, err := InsertAuditLog(AuditLogEntry{
		At: old, ActorKind: AuditActorHuman, ActorLabel: "human",
		Verb: "spawn", TargetLabel: "worker-1", GroupName: "crew",
		Detail: "worker-1", Method: "POST", Path: "/v1/groups/crew/spawn",
		Status: 200, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		At: recent, ActorKind: AuditActorAgent, ActorConv: "conv-aaaa", ActorLabel: "po",
		Verb: "message", TargetConv: "conv-bbbb", TargetLabel: "worker-1",
		Detail: "rebasing now…", Method: "POST", Path: "/v1/messages",
		Status: 200, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	_, err = InsertAuditLog(AuditLogEntry{
		At: recent, ActorKind: AuditActorAgent, ActorConv: "conv-cccc", ActorLabel: "worker-2",
		Verb: "retire", TargetConv: "conv-bbbb", TargetLabel: "worker-1",
		Method: "POST", Path: "/v1/agent/worker-1/retire",
		Status: 403, Source: AuditSourceCLI,
	})
	require.NoError(t, err)

	// List all — newest first by id.
	all, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, "retire", all[0].Verb, "newest row (highest id) comes first")
	assert.Equal(t, "spawn", all[2].Verb, "oldest insert is last")
	// Round-trip a denormalized field + status.
	assert.Equal(t, "worker-1", all[2].TargetLabel)
	assert.Equal(t, 403, all[0].Status)

	// Verb filter.
	msgs, err := ListAuditLog(AuditLogFilter{Verb: "message"})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, "rebasing now…", msgs[0].Detail)

	// Outcome filter: failures only.
	fails, err := ListAuditLog(AuditLogFilter{Outcome: "failure"})
	require.NoError(t, err)
	require.Len(t, fails, 1)
	assert.Equal(t, "retire", fails[0].Verb)

	// Outcome filter: successes only.
	oks, err := ListAuditLog(AuditLogFilter{Outcome: "success"})
	require.NoError(t, err)
	assert.Len(t, oks, 2)

	// Limit.
	one, err := ListAuditLog(AuditLogFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, one, 1)
	assert.Equal(t, "retire", one[0].Verb)

	// Prune everything older than 30 days: the old spawn row goes, the
	// two recent rows remain.
	removed, err := PruneAuditLog(time.Now().Add(-30 * 24 * time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(1), removed)

	n, err := CountAuditLog()
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	remaining, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, remaining, 2)
	for _, e := range remaining {
		assert.NotEqual(t, "spawn", e.Verb, "the old spawn row should have been pruned")
	}
}

// TestAuditLog_AtDefaultsToNow verifies a zero At is stamped at insert
// time rather than written as the zero value.
func TestAuditLog_AtDefaultsToNow(t *testing.T) {
	setupTestDB(t)

	before := time.Now().Add(-time.Second)
	_, err := InsertAuditLog(AuditLogEntry{Verb: "spawn", Source: AuditSourceCLI})
	require.NoError(t, err)

	rows, err := ListAuditLog(AuditLogFilter{})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.True(t, rows[0].At.After(before), "zero At should be stamped to now at insert")
}
