package db

import (
	"strings"
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

// A row inserted between the head read and the row read is DELIVERED but
// would not be covered by a stale head — so the returned cursor must be
// pulled up to the last delivered id. Without that the client re-polls
// from behind the row it just painted and raises the same banner every
// few seconds until it ages out.
//
// The race is reproduced deterministically by inserting the row through
// the read path's own timestamp seam, then asserting the invariant the
// race would break: the cursor never trails what was handed over.
func TestBrowserNotificationsCursorNeverTrailsWhatWasDelivered(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	for i := 0; i < 3; i++ {
		require.NoError(t, enqueueBrowserNotificationAt("sess", "Claude: Idle", "", now))
	}

	items, cursor, err := listBrowserNotificationsSinceAt(0, now)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	assert.GreaterOrEqual(t, cursor, items[len(items)-1].ID,
		"the cursor must cover every row just delivered, or that row repeats forever")

	// And polling from it is genuinely empty — no repeat.
	again, _, err := listBrowserNotificationsSinceAt(cursor, now)
	require.NoError(t, err)
	assert.Empty(t, again)
}

// The read path prunes too, so a queue abandoned when the operator
// switches delivery back to `os` still drains instead of sitting in the
// table forever.
func TestBrowserNotificationsReadPathPrunesToo(t *testing.T) {
	setupTestDB(t)

	now := time.Now()
	require.NoError(t, enqueueBrowserNotificationAt("sess-old", "Claude: Idle", "", now.Add(-2*browserNotificationTTL)))

	d, err := Open()
	require.NoError(t, err)
	var before int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM browser_notifications`).Scan(&before))
	require.Equal(t, 1, before)

	// A poll with no enqueue anywhere near it.
	_, _, err = listBrowserNotificationsSinceAt(0, now)
	require.NoError(t, err)

	var after int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM browser_notifications`).Scan(&after))
	assert.Zero(t, after, "reading prunes expired rows, not only enqueueing")
}

// created_at is compared as a STRING against the TTL cutoff, so both must
// be written in the same zone. A local-time stamp would order rows by
// wall-clock text and, across a DST fall-back, read a 70-minute-old row as
// fresh for an hour.
func TestBrowserNotificationsTimestampsAreUTC(t *testing.T) {
	setupTestDB(t)

	// A zone with a large positive offset: a local-time stamp here sorts
	// well AHEAD of the same instant rendered in UTC.
	loc := time.FixedZone("UTC+13", 13*3600)
	now := time.Now().In(loc)
	require.NoError(t, enqueueBrowserNotificationAt("sess", "Claude: Idle", "", now))

	d, err := Open()
	require.NoError(t, err)
	var created string
	require.NoError(t, d.QueryRow(`SELECT created_at FROM browser_notifications`).Scan(&created))
	assert.True(t, strings.HasSuffix(created, "Z"), "created_at must be UTC, got %q", created)

	// And it is still delivered when the reader is in yet another zone.
	items, _, err := listBrowserNotificationsSinceAt(0, now.In(time.FixedZone("UTC-11", -11*3600)))
	require.NoError(t, err)
	assert.Len(t, items, 1)
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
