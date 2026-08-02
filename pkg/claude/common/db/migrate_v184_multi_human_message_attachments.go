package db

import (
	"context"
	"database/sql"
	"fmt"
)

// migrateV183toV184 lets one human notification carry SEVERAL published files.
// v116 keyed human_message_attachments by message_id alone, so `notify-human
// --attach a --attach b` had to arrive as a single zip; that hides images
// behind an archive the dashboard cannot preview. The table is rebuilt with a
// surrogate id plus an ordering seq, and existing rows keep their identity as
// seq 0 of their message.
func migrateV183toV184(d *sql.DB) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate v183→v184 (multi human message attachments): connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// foreign_keys is connection-scoped and may only be changed outside a
	// transaction. The rebuild restores the human_messages FK before commit and
	// foreign_key_check proves the graph afterwards.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migrate v183→v184: disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate v183→v184: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE human_message_attachments_v184 (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id   INTEGER NOT NULL REFERENCES human_messages(id) ON DELETE CASCADE,
			seq          INTEGER NOT NULL DEFAULT 0,
			filename     TEXT NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			size_bytes   INTEGER NOT NULL,
			storage_path TEXT NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("migrate v183→v184: create replacement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO human_message_attachments_v184
			(message_id, seq, filename, content_type, size_bytes, storage_path)
		SELECT message_id, 0, filename, content_type, size_bytes, storage_path
		FROM human_message_attachments
		ORDER BY message_id`); err != nil {
		return fmt.Errorf("migrate v183→v184: copy rows: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE human_message_attachments`); err != nil {
		return fmt.Errorf("migrate v183→v184: drop legacy table: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`ALTER TABLE human_message_attachments_v184 RENAME TO human_message_attachments`); err != nil {
		return fmt.Errorf("migrate v183→v184: rename replacement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_human_message_attachments_message
		ON human_message_attachments(message_id, seq, id)`); err != nil {
		return fmt.Errorf("migrate v183→v184: index: %w", err)
	}
	if err := assertForeignKeyGraphIntact(ctx, tx, "migrate v183→v184"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 184`); err != nil {
		return fmt.Errorf("migrate v183→v184: version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v183→v184: commit: %w", err)
	}
	return nil
}

// assertForeignKeyGraphIntact fails the migration when the rebuild left a
// dangling reference behind — foreign keys are off for the rebuild, so the
// check has to be explicit.
func assertForeignKeyGraphIntact(ctx context.Context, tx *sql.Tx, stage string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("%s: foreign key check: %w", stage, err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return fmt.Errorf("%s: foreign key check reported violations", stage)
	}
	return rows.Err()
}
