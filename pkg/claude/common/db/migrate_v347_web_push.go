package db

import (
	"database/sql"
	"fmt"
)

// migrateV346toV347 adds Web Push (tk_2c9055c6):
//
//   - push_vapid: this node's one VAPID signing key, made on the first
//     subscribe. Private daemon state, like everything in db.sqlite.
//   - push_subscriptions: one row per subscribed device (browser), keyed by
//     its push service endpoint.
//   - push_outbox: one row per notification per device, written by whichever
//     process decided the notification (a hook callback, the daemon) and
//     sent by the daemon. Short-lived: a row is gone once delivered, given
//     up on, or past its time.
func migrateV346toV347(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v346→v347 (web push): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS push_vapid (
			id          INTEGER PRIMARY KEY CHECK(id = 1),
			private_key BLOB NOT NULL,
			created_at  INTEGER NOT NULL
		) STRICT;
		CREATE TABLE IF NOT EXISTS push_subscriptions (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint     TEXT NOT NULL UNIQUE CHECK(length(endpoint) BETWEEN 1 AND 2048),
			p256dh       TEXT NOT NULL CHECK(length(p256dh) BETWEEN 1 AND 256),
			auth         TEXT NOT NULL CHECK(length(auth) BETWEEN 1 AND 64),
			label        TEXT NOT NULL DEFAULT '' CHECK(length(label) <= 200),
			created_at   INTEGER NOT NULL,
			seen_at      INTEGER NOT NULL,
			last_ok_at   INTEGER,
			last_error   TEXT NOT NULL DEFAULT '',
			failures     INTEGER NOT NULL DEFAULT 0
		) STRICT;
		CREATE TABLE IF NOT EXISTS push_outbox (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			subscription_id INTEGER NOT NULL REFERENCES push_subscriptions(id) ON DELETE CASCADE,
			tag             TEXT NOT NULL,
			payload         TEXT NOT NULL,
			urgency         TEXT NOT NULL DEFAULT '',
			created_at      INTEGER NOT NULL,
			next_at         INTEGER NOT NULL,
			attempts        INTEGER NOT NULL DEFAULT 0
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_push_outbox_next ON push_outbox(next_at);
		CREATE INDEX IF NOT EXISTS idx_push_outbox_subscription ON push_outbox(subscription_id);
	`); err != nil {
		return fmt.Errorf("migrate v346→v347 (create web push tables): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 347`); err != nil {
		return fmt.Errorf("migrate v346→v347 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v346→v347 (commit): %w", err)
	}
	return nil
}
