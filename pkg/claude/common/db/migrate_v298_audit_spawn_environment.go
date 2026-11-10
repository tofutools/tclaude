package db

import (
	"database/sql"
	"fmt"
)

// Spawn audit rows kept the request's environment values in the clear
// (tcl-4vr1yo). New rows are redacted when written; this does the same to the
// rows already stored, changing only those values, so who did what and when
// stays as recorded. A row it cannot read is left for retention to remove.
func migrateV297toV298(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Some partial test schemas never had the table.
	haveAuditLog, err := txTableExists(tx, "audit_log")
	if err != nil {
		return fmt.Errorf("migrate v297→v298 (probe audit_log): %w", err)
	}
	if haveAuditLog {
		if err := redactStoredSpawnAudit(tx); err != nil {
			return fmt.Errorf("migrate v297→v298 (redact spawn rows): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 298`); err != nil {
		return err
	}
	return tx.Commit()
}

func redactStoredSpawnAudit(tx *sql.Tx) error {
	// LIKE ignores case, as the handler's JSON decoding does.
	rows, err := tx.Query(`SELECT id, detail FROM audit_log WHERE verb = 'spawn' AND detail LIKE '%"environment"%'`)
	if err != nil {
		return err
	}
	redacted := map[int64]string{}
	for rows.Next() {
		var id int64
		var detail string
		if err := rows.Scan(&id, &detail); err != nil {
			_ = rows.Close()
			return err
		}
		if clean, changed := redactSpawnAuditDetail(detail); changed {
			redacted[id] = clean
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for id, detail := range redacted {
		if _, err := tx.Exec(`UPDATE audit_log SET detail = ? WHERE id = ?`, detail, id); err != nil {
			return err
		}
	}
	return nil
}
