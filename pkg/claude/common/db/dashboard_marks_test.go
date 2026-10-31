package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardMarks_PruneKeepsActiveAndNewestEnded(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	add := func() int64 {
		m := &DashboardMark{Kind: "label", Spec: "{}", ExpiresAt: now.Add(time.Hour)}
		require.NoError(t, CreateDashboardMark(m, 0, 100, 2, now))
		return m.ID
	}
	first, second, third := add(), add(), add()
	for _, id := range []int64{first, second, third} {
		require.NoError(t, EndDashboardMark(id, MarkStateDismissed, MarkEndedByOperator, now))
	}
	active := add()
	marks, err := ListDashboardMarks("", 0, 10, now)
	require.NoError(t, err)
	ids := []int64{}
	for _, m := range marks {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []int64{active, third, second}, ids)
}

func TestDashboardCaptures_RetentionBoundsCountAndBytes(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	add := func(size int64) int64 {
		c := &DashboardCapture{Size: size}
		require.NoError(t, SaveDashboardCapture(c, now))
		return c.ID
	}
	a, b, c := add(10), add(10), add(10)
	stale, err := PruneDashboardCaptures(2, 1000)
	require.NoError(t, err)
	assert.Equal(t, []int64{a}, stale)
	stale, err = PruneDashboardCaptures(10, 15)
	require.NoError(t, err)
	assert.Equal(t, []int64{b}, stale)
	huge := add(10000)
	stale, err = PruneDashboardCaptures(10, 15)
	require.NoError(t, err)
	assert.Equal(t, []int64{c}, stale)
	kept, err := GetDashboardCapture(huge)
	require.NoError(t, err)
	assert.NotNil(t, kept, "the newest stays, whatever its size")
}
