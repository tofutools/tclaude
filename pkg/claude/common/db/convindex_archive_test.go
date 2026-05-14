package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetConvIndexArchived_Roundtrip locks in the archived-flag
// setter: write archived=true → row reports IsArchived; write
// archived=false → cleared. This is the column the reincarnate
// orchestrator stamps + listing surfaces filter on.
func TestSetConvIndexArchived_Roundtrip(t *testing.T) {
	setupTestDB(t)

	// Seed a conv_index row.
	convID := "11111111-aaaa-bbbb-cccc-111111111111"
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker",
		IndexedAt:   time.Now(),
	}), "UpsertConvIndex")

	// Initially active — column empty.
	row, err := GetConvIndex(convID)
	require.NoError(t, err, "GetConvIndex")
	require.NotNil(t, row, "GetConvIndex returned nil")
	assert.False(t, row.IsArchived(), "expected active row, got archived: %+v", row)

	// Stamp archived.
	require.NoError(t, SetConvIndexArchived(convID, true), "SetConvIndexArchived(true)")
	row, _ = GetConvIndex(convID)
	assert.True(t, row.IsArchived(), "expected archived row, got active: %+v", row)
	assert.False(t, row.ArchivedAt.IsZero(), "ArchivedAt should be non-zero, got %v", row.ArchivedAt)

	// Clear it back.
	require.NoError(t, SetConvIndexArchived(convID, false), "SetConvIndexArchived(false)")
	row, _ = GetConvIndex(convID)
	assert.False(t, row.IsArchived(), "expected active row after clear, got archived: %+v", row)
}

// TestUpsertConvIndex_PreservesArchivedAt locks in the contract that
// the routine .jsonl-rescan path (UpsertConvIndex) does NOT clobber
// the archived state. Without this guarantee, every `tclaude conv
// ls` would silently un-archive every reincarnated old conv on the
// first scan that picked up an mtime change.
func TestUpsertConvIndex_PreservesArchivedAt(t *testing.T) {
	setupTestDB(t)

	convID := "22222222-aaaa-bbbb-cccc-222222222222"
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker",
		IndexedAt:   time.Now(),
	}), "first upsert")
	require.NoError(t, SetConvIndexArchived(convID, true), "SetConvIndexArchived")

	// Re-upsert — simulates a routine .jsonl rescan after the user
	// pokes the file (mtime bump).
	require.NoError(t, UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker-x", // post-rename title
		IndexedAt:   time.Now(),
	}), "re-upsert")

	row, _ := GetConvIndex(convID)
	assert.True(t, row.IsArchived(), "upsert clobbered archived flag; got %+v", row)
}

// TestSetConvIndexArchived_MissingRow returns sql.ErrNoRows so the
// caller can distinguish "no such conv" from "set succeeded".
func TestSetConvIndexArchived_MissingRow(t *testing.T) {
	setupTestDB(t)
	err := SetConvIndexArchived("nonexistent-conv-id", true)
	assert.Error(t, err, "expected error on missing row")
}
