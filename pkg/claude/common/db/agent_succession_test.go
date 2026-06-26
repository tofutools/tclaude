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

func TestGetConvPredecessor_BackwardEdge(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, RecordConvSuccession("old", "new", "reincarnate"), "record edge")

	// Backward: new's predecessor is old.
	pred, err := GetConvPredecessor("new")
	require.NoError(t, err, "GetConvPredecessor")
	assert.Equal(t, "old", pred, "new <- old")

	// A conv that succeeded nothing has no predecessor.
	pred, err = GetConvPredecessor("old")
	require.NoError(t, err, "GetConvPredecessor(old)")
	assert.Equal(t, "", pred, "old has no predecessor")

	// Empty input is a benign empty result, not an error.
	pred, err = GetConvPredecessor("")
	require.NoError(t, err, "GetConvPredecessor(\"\")")
	assert.Equal(t, "", pred, "empty in -> empty out")
}

func TestResolvePredecessorN_WalksBack(t *testing.T) {
	setupTestDB(t)
	// Chain: a -> b -> c -> d (oldest to newest).
	require.NoError(t, RecordConvSuccession("a", "b", "reincarnate"), "a->b")
	require.NoError(t, RecordConvSuccession("b", "c", "reincarnate"), "b->c")
	require.NoError(t, RecordConvSuccession("c", "d", "reincarnate"), "c->d")

	// One hop back from d is c.
	got, hops, err := ResolvePredecessorN("d", 1)
	require.NoError(t, err, "back 1")
	assert.Equal(t, "c", got, "d back 1 -> c")
	assert.Equal(t, 1, hops, "hops")

	// Two hops back from d is b.
	got, hops, err = ResolvePredecessorN("d", 2)
	require.NoError(t, err, "back 2")
	assert.Equal(t, "b", got, "d back 2 -> b")
	assert.Equal(t, 2, hops, "hops")

	// Asking to walk further than the chain is deep lands on the root
	// (a) with hops < requested — best-effort, not an error.
	got, hops, err = ResolvePredecessorN("d", 99)
	require.NoError(t, err, "back 99")
	assert.Equal(t, "a", got, "d back 99 -> a (root)")
	assert.Equal(t, 3, hops, "hops capped at chain depth")
}

func TestResolvePredecessorN_NoPredecessor(t *testing.T) {
	setupTestDB(t)
	got, hops, err := ResolvePredecessorN("lonely", 1)
	require.NoError(t, err, "no predecessor")
	assert.Equal(t, "", got, "no ancestor")
	assert.Equal(t, 0, hops, "no hops")
}

func TestResolvePredecessorN_CycleProtection(t *testing.T) {
	setupTestDB(t)
	// A malformed pair of edges that loop (x's predecessor is y, y's is
	// x). The walk must terminate rather than spin.
	require.NoError(t, RecordConvSuccession("y", "x", "reincarnate"), "y->x")
	require.NoError(t, RecordConvSuccession("x", "y", "reincarnate"), "x->y")
	got, hops, err := ResolvePredecessorN("x", 99)
	require.NoError(t, err, "cycle")
	// First hop x<-y is fine; second would revisit x, so we stop.
	assert.Equal(t, "y", got, "stops at first repeat")
	assert.Equal(t, 1, hops, "single hop before cycle detected")
}
