package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "seed old grant")
	recent, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "recent",
		Slug:      "groups.spawn",
		GrantedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed recent grant")
	active, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    "active",
		Slug:      "groups.spawn",
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:popup-id=test",
	})
	require.NoError(t, err, "seed active grant")

	runSudoGrantsCleanup(now)

	// Old row gone.
	g, _ := db.GetSudoGrant(old)
	assert.Nil(t, g, "old grant should be purged; got %+v", g)
	// Recent + active still there.
	g, _ = db.GetSudoGrant(recent)
	assert.NotNil(t, g, "recent grant must survive (inside retention window)")
	g, _ = db.GetSudoGrant(active)
	assert.NotNil(t, g, "active grant must survive (in-window expires_at)")
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
	require.NoError(t, err, "seed")
	runSudoGrantsCleanup(now)
	g, _ := db.GetSudoGrant(id)
	assert.NotNil(t, g, "active grant must not be touched by cleanup")
}
