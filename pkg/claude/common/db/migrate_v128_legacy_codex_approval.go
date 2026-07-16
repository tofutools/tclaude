package db

import (
	"database/sql"
	"fmt"
)

// migrateV127toV128 backfills only Codex approval postures that can be
// reconstructed from durable launch provenance. See inferLegacyCodexApproval:
// ambiguous direct/imported/template rows deliberately remain empty and the
// runtime guard continues to fail them closed with a relaunch repair path.
func migrateV127toV128(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	finishWithoutBackfill := func() error {
		if _, err := tx.Exec(`UPDATE schema_version SET version = 128`); err != nil {
			return fmt.Errorf("migrate v127→v128 (legacy codex approval): version: %w", err)
		}
		return tx.Commit()
	}
	for table, columns := range map[string][]string{
		"sessions":            {"id", "conv_id", "agent_id", "harness", "approval_policy", "approval_auto_review"},
		"agents":              {"agent_id", "created_via", "initial_spawn_config"},
		"agent_conversations": {"conv_id", "agent_id", "reason"},
	} {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return fmt.Errorf("migrate v127→v128 (legacy codex approval): probe %s: %w", table, err)
		}
		if n == 0 {
			return finishWithoutBackfill()
		}
		for _, column := range columns {
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('`+table+`') WHERE name = ?`, column).Scan(&n); err != nil {
				return fmt.Errorf("migrate v127→v128 (legacy codex approval): probe %s.%s: %w", table, column, err)
			}
			if n == 0 {
				return finishWithoutBackfill()
			}
		}
	}

	rows, err := tx.Query(`
		SELECT s.id, COALESCE(a.created_via, ''), COALESCE(ac.reason, ''),
		       COALESCE(a.initial_spawn_config, '')
		  FROM sessions s
		  LEFT JOIN agent_conversations ac ON ac.conv_id = s.conv_id
		  LEFT JOIN agents a ON a.agent_id = CASE
		       WHEN s.agent_id <> '' THEN s.agent_id ELSE ac.agent_id END
		 WHERE s.harness = 'codex' AND trim(s.approval_policy) = ''`)
	if err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): query: %w", err)
	}
	type repair struct{ id, policy string }
	var repairs []repair
	for rows.Next() {
		var id, createdVia, reason, config string
		if err := rows.Scan(&id, &createdVia, &reason, &config); err != nil {
			_ = rows.Close()
			return fmt.Errorf("migrate v127→v128 (legacy codex approval): scan: %w", err)
		}
		if policy, ok := inferLegacyCodexApproval(createdVia, reason, config); ok {
			repairs = append(repairs, repair{id, policy})
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): close rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): rows: %w", err)
	}
	for _, repair := range repairs {
		if _, err := tx.Exec(`UPDATE sessions SET approval_policy = ?, approval_auto_review = 0 WHERE id = ? AND trim(approval_policy) = ''`, repair.policy, repair.id); err != nil {
			return fmt.Errorf("migrate v127→v128 (legacy codex approval): repair %s: %w", repair.id, err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 128`); err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v127→v128 (legacy codex approval): commit: %w", err)
	}
	return nil
}
