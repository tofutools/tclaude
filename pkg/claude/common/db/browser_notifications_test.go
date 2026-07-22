package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrowserNotificationsCursorDeliversEachRowOnce(t *testing.T) {
	setupTestDB(t)

	require.NoError(t, EnqueueBrowserNotification("sess-1", "Claude: Idle", "abc | proj"))
	require.NoError(t, EnqueueBrowserNotification("sess-2", "Codex: Exited", ""))

	items, head, err := ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "Claude: Idle", items[0].Title)
	assert.Equal(t, "sess-2", items[1].SessionID)
	assert.Equal(t, items[1].ID, head)

	// Re-polling from the returned cursor yields nothing — no replay.
	again, head2, err := ListBrowserNotificationsSince(head)
	require.NoError(t, err)
	assert.Empty(t, again)
	assert.Equal(t, head, head2)

	// A second dashboard tab starting from 0 still sees both: reads do not
	// consume, so every open tab gets every notification.
	both, _, err := ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	assert.Len(t, both, 2)
}

func TestBrowserNotificationsHeadIsTheStartingCursor(t *testing.T) {
	setupTestDB(t)

	head, err := LatestBrowserNotificationID()
	require.NoError(t, err)
	assert.Zero(t, head, "an empty queue starts a fresh tab at 0")

	require.NoError(t, EnqueueBrowserNotification("sess-1", "Claude: Idle", ""))
	head, err = LatestBrowserNotificationID()
	require.NoError(t, err)
	assert.Positive(t, head)

	// A dashboard opening now adopts the head and sees nothing already queued.
	items, _, err := ListBrowserNotificationsSince(head)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestBrowserNotificationsExpireAndArePruned(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	old := now.Add(-2 * browserNotificationTTL)
	require.NoError(t, enqueueBrowserNotificationAt("sess-old", "Claude: Idle", "stale", old))

	// Still physically present, but past the TTL — never delivered.
	items, _, err := listBrowserNotificationsSinceAt(0, now)
	require.NoError(t, err)
	assert.Empty(t, items, "a banner older than the TTL is stale news, not a backlog")

	// The next enqueue prunes it, and the fresh one is delivered.
	require.NoError(t, enqueueBrowserNotificationAt("sess-new", "Claude: Idle", "fresh", now))
	items, _, err = listBrowserNotificationsSinceAt(0, now)
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "sess-new", items[0].SessionID)

	d, err := Open()
	require.NoError(t, err)
	var remaining int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM browser_notifications`).Scan(&remaining))
	assert.Equal(t, 1, remaining, "the expired row is pruned, not accumulated forever")
}

func TestBrowserNotificationsBatchIsCappedWithoutSkipping(t *testing.T) {
	setupTestDB(t)

	total := browserNotificationLimit + 5
	for i := 0; i < total; i++ {
		require.NoError(t, EnqueueBrowserNotification("sess", "Claude: Idle", ""))
	}

	first, cursor, err := ListBrowserNotificationsSince(0)
	require.NoError(t, err)
	require.Len(t, first, browserNotificationLimit)
	// A truncated batch reports the LAST DELIVERED id, so the remainder is
	// picked up next poll rather than skipped.
	assert.Equal(t, first[len(first)-1].ID, cursor)

	rest, _, err := ListBrowserNotificationsSince(cursor)
	require.NoError(t, err)
	assert.Len(t, rest, 5)
}
