package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FindSessionsByPID exists because a pid is not unique over a machine's
// lifetime: rows record the pid their pane had at spawn, so a long-dead
// session and a live one can hold the same number. Identity resolution
// needs all the candidates so it can choose between them, where
// FindSessionByPID could only guess (TCL-761).

// stampSessionUpdatedAt pins updated_at so the ORDER BY under test is
// deterministic. SaveSession always stamps time.Now(), and two saves in
// one test can land close enough together that RFC3339Nano's
// variable-width fraction does not sort the way wall-clock did.
func stampSessionUpdatedAt(t *testing.T, id string, at time.Time) {
	t.Helper()
	handle, err := Open()
	require.NoError(t, err)
	_, err = handle.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		dbTime(at.UTC().Truncate(time.Second)), id)
	require.NoError(t, err)
}

func TestFindSessionsByPID_ReturnsEveryRowInFindSessionByPIDOrder(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	for i, id := range []string{"s-oldest", "s-middle", "s-newest"} {
		require.NoError(t, SaveSession(&SessionRow{ID: id, ConvID: "conv-" + id, PID: 4242}))
		stampSessionUpdatedAt(t, id, now.Add(time.Duration(i)*time.Minute))
	}
	// A row at another pid must not leak in.
	require.NoError(t, SaveSession(&SessionRow{ID: "s-other", ConvID: "conv-other", PID: 4243}))

	rows, err := FindSessionsByPID(4242)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	assert.Equal(t, []string{"s-newest", "s-middle", "s-oldest"}, ids,
		"most recently updated first — the same order FindSessionByPID picks its winner from")

	// The contract callers rely on: rows[0] IS FindSessionByPID's answer,
	// so a caller that ignores the extra candidates behaves identically.
	single, err := FindSessionByPID(4242)
	require.NoError(t, err)
	require.NotNil(t, single)
	assert.Equal(t, single.ID, rows[0].ID)
}

func TestFindSessionsByPID_NoMatchAndInvalidPID(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, SaveSession(&SessionRow{ID: "s1", ConvID: "conv-s1", PID: 4242}))

	rows, err := FindSessionsByPID(9999)
	require.NoError(t, err)
	assert.Empty(t, rows, "no match is an empty result, not an error")

	// pid 0 is the column default, so every row with no recorded pid
	// would match it. Both callers treat a nil result as "unplaceable",
	// which fails closed; returning the whole table would not.
	for _, pid := range []int{0, -1} {
		rows, err := FindSessionsByPID(pid)
		require.NoError(t, err)
		assert.Empty(t, rows, "pid %d must never match anything", pid)
	}
}

// A scan error on ONE row must not fail the whole query. This is the
// difference between "the daemon prefers a live row" and "an unrelated
// corrupt sibling makes a live agent unplaceable for its whole life" —
// the exact silent total-telemetry-loss failure the multi-row read was
// added to remove. FindSessionByPID never saw its siblings' decode
// errors, so failing here would be a regression against the code this
// replaced.
func TestFindSessionsByPID_SkipsUndecodableSiblings(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, SaveSession(&SessionRow{ID: "s-bad", ConvID: "conv-bad", PID: 4242}))
	require.NoError(t, SaveSession(&SessionRow{ID: "s-good", ConvID: "conv-good", PID: 4242}))

	// A launch snapshot this build cannot decode — what a row written by
	// a newer tclaude looks like after a downgrade.
	handle, err := Open()
	require.NoError(t, err)
	_, err = handle.Exec(
		`UPDATE sessions SET effective_sandbox_config = ? WHERE id = ?`,
		`{"version":999}`, "s-bad")
	require.NoError(t, err)

	rows, err := FindSessionsByPID(4242)
	require.NoError(t, err, "one bad row must not fail the query")
	require.Len(t, rows, 1, "the decodable sibling still resolves")
	assert.Equal(t, "s-good", rows[0].ID)
}
