package db

import (
	"database/sql"
	"errors"
	"time"
)

// Web Push state (migrate_v347_web_push.go): the VAPID key, the subscribed
// devices, and the outbox the daemon sends from.

// PushOutboxTTL is how long a queued push stays worth sending. An alert
// about something twenty minutes old is noise, and a node that was down
// must not flood the phone when it comes back: the same bound as the
// browser queue.
const PushOutboxTTL = browserNotificationTTL

// pushMaxAttempts is how often one push is tried before it is dropped.
const pushMaxAttempts = 3

// ErrPushSubscriptionNotFound is a device id that is not subscribed.
var ErrPushSubscriptionNotFound = errors.New("push subscription not found")

// PushSubscription is one subscribed device.
type PushSubscription struct {
	ID        int64      `json:"id"`
	Endpoint  string     `json:"-"`
	P256DH    string     `json:"-"`
	Auth      string     `json:"-"`
	Label     string     `json:"label"`
	CreatedAt time.Time  `json:"created_at"`
	SeenAt    time.Time  `json:"seen_at"`
	LastOKAt  *time.Time `json:"last_ok_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Failures  int        `json:"failures,omitempty"`
}

func pushMS(t time.Time) int64 { return t.UnixMilli() }

func pushFromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// PushVAPIDKey returns the stored VAPID private key, making and storing one
// with gen when there is none yet. Two callers racing both get the key that
// won.
func PushVAPIDKey(gen func() ([]byte, error), now time.Time) ([]byte, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var key []byte
	err = d.QueryRow(`SELECT private_key FROM push_vapid WHERE id = 1`).Scan(&key)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	fresh, err := gen()
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`INSERT OR IGNORE INTO push_vapid (id, private_key, created_at) VALUES (1, ?, ?)`, fresh, pushMS(now)); err != nil {
		return nil, err
	}
	err = d.QueryRow(`SELECT private_key FROM push_vapid WHERE id = 1`).Scan(&key)
	return key, err
}

// HasPushSubscriptions reports whether any device is subscribed.
func HasPushSubscriptions() (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var one int
	err = d.QueryRow(`SELECT 1 FROM push_subscriptions LIMIT 1`).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PushVAPIDKeyIfAny returns the stored VAPID private key, or nil when none
// was made yet.
func PushVAPIDKeyIfAny() ([]byte, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var key []byte
	err = d.QueryRow(`SELECT private_key FROM push_vapid WHERE id = 1`).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return key, err
}

// ResetPushVAPID forgets the VAPID key and, since every subscription was
// made for that key, every device and its queued pushes. It returns how
// many devices were removed.
func ResetPushVAPID() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM push_vapid`); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM push_subscriptions`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// UpsertPushSubscription stores a device's subscription, keyed by its
// endpoint. A device that subscribes again (each app open re-posts) keeps
// its id and label and gets its keys refreshed and its failures cleared.
// label is used only for a new device or when non-empty. It reports
// whether the device is new.
func UpsertPushSubscription(endpoint, p256dh, auth, label string, now time.Time) (PushSubscription, bool, error) {
	d, err := Open()
	if err != nil {
		return PushSubscription{}, false, err
	}
	var existing int64
	err = d.QueryRow(`SELECT id FROM push_subscriptions WHERE endpoint = ?`, endpoint).Scan(&existing)
	isNew := errors.Is(err, sql.ErrNoRows)
	if err != nil && !isNew {
		return PushSubscription{}, false, err
	}
	if _, err := d.Exec(`
		INSERT INTO push_subscriptions (endpoint, p256dh, auth, label, created_at, seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET
			p256dh = excluded.p256dh,
			auth = excluded.auth,
			label = CASE WHEN excluded.label <> '' THEN excluded.label ELSE push_subscriptions.label END,
			seen_at = excluded.seen_at,
			last_error = '',
			failures = 0`,
		endpoint, p256dh, auth, label, pushMS(now), pushMS(now)); err != nil {
		return PushSubscription{}, false, err
	}
	sub, err := pushSubscriptionWhere(d, `endpoint = ?`, endpoint)
	return sub, isNew, err
}

const pushSubscriptionCols = `id, endpoint, p256dh, auth, label, created_at, seen_at, last_ok_at, last_error, failures`

func scanPushSubscription(row interface{ Scan(...any) error }) (PushSubscription, error) {
	var s PushSubscription
	var created, seen int64
	var ok sql.NullInt64
	if err := row.Scan(&s.ID, &s.Endpoint, &s.P256DH, &s.Auth, &s.Label, &created, &seen, &ok, &s.LastError, &s.Failures); err != nil {
		return s, err
	}
	s.CreatedAt, s.SeenAt = pushFromMS(created), pushFromMS(seen)
	if ok.Valid {
		t := pushFromMS(ok.Int64)
		s.LastOKAt = &t
	}
	return s, nil
}

func pushSubscriptionWhere(d *sql.DB, where string, args ...any) (PushSubscription, error) {
	s, err := scanPushSubscription(d.QueryRow(`SELECT `+pushSubscriptionCols+` FROM push_subscriptions WHERE `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrPushSubscriptionNotFound
	}
	return s, err
}

// GetPushSubscription returns one device.
func GetPushSubscription(id int64) (PushSubscription, error) {
	d, err := Open()
	if err != nil {
		return PushSubscription{}, err
	}
	return pushSubscriptionWhere(d, `id = ?`, id)
}

// PushSubscriptionByEndpoint returns the device with that endpoint.
func PushSubscriptionByEndpoint(endpoint string) (PushSubscription, error) {
	d, err := Open()
	if err != nil {
		return PushSubscription{}, err
	}
	return pushSubscriptionWhere(d, `endpoint = ?`, endpoint)
}

// ListPushSubscriptions returns every subscribed device, oldest first.
func ListPushSubscriptions() ([]PushSubscription, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ` + pushSubscriptionCols + ` FROM push_subscriptions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []PushSubscription{}
	for rows.Next() {
		s, err := scanPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DeletePushSubscription forgets a device and its queued pushes.
func DeletePushSubscription(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM push_subscriptions WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPushSubscriptionNotFound
	}
	return nil
}

// EnqueuePush queues one push for every subscribed device, or for only
// the device onlyID when it is non-zero, due at once. It returns how many
// were queued: none when no device is subscribed.
func EnqueuePush(tag string, payload []byte, urgency string, onlyID int64, now time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`
		INSERT INTO push_outbox (subscription_id, tag, payload, urgency, created_at, next_at)
		SELECT id, ?, ?, ?, ?, 0 FROM push_subscriptions WHERE ? = 0 OR id = ?`,
		tag, string(payload), urgency, pushMS(now), onlyID, onlyID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PushJob is one queued push due to be sent, with its device.
type PushJob struct {
	ID        int64
	Tag       string
	Payload   []byte
	Urgency   string
	CreatedAt time.Time
	Attempts  int
	Sub       PushSubscription
}

// DuePushJobs drops the expired queue and returns the pushes due at now,
// oldest first, at most limit.
func DuePushJobs(now time.Time, limit int) ([]PushJob, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	if _, err := d.Exec(`DELETE FROM push_outbox WHERE created_at < ?`, pushMS(now.Add(-PushOutboxTTL))); err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT o.id, o.tag, o.payload, o.urgency, o.created_at, o.attempts,
		       s.id, s.endpoint, s.p256dh, s.auth, s.label, s.created_at, s.seen_at, s.last_ok_at, s.last_error, s.failures
		  FROM push_outbox o JOIN push_subscriptions s ON s.id = o.subscription_id
		 WHERE o.next_at <= ?
		 ORDER BY o.id LIMIT ?`, pushMS(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PushJob
	for rows.Next() {
		var j PushJob
		var payload string
		var created, sCreated, sSeen int64
		var ok sql.NullInt64
		if err := rows.Scan(&j.ID, &j.Tag, &payload, &j.Urgency, &created, &j.Attempts,
			&j.Sub.ID, &j.Sub.Endpoint, &j.Sub.P256DH, &j.Sub.Auth, &j.Sub.Label, &sCreated, &sSeen, &ok, &j.Sub.LastError, &j.Sub.Failures); err != nil {
			return nil, err
		}
		j.Payload = []byte(payload)
		j.CreatedAt = pushFromMS(created)
		j.Sub.CreatedAt, j.Sub.SeenAt = pushFromMS(sCreated), pushFromMS(sSeen)
		if ok.Valid {
			t := pushFromMS(ok.Int64)
			j.Sub.LastOKAt = &t
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// NextPushDue is when the earliest queued push is due, if any.
func NextPushDue() (time.Time, bool, error) {
	d, err := Open()
	if err != nil {
		return time.Time{}, false, err
	}
	var next sql.NullInt64
	if err := d.QueryRow(`SELECT MIN(next_at) FROM push_outbox`).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return pushFromMS(next.Int64), true, nil
}

// PushDelivered removes a sent push and marks its device as reachable.
func PushDelivered(job PushJob, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM push_outbox WHERE id = ?`, job.ID); err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE push_subscriptions SET last_ok_at = ?, last_error = '', failures = 0 WHERE id = ?`, pushMS(now), job.Sub.ID)
	return err
}

// PushFailed records a failed attempt on the device. When retryAt is set
// and the push has tries left, it is due again then; otherwise it is
// dropped. It reports whether it will be tried again.
func PushFailed(job PushJob, reason string, retryAt *time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	if _, err := d.Exec(`UPDATE push_subscriptions SET last_error = ?, failures = failures + 1 WHERE id = ?`, reason, job.Sub.ID); err != nil {
		return false, err
	}
	if retryAt != nil && job.Attempts+1 < pushMaxAttempts {
		_, err := d.Exec(`UPDATE push_outbox SET attempts = attempts + 1, next_at = ? WHERE id = ?`, pushMS(*retryAt), job.ID)
		return err == nil, err
	}
	_, err = d.Exec(`DELETE FROM push_outbox WHERE id = ?`, job.ID)
	return false, err
}
