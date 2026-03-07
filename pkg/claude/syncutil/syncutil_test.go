package syncutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadTombstones_Empty(t *testing.T) {
	dir := t.TempDir()
	tombstones, err := LoadTombstones(dir)
	if err != nil {
		t.Fatalf("LoadTombstones failed: %v", err)
	}
	if tombstones.Version != 1 {
		t.Errorf("expected version 1, got %d", tombstones.Version)
	}
	if len(tombstones.Entries) != 0 {
		t.Errorf("expected empty entries, got %d", len(tombstones.Entries))
	}
}

func TestAddTombstone(t *testing.T) {
	dir := t.TempDir()

	err := AddTombstone(dir, "session-123")
	if err != nil {
		t.Fatalf("AddTombstone failed: %v", err)
	}

	// Load and verify
	tombstones, err := LoadTombstones(dir)
	if err != nil {
		t.Fatalf("LoadTombstones failed: %v", err)
	}

	if len(tombstones.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(tombstones.Entries))
	}

	if tombstones.Entries[0].SessionID != "session-123" {
		t.Errorf("expected session-123, got %s", tombstones.Entries[0].SessionID)
	}

	// Verify file was created
	path := filepath.Join(dir, DeletionsFile)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Errorf("deletions.json was not created")
	}
}

func TestAddTombstone_Duplicate(t *testing.T) {
	dir := t.TempDir()

	// Add same tombstone twice
	AddTombstone(dir, "session-123")
	AddTombstone(dir, "session-123")

	tombstones, _ := LoadTombstones(dir)
	if len(tombstones.Entries) != 1 {
		t.Errorf("duplicate tombstone was added, got %d entries", len(tombstones.Entries))
	}
}

func TestMergeTombstones(t *testing.T) {
	src := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "a", DeletedAt: "2026-01-01T10:00:00Z", DeletedBy: "host1"},
			{SessionID: "b", DeletedAt: "2026-01-01T11:00:00Z", DeletedBy: "host1"},
		},
	}
	dst := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "b", DeletedAt: "2026-01-01T12:00:00Z", DeletedBy: "host2"}, // later
			{SessionID: "c", DeletedAt: "2026-01-01T13:00:00Z", DeletedBy: "host2"},
		},
	}

	modified := MergeTombstones(src, dst)
	if !modified {
		t.Error("expected dst to be modified")
	}

	if len(dst.Entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(dst.Entries))
	}

	// Find entry "b" - should have earlier time from src
	var foundB *Tombstone
	for i := range dst.Entries {
		if dst.Entries[i].SessionID == "b" {
			foundB = &dst.Entries[i]
			break
		}
	}
	if foundB == nil {
		t.Fatal("entry 'b' not found")
	}
	if foundB.DeletedAt != "2026-01-01T11:00:00Z" {
		t.Errorf("expected earlier time for 'b', got %s", foundB.DeletedAt)
	}
}

func TestMergeTombstones_NilSrc(t *testing.T) {
	dst := &Deletions{Version: 1}
	modified := MergeTombstones(nil, dst)
	if modified {
		t.Error("expected no modification with nil src")
	}
}

func TestMergeTombstones_EmptySrc(t *testing.T) {
	src := &Deletions{Version: 1}
	dst := &Deletions{Version: 1, Entries: []Tombstone{{SessionID: "a"}}}
	modified := MergeTombstones(src, dst)
	if modified {
		t.Error("expected no modification with empty src")
	}
}

func TestCleanupOldTombstones(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour) // 40 days ago
	recent := now.Add(-10 * 24 * time.Hour) // 10 days ago

	deletions := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "old1", DeletedAt: old.Format(time.RFC3339)},
			{SessionID: "recent", DeletedAt: recent.Format(time.RFC3339)},
			{SessionID: "old2", DeletedAt: old.Format(time.RFC3339)},
		},
	}

	removed := CleanupOldTombstones(deletions, TombstoneMaxAge)
	if removed != 2 {
		t.Errorf("expected 2 removed, got %d", removed)
	}

	if len(deletions.Entries) != 1 {
		t.Errorf("expected 1 remaining, got %d", len(deletions.Entries))
	}

	if deletions.Entries[0].SessionID != "recent" {
		t.Errorf("expected 'recent' to remain, got %s", deletions.Entries[0].SessionID)
	}
}

func TestCleanupOldTombstones_Empty(t *testing.T) {
	deletions := &Deletions{Version: 1}
	removed := CleanupOldTombstones(deletions, TombstoneMaxAge)
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

func TestCleanupOldTombstones_Nil(t *testing.T) {
	removed := CleanupOldTombstones(nil, TombstoneMaxAge)
	if removed != 0 {
		t.Errorf("expected 0 removed, got %d", removed)
	}
}

func TestHasTombstone(t *testing.T) {
	deletions := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "session-a"},
			{SessionID: "session-b"},
		},
	}

	if !deletions.HasTombstone("session-a") {
		t.Error("expected HasTombstone to return true for session-a")
	}
	if !deletions.HasTombstone("session-b") {
		t.Error("expected HasTombstone to return true for session-b")
	}
	if deletions.HasTombstone("session-c") {
		t.Error("expected HasTombstone to return false for session-c")
	}
}

func TestHasTombstone_Nil(t *testing.T) {
	var deletions *Deletions
	if deletions.HasTombstone("anything") {
		t.Error("expected HasTombstone to return false for nil")
	}
}

func TestTombstonedSessionIDs(t *testing.T) {
	deletions := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "session-a"},
			{SessionID: "session-b"},
		},
	}

	ids := deletions.TombstonedSessionIDs()
	if len(ids) != 2 {
		t.Fatalf("expected 2 IDs, got %d", len(ids))
	}
	if !ids["session-a"] || !ids["session-b"] {
		t.Error("expected both session IDs to be in map")
	}
}

func TestTombstonedSessionIDs_Nil(t *testing.T) {
	var deletions *Deletions
	ids := deletions.TombstonedSessionIDs()
	if len(ids) != 0 {
		t.Errorf("expected empty map, got %d entries", len(ids))
	}
}

func TestSaveTombstones(t *testing.T) {
	dir := t.TempDir()

	deletions := &Deletions{
		Version: 1,
		Entries: []Tombstone{
			{SessionID: "test-session", DeletedAt: "2026-01-15T10:00:00Z", DeletedBy: "testhost"},
		},
	}

	err := SaveTombstones(dir, deletions)
	if err != nil {
		t.Fatalf("SaveTombstones failed: %v", err)
	}

	// Load and verify
	loaded, err := LoadTombstones(dir)
	if err != nil {
		t.Fatalf("LoadTombstones failed: %v", err)
	}

	if len(loaded.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(loaded.Entries))
	}

	if loaded.Entries[0].SessionID != "test-session" {
		t.Errorf("expected test-session, got %s", loaded.Entries[0].SessionID)
	}
	if loaded.Entries[0].DeletedBy != "testhost" {
		t.Errorf("expected testhost, got %s", loaded.Entries[0].DeletedBy)
	}
}
