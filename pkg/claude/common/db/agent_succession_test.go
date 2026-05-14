package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecordConvSuccession_Roundtrip(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, RecordConvSuccession("aaaa", "bbbb", "reincarnate"), "RecordConvSuccession")
	got, err := GetConvSuccessor("aaaa")
	require.NoError(t, err, "GetConvSuccessor")
	assert.Equal(t, "bbbb", got, "successor")
}

func TestRecordConvSuccession_Idempotent(t *testing.T) {
	setupTestDB(t)
	// First write — establishes the chain edge.
	require.NoError(t, RecordConvSuccession("aaaa", "bbbb", "reincarnate"), "first write")
	// Re-write the same edge — should overwrite, not error. (In
	// practice reincarnate never re-records the same edge, but the
	// idempotency keeps the contract robust against retries.)
	require.NoError(t, RecordConvSuccession("aaaa", "bbbb", "reincarnate"), "re-write same edge")
	// Update to a different successor — also should overwrite.
	require.NoError(t, RecordConvSuccession("aaaa", "cccc", "clone-replace"), "change edge")
	got, _ := GetConvSuccessor("aaaa")
	assert.Equal(t, "cccc", got, "after edge update, successor")
}

func TestRecordConvSuccession_Rejects(t *testing.T) {
	setupTestDB(t)
	assert.Error(t, RecordConvSuccession("", "bbbb", "x"), "expected error for empty oldConv")
	assert.Error(t, RecordConvSuccession("aaaa", "", "x"), "expected error for empty newConv")
	assert.Error(t, RecordConvSuccession("aaaa", "aaaa", "x"), "expected error for old == new")
}

func TestResolveLatestConv_WalksChain(t *testing.T) {
	setupTestDB(t)
	// A → B → C → D
	for _, edge := range [][2]string{
		{"aaaa", "bbbb"},
		{"bbbb", "cccc"},
		{"cccc", "dddd"},
	} {
		require.NoError(t, RecordConvSuccession(edge[0], edge[1], "reincarnate"), "RecordConvSuccession")
	}
	cases := map[string]string{
		"aaaa": "dddd", // four hops back → live
		"bbbb": "dddd",
		"cccc": "dddd",
		"dddd": "dddd", // already live
		"eeee": "eeee", // no chain → returns input
	}
	for in, want := range cases {
		got := ResolveLatestConv(in)
		assert.Equal(t, want, got, "ResolveLatestConv(%q)", in)
	}
}

func TestMigrateCronJobConvRef(t *testing.T) {
	setupTestDB(t)

	// Two jobs: one owned by `oldconv`, one targeted at `oldconv`.
	owner, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "owned-by-old",
		OwnerConv:       "oldconv",
		TargetConv:      "someother",
		IntervalSeconds: 60,
		Body:            "ping",
		Enabled:         true,
	})
	require.NoError(t, err, "insert owner job")
	target, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "targets-old",
		OwnerConv:       "manager",
		TargetConv:      "oldconv",
		IntervalSeconds: 120,
		Body:            "status?",
		Enabled:         true,
	})
	require.NoError(t, err, "insert target job")
	// Sanity: a third job that doesn't reference oldconv at all.
	bystander, err := InsertAgentCronJob(&AgentCronJob{
		Name:            "untouched",
		OwnerConv:       "elsewhere",
		TargetConv:      "elsewhere2",
		IntervalSeconds: 30,
		Body:            "ok",
		Enabled:         true,
	})
	require.NoError(t, err, "insert bystander job")

	n, err := MigrateCronJobConvRef("oldconv", "newconv")
	require.NoError(t, err, "MigrateCronJobConvRef")
	assert.Equal(t, int64(2), n, "rows affected")

	got1, _ := GetAgentCronJob(owner)
	assert.Equal(t, "newconv", got1.OwnerConv, "owner job owner_conv")
	assert.Equal(t, "someother", got1.TargetConv, "owner job target_conv mutated unexpectedly")
	got2, _ := GetAgentCronJob(target)
	assert.Equal(t, "newconv", got2.TargetConv, "target job target_conv")
	got3, _ := GetAgentCronJob(bystander)
	assert.Equal(t, "elsewhere", got3.OwnerConv, "bystander owner mutated")
	assert.Equal(t, "elsewhere2", got3.TargetConv, "bystander target mutated")
}

func TestListAgentConvSuccessions_OrderedByRecency(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, RecordConvSuccession("a1", "a2", "reincarnate"), "first record")
	require.NoError(t, RecordConvSuccession("b1", "b2", "reincarnate"), "second record")
	rows, err := ListAgentConvSuccessions()
	require.NoError(t, err, "ListAgentConvSuccessions")
	require.Len(t, rows, 2, "len(rows)")
	// Most recent first. RFC3339 succeeded_at has 1-second precision so
	// rapid back-to-back writes can collide; the rowid-DESC tiebreaker
	// guarantees deterministic ordering regardless of clock granularity.
	assert.Equal(t, "b1", rows[0].OldConvID, "rows[0].OldConvID")
}
