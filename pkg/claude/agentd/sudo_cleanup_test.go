package agentd

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestSudoGrantsCleanup_PurgesAgedExpiredRows pins the housekeeping
// invariant: rows whose expires_at slipped past wall-clock minus
// sudoGrantsRetention get hard-deleted; recent rows (still inside
// the retention window, expired or not) survive.
//
// Doesn't kick the timer-driven goroutine — calls runSudoGrantsCleanup
// directly so the test stays deterministic.
func TestSudoGrantsCleanup_PurgesAgedExpiredRows(t *testing.T) {
	setupTestDB(t)

	// Three test rows:
	//   1. expired ages ago      → purged (older than retention)
	//   2. expired just now      → kept   (inside retention)
	//   3. active (in-window)    → kept   (filter is `expires_at < now`)
	now := time.Now()
	const long = 90 * 24 * time.Hour // way past 30d retention

	old, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "old",
		Slug:      "groups.spawn",
		GrantedAt: now.Add(-long - time.Hour),
		ExpiresAt: now.Add(-long),
		GrantedBy: "human:popup-id=test",
	})
	if err != nil {
		t.Fatalf("seed old grant: %v", err)
	}
	recent, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "recent",
		Slug:      "groups.spawn",
		GrantedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
		GrantedBy: "human:popup-id=test",
	})
	if err != nil {
		t.Fatalf("seed recent grant: %v", err)
	}
	active, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "active",
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	if err != nil {
		t.Fatalf("seed active grant: %v", err)
	}

	runSudoGrantsCleanup(now)

	// Old row gone.
	if g, _ := db.GetSudoGrant(old); g != nil {
		t.Errorf("old grant should be purged; got %+v", g)
	}
	// Recent + active still there.
	if g, _ := db.GetSudoGrant(recent); g == nil {
		t.Errorf("recent grant must survive (inside retention window)")
	}
	if g, _ := db.GetSudoGrant(active); g == nil {
		t.Errorf("active grant must survive (in-window expires_at)")
	}
}

// TestSudoGrantsCleanup_QuietWhenNothingToPurge pins the no-op
// path: cleanup over an empty/clean table doesn't error and
// doesn't touch any rows.
func TestSudoGrantsCleanup_QuietWhenNothingToPurge(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	id, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "alice",
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	runSudoGrantsCleanup(now)
	if g, _ := db.GetSudoGrant(id); g == nil {
		t.Errorf("active grant must not be touched by cleanup")
	}
}
