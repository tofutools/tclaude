package db

import (
	"testing"
	"time"
)

// TestSetConvIndexArchived_Roundtrip locks in the archived-flag
// setter: write archived=true → row reports IsArchived; write
// archived=false → cleared. This is the column the reincarnate
// orchestrator stamps + listing surfaces filter on.
func TestSetConvIndexArchived_Roundtrip(t *testing.T) {
	setupTestDB(t)

	// Seed a conv_index row.
	convID := "11111111-aaaa-bbbb-cccc-111111111111"
	if err := UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker",
		IndexedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("UpsertConvIndex: %v", err)
	}

	// Initially active — column empty.
	row, err := GetConvIndex(convID)
	if err != nil || row == nil {
		t.Fatalf("GetConvIndex: %v / nil=%v", err, row == nil)
	}
	if row.IsArchived() {
		t.Errorf("expected active row, got archived: %+v", row)
	}

	// Stamp archived.
	if err := SetConvIndexArchived(convID, true); err != nil {
		t.Fatalf("SetConvIndexArchived(true): %v", err)
	}
	row, _ = GetConvIndex(convID)
	if !row.IsArchived() {
		t.Errorf("expected archived row, got active: %+v", row)
	}
	if row.ArchivedAt.IsZero() {
		t.Errorf("ArchivedAt should be non-zero, got %v", row.ArchivedAt)
	}

	// Clear it back.
	if err := SetConvIndexArchived(convID, false); err != nil {
		t.Fatalf("SetConvIndexArchived(false): %v", err)
	}
	row, _ = GetConvIndex(convID)
	if row.IsArchived() {
		t.Errorf("expected active row after clear, got archived: %+v", row)
	}
}

// TestUpsertConvIndex_PreservesArchivedAt locks in the contract that
// the routine .jsonl-rescan path (UpsertConvIndex) does NOT clobber
// the archived state. Without this guarantee, every `tclaude conv
// ls` would silently un-archive every reincarnated old conv on the
// first scan that picked up an mtime change.
func TestUpsertConvIndex_PreservesArchivedAt(t *testing.T) {
	setupTestDB(t)

	convID := "22222222-aaaa-bbbb-cccc-222222222222"
	if err := UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker",
		IndexedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := SetConvIndexArchived(convID, true); err != nil {
		t.Fatalf("SetConvIndexArchived: %v", err)
	}

	// Re-upsert — simulates a routine .jsonl rescan after the user
	// pokes the file (mtime bump).
	if err := UpsertConvIndex(&ConvIndexRow{
		ConvID:      convID,
		ProjectDir:  "/tmp/proj",
		FullPath:    "/tmp/proj/" + convID + ".jsonl",
		CustomTitle: "worker-x", // post-rename title
		IndexedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	row, _ := GetConvIndex(convID)
	if !row.IsArchived() {
		t.Errorf("upsert clobbered archived flag; got %+v", row)
	}
}

// TestSetConvIndexArchived_MissingRow returns sql.ErrNoRows so the
// caller can distinguish "no such conv" from "set succeeded".
func TestSetConvIndexArchived_MissingRow(t *testing.T) {
	setupTestDB(t)
	err := SetConvIndexArchived("nonexistent-conv-id", true)
	if err == nil {
		t.Error("expected error on missing row, got nil")
	}
}
