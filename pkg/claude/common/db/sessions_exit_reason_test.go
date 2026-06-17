package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedExitReasonSession writes a minimal session row for the
// exit_reason tests.
func seedExitReasonSession(t *testing.T, id, status string) {
	t.Helper()
	require.NoError(t, SaveSession(&SessionRow{
		ID:     id,
		ConvID: "conv-" + id,
		Status: status,
	}), "seed session %s", id)
}

// SetSessionExitReason records a reason; GetSessionExitReason reads it
// back; ClearSessionExitReasonByConv returns it to no-reason. A session
// that never had one recorded reads as "".
func TestSessionExitReason_SetGetClear(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "s1", "exited") // ConvID "conv-s1"

	// No reason recorded yet — NULL reads back as "".
	got, err := GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "", got, "a session with no recorded reason reads as empty")

	// Record one.
	require.NoError(t, SetSessionExitReason("s1", "logout"))
	got, err = GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "logout", got)

	// Clearing the conv drops it back to NULL → "".
	require.NoError(t, ClearSessionExitReasonByConv("conv-s1"))
	got, err = GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "", got, "ClearSessionExitReasonByConv returns the row to no-reason")
}

// ClearSessionExitReasonByConv wipes exit_reason from EVERY row of a
// conv, not just one. A conv can own several session rows (an
// auto-registered row alongside an older one); a stale reason left on a
// sibling could be picked up by a later dashboard read and misreported
// as a crash. Pins the conv-scoped clear.
func TestClearSessionExitReasonByConv_ClearsEveryRow(t *testing.T) {
	setupTestDB(t)
	// Two rows, same conv_id — the multi-row shape an auto-registered
	// session leaves alongside an older row.
	require.NoError(t, SaveSession(&SessionRow{ID: "row-old", ConvID: "conv-x", Status: "exited"}))
	require.NoError(t, SaveSession(&SessionRow{ID: "row-new", ConvID: "conv-x", Status: "idle"}))
	require.NoError(t, SetSessionExitReason("row-old", "unexpected"))
	require.NoError(t, SetSessionExitReason("row-new", "logout"))

	require.NoError(t, ClearSessionExitReasonByConv("conv-x"))

	for _, id := range []string{"row-old", "row-new"} {
		got, err := GetSessionExitReason(id)
		require.NoError(t, err)
		assert.Equalf(t, "", got, "every row of the conv must be cleared (%s)", id)
	}
}

// GetSessionExitReason on an unknown id is not an error — it returns ""
// (the dashboard treats a missing row like a no-reason one).
func TestGetSessionExitReason_UnknownID(t *testing.T) {
	setupTestDB(t)
	got, err := GetSessionExitReason("nope")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// MarkSessionExitedIfUnchanged stamps the caller's fallback exit reason
// when it reaps a session that recorded no reason.
func TestMarkSessionExitedIfUnchanged_StampsFallbackReason(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "crash", "working")
	row, err := LoadSession("crash")
	require.NoError(t, err)

	ok, err := MarkSessionExitedIfUnchanged("crash", "working", row.UpdatedAt, "unexpected")
	require.NoError(t, err)
	require.True(t, ok, "the CAS should succeed — the row was unchanged")

	after, err := LoadSession("crash")
	require.NoError(t, err)
	assert.Equal(t, "exited", after.Status, "the reaped row is marked exited")

	reason, err := GetSessionExitReason("crash")
	require.NoError(t, err)
	assert.Equal(t, "unexpected", reason,
		"a reaped session with no recorded reason is stamped unexpected")
}

// MarkSessionExitedIfUnchanged with an empty fallback leaves a missing
// exit_reason NULL. That lets callers mark a dead process as plain
// offline when they do not have a positive crash signal.
func TestMarkSessionExitedIfUnchanged_EmptyFallbackLeavesReasonEmpty(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "plain", "working")
	row, err := LoadSession("plain")
	require.NoError(t, err)

	ok, err := MarkSessionExitedIfUnchanged("plain", "working", row.UpdatedAt, "")
	require.NoError(t, err)
	require.True(t, ok)

	reason, err := GetSessionExitReason("plain")
	require.NoError(t, err)
	assert.Equal(t, "", reason, "empty fallback must leave exit_reason NULL")
}

// MarkSessionExitedIfUnchanged must NOT clobber an exit_reason a real
// SessionEnd already recorded — the COALESCE only fills a NULL, even
// when the fallback is empty. Pins the narrow reaper-vs-SessionEnd race.
func TestMarkSessionExitedIfUnchanged_KeepsExistingReason(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "clean", "working")
	require.NoError(t, SetSessionExitReason("clean", "logout"))
	row, err := LoadSession("clean")
	require.NoError(t, err)

	ok, err := MarkSessionExitedIfUnchanged("clean", "working", row.UpdatedAt, "")
	require.NoError(t, err)
	require.True(t, ok)

	reason, err := GetSessionExitReason("clean")
	require.NoError(t, err)
	assert.Equal(t, "logout", reason,
		"COALESCE must preserve a reason a real SessionEnd already recorded")
}
