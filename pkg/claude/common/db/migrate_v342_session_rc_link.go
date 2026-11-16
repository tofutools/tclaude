package db

import (
	"database/sql"
	"fmt"
	"time"
)

// migrateV341toV342 adds sessions.rc_session_url and rc_session_url_at
// (tk_5ef87574): the Claude Code Remote Access link a launch announced in
// its transcript (a bridge_status record, read by the transcript follower),
// and that record's time, or the last switch's. The link lives on the
// session row, so each launch starts without one and it survives a daemon
// restart. A switch clears the link and stamps the time, so the follower
// does not write an older, now stale, link back; only a newer record does.
func migrateV341toV342(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v341→v342 (session rc link): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, col := range []struct{ name, ddl string }{
		{"rc_session_url", `ALTER TABLE sessions ADD COLUMN rc_session_url TEXT NOT NULL DEFAULT ''`},
		{"rc_session_url_at", `ALTER TABLE sessions ADD COLUMN rc_session_url_at INTEGER NOT NULL DEFAULT 0`},
	} {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col.name).Scan(&n); err != nil {
			return fmt.Errorf("migrate v341→v342 (probe sessions.%s): %w", col.name, err)
		}
		if n == 0 {
			if _, err := tx.Exec(col.ddl); err != nil {
				return fmt.Errorf("migrate v341→v342 (add sessions.%s): %w", col.name, err)
			}
		}
	}
	// Rows from before the upgrade start at the upgrade: a record they never
	// saw may be from a launch, or before a switch, this schema never
	// tracked, so it must neither become their link nor heal their flag.
	if _, err := tx.Exec(`UPDATE sessions SET rc_session_url_at = ? WHERE rc_session_url_at < ?`,
		time.Now().UnixNano(), time.Now().UnixNano()); err != nil {
		return fmt.Errorf("migrate v341→v342 (stamp existing rows): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 342`); err != nil {
		return fmt.Errorf("migrate v341→v342 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v341→v342 (commit): %w", err)
	}
	return nil
}
