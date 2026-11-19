package db

import (
	"database/sql"
	"fmt"
	"time"
)

// migrateV344toV345 adds process_message_deliveries (tk_9f7b4513): one row
// per recipient a process message step's command reached, written as each
// is delivered, so a send resumed after a crash or a failed commit skips who
// already has it.
func migrateV344toV345(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v344→v345 (process message deliveries): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS process_message_deliveries (
			command_id   TEXT NOT NULL,
			recipient    TEXT NOT NULL,
			run_id       TEXT NOT NULL REFERENCES process_runs(id) ON DELETE CASCADE,
			delivered_at INTEGER NOT NULL,
			PRIMARY KEY (command_id, recipient)
		) STRICT;
		CREATE INDEX IF NOT EXISTS idx_process_message_deliveries_run ON process_message_deliveries(run_id);
	`); err != nil {
		return fmt.Errorf("migrate v344→v345 (process message deliveries): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 345`); err != nil {
		return fmt.Errorf("migrate v344→v345 (process message deliveries): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v344→v345 (process message deliveries): commit: %w", err)
	}
	return nil
}

// ProcessMessageDelivered reports whether a message command already reached
// recipient.
func ProcessMessageDelivered(commandID, recipient string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM process_message_deliveries WHERE command_id = ? AND recipient = ?`, commandID, recipient).Scan(&n)
	return n > 0, err
}

// RecordProcessMessageDelivery records that a message command reached
// recipient. Recording it twice is a no-op.
func RecordProcessMessageDelivery(runID, commandID, recipient string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT OR IGNORE INTO process_message_deliveries (command_id, recipient, run_id, delivered_at) VALUES (?, ?, ?, ?)`,
		commandID, recipient, runID, time.Now().UnixNano())
	return err
}
