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
// back; ClearSessionExitReason returns the row to no-reason. A session
// that never had one recorded reads as "".
func TestSessionExitReason_SetGetClear(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "s1", "exited")

	// No reason recorded yet — NULL reads back as "".
	got, err := GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "", got, "a session with no recorded reason reads as empty")

	// Record one.
	require.NoError(t, SetSessionExitReason("s1", "logout"))
	got, err = GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "logout", got)

	// Clearing it drops back to NULL → "".
	require.NoError(t, ClearSessionExitReason("s1"))
	got, err = GetSessionExitReason("s1")
	require.NoError(t, err)
	assert.Equal(t, "", got, "ClearSessionExitReason returns the row to no-reason")
}

// GetSessionExitReason on an unknown id is not an error — it returns ""
// (the dashboard treats a missing row like a no-reason one).
func TestGetSessionExitReason_UnknownID(t *testing.T) {
	setupTestDB(t)
	got, err := GetSessionExitReason("nope")
	require.NoError(t, err)
	assert.Equal(t, "", got)
}

// MarkSessionExitedIfUnchanged stamps exit_reason='unexpected' when it
// reaps a session that recorded no reason — the death was unclean (no
// SessionEnd hook fired).
func TestMarkSessionExitedIfUnchanged_StampsUnexpected(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "crash", "working")
	row, err := LoadSession("crash")
	require.NoError(t, err)

	ok, err := MarkSessionExitedIfUnchanged("crash", "working", row.UpdatedAt)
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

// MarkSessionExitedIfUnchanged must NOT clobber an exit_reason a real
// SessionEnd already recorded — the COALESCE only fills a NULL. Pins
// the narrow reaper-vs-SessionEnd race.
func TestMarkSessionExitedIfUnchanged_KeepsExistingReason(t *testing.T) {
	setupTestDB(t)
	seedExitReasonSession(t, "clean", "working")
	require.NoError(t, SetSessionExitReason("clean", "logout"))
	row, err := LoadSession("clean")
	require.NoError(t, err)

	ok, err := MarkSessionExitedIfUnchanged("clean", "working", row.UpdatedAt)
	require.NoError(t, err)
	require.True(t, ok)

	reason, err := GetSessionExitReason("clean")
	require.NoError(t, err)
	assert.Equal(t, "logout", reason,
		"COALESCE must preserve a reason a real SessionEnd already recorded")
}
