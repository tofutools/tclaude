package db

import (
	"database/sql"
	"time"
)

// BrowserNotification is one queued banner awaiting pickup by a dashboard
// tab, which raises it through the browser's Web Notification API.
type BrowserNotification struct {
	ID        int64     `json:"id"`
	SessionID string    `json:"session_id,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// browserNotificationTTL is how long a queued notification stays
// deliverable. A banner about a state the agent left twenty minutes ago is
// noise, and a dashboard that was closed all night must not flood the
// human with a backlog when it reopens — so pickup is bounded in time as
// well as by the caller's cursor.
const browserNotificationTTL = 10 * time.Minute

// browserNotificationLimit caps one poll's batch. A burst (a group of
// agents all going idle) is delivered over consecutive polls rather than
// as one wall of banners.
const browserNotificationLimit = 20

// EnqueueBrowserNotification appends an already-formatted notification to
// the browser delivery queue and opportunistically prunes expired rows.
// sessionID may be empty (the banner is then non-clickable in the browser,
// mirroring the OS path).
func EnqueueBrowserNotification(sessionID, title, body string) error {
	return enqueueBrowserNotificationAt(sessionID, title, body, time.Now())
}

func enqueueBrowserNotificationAt(sessionID, title, body string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if _, err := d.Exec(
		`INSERT INTO browser_notifications (session_id, title, body, created_at) VALUES (?, ?, ?, ?)`,
		sessionID, title, body, stamp(now),
	); err != nil {
		return err
	}
	pruneBrowserNotifications(d, now)
	return nil
}

// stamp renders a queue timestamp. Deliberately UTC: created_at is compared
// against the TTL cutoff as a STRING, so two rows must never be ordered by
// their wall-clock text under different UTC offsets. At a DST fall-back a
// local-time stamp would make a 70-minute-old row read as fresh (and hide
// expired rows from the prune) for an hour.
func stamp(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// pruneBrowserNotifications drops expired rows. Best-effort by design: a
// failed prune must never fail the operation that triggered it — the row
// that matters is already committed, and every later enqueue or poll
// prunes again.
//
// Called from BOTH the enqueue and the read path, so a queue left behind
// when the operator switches delivery back to `os` still drains instead of
// sitting in the table forever.
func pruneBrowserNotifications(d *sql.DB, now time.Time) {
	_, _ = d.Exec(`DELETE FROM browser_notifications WHERE created_at < ?`,
		stamp(now.Add(-browserNotificationTTL)))
}

// ListBrowserNotificationsSince returns the un-expired notifications
// queued after afterID, oldest first, capped at browserNotificationLimit.
// It also returns the queue's current head id so a caller that received
// nothing (or a truncated batch) can still advance its cursor past rows
// that expired unseen instead of re-reading them forever.
func ListBrowserNotificationsSince(afterID int64) (items []BrowserNotification, head int64, err error) {
	return listBrowserNotificationsSinceAt(afterID, time.Now())
}

func listBrowserNotificationsSinceAt(afterID int64, now time.Time) ([]BrowserNotification, int64, error) {
	d, err := Open()
	if err != nil {
		return nil, 0, err
	}

	pruneBrowserNotifications(d, now)

	// Read the head BEFORE the rows, never after: these are two separate
	// implicit transactions, so a row inserted between them must land on
	// the side that cannot lose it. Head-first means such a row is either
	// invisible to both (delivered next poll) or visible to the row query
	// — and the max() below then pulls the returned cursor up to cover it.
	// Rows-first would instead produce a head AHEAD of what was delivered,
	// silently skipping that row forever.
	var head sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(id) FROM browser_notifications`).Scan(&head); err != nil {
		return nil, 0, err
	}

	cutoff := stamp(now.Add(-browserNotificationTTL))
	rows, err := d.Query(
		`SELECT id, session_id, title, body, created_at
		   FROM browser_notifications
		  WHERE id > ? AND created_at >= ?
		  ORDER BY id ASC
		  LIMIT ?`,
		afterID, cutoff, browserNotificationLimit)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var items []BrowserNotification
	for rows.Next() {
		var n BrowserNotification
		var created string
		if err := rows.Scan(&n.ID, &n.SessionID, &n.Title, &n.Body, &created); err != nil {
			return nil, 0, err
		}
		n.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	if len(items) == 0 {
		return items, head.Int64, nil
	}
	last := items[len(items)-1].ID
	// A truncated batch must NOT advance the caller past the rows it did
	// not receive, so the last delivered id is the cursor in that case.
	if len(items) == browserNotificationLimit {
		return items, last, nil
	}
	// Otherwise the cursor must cover everything just delivered: a row
	// inserted after the head read but before the row query is IN items
	// while head still sits behind it. Returning the stale head there
	// would re-deliver that row on every subsequent poll — a banner that
	// repeats every few seconds until it ages out.
	return items, max(head.Int64, last), nil
}

// LatestBrowserNotificationID returns the queue's head id — what a
// freshly-loaded dashboard tab adopts as its starting cursor so it shows
// only what happens from now on, never a backlog.
func LatestBrowserNotificationID() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var head sql.NullInt64
	if err := d.QueryRow(`SELECT MAX(id) FROM browser_notifications`).Scan(&head); err != nil {
		return 0, err
	}
	return head.Int64, nil
}
