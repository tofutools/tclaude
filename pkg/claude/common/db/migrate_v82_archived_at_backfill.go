package db

import (
	"database/sql"
	"fmt"
)

// migrateV81toV82 backfills conv_index.archived_at for reincarnation
// predecessors (JOH-320), retiring the `-x` title heuristic that used to
// stand in for the column.
//
// Background: conv_index.archived_at (schema v17) is the durable, explicit
// "this conversation is archived / soft-deleted" signal. Until now the live
// reincarnate path only renamed the retiring predecessor's title to `<base>-x`
// and let `conv ls` infer archival from that suffix — which mis-fires for a
// LIVE agent whose base name legitimately ends in `-x` (it self-hides). JOH-320
// flips every listing surface onto the column and demotes `-x` to a pure
// display convention. This migration makes that flip lossless for existing data
// by stamping the column on the generations the suffix used to hide.
//
// A reincarnation predecessor is recorded explicitly: it is the old_conv_id of
// an agent_conv_succession edge whose reason is 'reincarnate'. We derive
// archived_at from that edge's succeeded_at (the moment the successor took
// over), which is always a non-empty RFC3339 timestamp. Only rows still flagged
// active (empty archived_at) are touched, so the pass is idempotent and never
// overwrites a manual `conv archive` timestamp.
//
// Scoped to reason='reincarnate' to MATCH the forward path: only the agentd
// reincarnate orchestrator stamps archived_at on its predecessor. The other
// succession producer — Claude Code's /clear (reason='clear', see
// session/hook_callback.go) — does NOT `-x`-rename its predecessor and does NOT
// stamp the column, so a /clear predecessor was, and stays, visible in
// `conv ls`. Backfilling it would both hide a previously-visible conv and
// disagree with the (still-visible) forward /clear behaviour. Consistently
// hiding /clear generations too would be a deliberate, broader change — a
// separate follow-up, not smuggled into the -x-heuristic retirement.
//
// Deliberately NOT backfilled: a `-x`-titled conv that is not a reincarnate
// succession predecessor (a /clear predecessor, a pre-succession-table
// reincarnation, or a title a human typed by hand). Under JOH-320 the `-x`
// suffix no longer carries visibility weight, so such a conv reappears in
// `conv ls` — the sanctioned new behaviour; use `tclaude conv archive` to hide
// it explicitly. Going forward the live reincarnate path stamps the column
// itself (see agentd reincarnate), so no future backfill is required.
//
// Data-only: no schema change (the column and its index already exist), so the
// golden schema snapshot is unaffected. One transaction — a failure rolls back
// and the version stays at 81. The table probes make a partial-schema heal DB a
// clean skip.
func migrateV81toV82(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v81→v82: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveConv, err := txTableExists(tx, "conv_index")
	if err != nil {
		return fmt.Errorf("migrate v81→v82 (probe conv_index): %w", err)
	}
	haveSucc, err := txTableExists(tx, "agent_conv_succession")
	if err != nil {
		return fmt.Errorf("migrate v81→v82 (probe agent_conv_succession): %w", err)
	}

	// Both tables predate v81 in the normal chain; a partial-schema heal DB
	// missing either simply has nothing to backfill.
	if haveConv && haveSucc {
		if _, err := tx.Exec(`
			UPDATE conv_index
			SET archived_at = (
				SELECT s.succeeded_at FROM agent_conv_succession s
				WHERE s.old_conv_id = conv_index.conv_id
				  AND s.reason = 'reincarnate'
			)
			WHERE archived_at = ''
			  AND conv_id IN (
			      SELECT old_conv_id FROM agent_conv_succession
			      WHERE old_conv_id IS NOT NULL AND old_conv_id != ''
			        AND reason = 'reincarnate'
			  )`); err != nil {
			return fmt.Errorf("migrate v81→v82 (backfill archived_at): %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 82`); err != nil {
		return fmt.Errorf("migrate v81→v82 (version): %w", err)
	}
	return tx.Commit()
}
