//go:build linux || darwin

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEpochV8GCBatchCursorProgressesAcrossProtectedEntriesAndRestart(t *testing.T) {
	epochsDir := t.TempDir()
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	old := now.Add(-EpochV8GCMinOrphanAge - time.Second)
	for i := 1; i <= EpochV8GCMaxEntries+2; i++ {
		name := fmt.Sprintf("%064x", i)
		path := filepath.Join(epochsDir, name)
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := os.Open(epochsDir)
	if err != nil {
		t.Fatal(err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	physical := make([]string, 0, len(names))
	for _, name := range names {
		if isHexSHA256(name) {
			physical = append(physical, name)
		}
	}
	if len(physical) != EpochV8GCMaxEntries+2 {
		t.Fatalf("physical entries = %d, want %d", len(physical), EpochV8GCMaxEntries+2)
	}
	orphan := physical[len(physical)-1]
	referenced := make(map[string]struct{}, len(physical)-1)
	for _, name := range physical[:len(physical)-1] {
		referenced[name] = struct{}{}
	}

	leaseChecks := 0
	requireLease := func() error {
		leaseChecks++
		return nil
	}
	first, err := collectEpochV8GarbageBatch(t.Context(), epochsDir, referenced, now, "", requireLease)
	if err != nil {
		t.Fatal(err)
	}
	if first.Scanned != EpochV8GCMaxEntries || first.Removed != 0 || first.Complete || first.NextCursor == "" {
		t.Fatalf("first GC batch = %#v", first)
	}
	if _, err := os.Stat(filepath.Join(epochsDir, orphan)); err != nil {
		t.Fatalf("physically later orphan removed before continuation: %v", err)
	}

	second, err := collectEpochV8GarbageBatch(t.Context(), epochsDir, referenced, now, first.NextCursor, requireLease)
	if err != nil {
		t.Fatal(err)
	}
	if second.Scanned > EpochV8GCMaxEntries || second.Removed != 1 || !second.Complete {
		t.Fatalf("second GC batch = %#v", second)
	}
	if _, err := os.Stat(filepath.Join(epochsDir, orphan)); !os.IsNotExist(err) {
		t.Fatalf("physically later orphan still exists: %v", err)
	}
	if leaseChecks != 1 {
		t.Fatalf("lease checks = %d, want 1 deletion check", leaseChecks)
	}
	for name := range referenced {
		if _, err := os.Stat(filepath.Join(epochsDir, name)); err != nil {
			t.Fatalf("referenced epoch %q was removed: %v", name, err)
		}
	}

	if _, err := collectEpochV8GarbageBatch(
		t.Context(), epochsDir, referenced, now, string(make([]byte, 32<<10)), requireLease,
	); err == nil {
		t.Fatal("oversized cursor unexpectedly accepted")
	}
}
