package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestExportJobLifecycle(t *testing.T) {
	setupTestDB(t)

	id, err := InsertExportJob(&ExportJob{
		ConvID:       "conv-aaa",
		Title:        "Research summary",
		Instructions: "make it clear",
		Preset:       "summary",
	})
	require.NoError(t, err)
	require.Positive(t, id)

	got, err := GetExportJob(id)
	require.NoError(t, err)
	assert.Equal(t, "conv-aaa", got.ConvID)
	assert.Equal(t, "Research summary", got.Title)
	assert.Equal(t, ExportStatusRequested, got.Status, "defaults to requested")
	assert.False(t, got.CreatedAt.IsZero(), "created_at stamped")

	// requested → running (only from requested; idempotent thereafter).
	moved, err := MarkExportJobRunning(id)
	require.NoError(t, err)
	assert.True(t, moved)
	moved, err = MarkExportJobRunning(id)
	require.NoError(t, err)
	assert.False(t, moved, "running is not re-entered from running")

	// running → ready records the artifact metadata.
	ok, err := SetExportJobReady(id, "/tmp/x/export.zip", "export.zip", 1234, "application/zip")
	require.NoError(t, err)
	assert.True(t, ok)
	got, err = GetExportJob(id)
	require.NoError(t, err)
	assert.Equal(t, ExportStatusReady, got.Status)
	assert.Equal(t, "export.zip", got.ArtifactName)
	assert.Equal(t, int64(1234), got.ArtifactSize)
	assert.Equal(t, "application/zip", got.ContentType)

	// A timeout sweep must NOT clobber a delivered (ready) job.
	failed, err := FailExportJob(id, "timed out")
	require.NoError(t, err)
	assert.False(t, failed, "ready job is authoritative")
	got, _ = GetExportJob(id)
	assert.Equal(t, ExportStatusReady, got.Status)
}

func TestExportJobNotFound(t *testing.T) {
	setupTestDB(t)
	_, err := GetExportJob(999)
	assert.ErrorIs(t, err, ErrExportJobNotFound)
}

func TestExportJobFailFromRequested(t *testing.T) {
	setupTestDB(t)
	id, err := InsertExportJob(&ExportJob{ConvID: "c", Status: ExportStatusRequested})
	require.NoError(t, err)
	ok, err := FailExportJob(id, "the agent did not respond")
	require.NoError(t, err)
	assert.True(t, ok)
	got, _ := GetExportJob(id)
	assert.Equal(t, ExportStatusFailed, got.Status)
	assert.Equal(t, "the agent did not respond", got.Error)
}

func TestLatestExportJobForConv(t *testing.T) {
	setupTestDB(t)
	assertNil, err := LatestExportJobForConv("nobody")
	require.NoError(t, err)
	assert.Nil(t, assertNil)

	_, err = InsertExportJob(&ExportJob{ConvID: "c1", Title: "first"})
	require.NoError(t, err)
	id2, err := InsertExportJob(&ExportJob{ConvID: "c1", Title: "second"})
	require.NoError(t, err)
	// A job for a different conv must not be picked.
	_, err = InsertExportJob(&ExportJob{ConvID: "c2", Title: "other"})
	require.NoError(t, err)

	latest, err := LatestExportJobForConv("c1")
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, id2, latest.ID, "latest by id, not created_at")
	assert.Equal(t, "second", latest.Title)
}

func TestListStaleExportJobs(t *testing.T) {
	setupTestDB(t)

	// A fresh ready job and an old requested job.
	freshID, err := InsertExportJob(&ExportJob{ConvID: "c", Status: ExportStatusReady})
	require.NoError(t, err)
	oldID, err := InsertExportJob(&ExportJob{
		ConvID:    "c",
		Status:    ExportStatusRequested,
		CreatedAt: time.Now().Add(-time.Hour),
		UpdatedAt: time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)

	// All stale (updated > 1m ago): only the old one qualifies.
	stale, err := ListStaleExportJobs(time.Now().Add(-time.Minute), false)
	require.NoError(t, err)
	require.Len(t, stale, 1)
	assert.Equal(t, oldID, stale[0].ID)

	// terminalOnly excludes the requested job; the fresh ready one isn't stale.
	terminal, err := ListStaleExportJobs(time.Now().Add(-time.Minute), true)
	require.NoError(t, err)
	assert.Empty(t, terminal)

	// Delete works and is reflected in a subsequent fetch.
	removed, err := DeleteExportJob(oldID)
	require.NoError(t, err)
	assert.True(t, removed)
	_, err = GetExportJob(oldID)
	assert.ErrorIs(t, err, ErrExportJobNotFound)
	// The fresh one survives.
	_, err = GetExportJob(freshID)
	assert.NoError(t, err)
}
