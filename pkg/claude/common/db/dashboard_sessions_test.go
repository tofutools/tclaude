package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardSessionGracePreserveListAndPrune(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)

	require.NoError(t, PreserveDashboardSessionGrace("old-expired", now.Add(-time.Minute), now.Add(-time.Hour)))
	require.NoError(t, PreserveDashboardSessionGrace("current", now.Add(30*time.Minute), now))
	require.NoError(t, PreserveDashboardSessionGrace("second", now.Add(20*time.Minute), now))

	got, err := ListDashboardSessionGrace(now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "second", got[0].TokenHash)
	assert.Equal(t, now.Add(20*time.Minute), got[0].ExpiresAt)
	assert.Equal(t, "current", got[1].TokenHash)

	var expiredRows int
	d, err := Open()
	require.NoError(t, err)
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM dashboard_session_grace
		WHERE token_hash = 'old-expired'`).Scan(&expiredRows))
	assert.Zero(t, expiredRows, "expired rows are physically pruned")

}

func TestPreserveDashboardSessionGraceRejectsEmptyHash(t *testing.T) {
	setupTestDB(t)
	err := PreserveDashboardSessionGrace("", time.Now().Add(time.Minute), time.Now())
	require.Error(t, err)
}
