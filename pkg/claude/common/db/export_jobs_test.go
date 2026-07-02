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
	got, err = GetExportJob(id)
	require.NoError(t, err)
	assert.Equal(t, ExportStatusReady, got.Status)

	// A second SetExportJobReady must NOT overwrite an already-ready job —
	// a duplicate submit can't clobber the delivered artifact's metadata.
	again, err := SetExportJobReady(id, "/tmp/x/other.zip", "other.zip", 99, "application/zip")
	require.NoError(t, err)
	assert.False(t, again, "ready job is not overwritten")
	got, err = GetExportJob(id)
	require.NoError(t, err)
	assert.Equal(t, "export.zip", got.ArtifactName, "original artifact preserved")
	assert.Equal(t, int64(1234), got.ArtifactSize)
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

func TestListExportJobsForConv(t *testing.T) {
	setupTestDB(t)
	none, err := ListExportJobsForConv("nobody", 50)
	require.NoError(t, err)
	assert.Empty(t, none)

	_, err = InsertExportJob(&ExportJob{ConvID: "c1", Title: "first"})
	require.NoError(t, err)
	id2, err := InsertExportJob(&ExportJob{ConvID: "c1", Title: "second"})
	require.NoError(t, err)
	_, err = InsertExportJob(&ExportJob{ConvID: "c2", Title: "other"})
	require.NoError(t, err)

	jobs, err := ListExportJobsForConv("c1", 50)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, id2, jobs[0].ID, "newest first by id")
	assert.Equal(t, "second", jobs[0].Title)
	assert.Equal(t, "first", jobs[1].Title)

	// limit caps the result.
	limited, err := ListExportJobsForConv("c1", 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, id2, limited[0].ID)
}

func TestListExportJobs(t *testing.T) {
	setupTestDB(t)
	none, err := ListExportJobs(50)
	require.NoError(t, err)
	assert.Empty(t, none)

	id1, err := InsertExportJob(&ExportJob{ConvID: "c1", Title: "first"})
	require.NoError(t, err)
	id2, err := InsertExportJob(&ExportJob{ConvID: "c2", Title: "second"})
	require.NoError(t, err)

	// Spans all conversations, newest first by id.
	jobs, err := ListExportJobs(50)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
	assert.Equal(t, id2, jobs[0].ID, "newest first by id")
	assert.Equal(t, "c2", jobs[0].ConvID)
	assert.Equal(t, id1, jobs[1].ID)

	// limit caps the result to the newest.
	limited, err := ListExportJobs(1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	assert.Equal(t, id2, limited[0].ID)
}

func TestDeleteExportJobsForConv(t *testing.T) {
	setupTestDB(t)
	a, err := InsertExportJob(&ExportJob{ConvID: "c1"})
	require.NoError(t, err)
	b, err := InsertExportJob(&ExportJob{ConvID: "c1"})
	require.NoError(t, err)
	keep, err := InsertExportJob(&ExportJob{ConvID: "c2"})
	require.NoError(t, err)

	ids, err := DeleteExportJobsForConv("c1")
	require.NoError(t, err)
	assert.ElementsMatch(t, []int64{a, b}, ids, "returns the deleted ids for artifact cleanup")

	gone, err := ListExportJobsForConv("c1", 50)
	require.NoError(t, err)
	assert.Empty(t, gone)
	// The other conv's job survives.
	_, err = GetExportJob(keep)
	assert.NoError(t, err)

	// Clearing an empty conv is a no-op, not an error.
	ids, err = DeleteExportJobsForConv("c1")
	require.NoError(t, err)
	assert.Empty(t, ids)
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
