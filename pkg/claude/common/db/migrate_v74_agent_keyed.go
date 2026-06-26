package db

import (
	"database/sql"
	"fmt"
)

// migrateV73toV74 cuts the remaining conv-keyed identity-bearing tables over to
// the stable agent_id introduced in v72 (JOH-26 PR3a). After v73 re-keyed the
// authorization tables (memberships / ownerships / permissions / sudo / notify),
// the rate-limit / scheduling subjects were still keyed on the rotating conv_id:
//
//	agent_cron_jobs   owner_conv      → owner_agent
//	                  target_conv     → target_agent
//	agent_spawn_history spawner_conv_id → spawner_agent_id
//	agent_clone_history source_conv_id  → source_agent_id
//
// After this, a reincarnate / Claude Code /clear no longer needs to rewrite any
// of these refs — the actor's agent_id never moves, so a cron job stays owned by
// the same actor (and the fire path resolves owner/target agent → current_conv
// at fire time), and a spawn/clone rate limit follows the actor across conv
// rotations instead of resetting when the conv-id rotates.
//
// Why RENAME COLUMN + an in-place value transform instead of the v73-style
// shadow-table rebuild (CREATE _vN → INSERT … SELECT JOIN → DROP → RENAME):
//
//   - agent_cron_jobs is REFERENCED BY agent_cron_runs (FK ON DELETE CASCADE).
//     With foreign_keys enforced (the DB opens with _pragma=foreign_keys(1)),
//     `DROP TABLE agent_cron_jobs` performs an implicit DELETE of every row
//     first, which would CASCADE-delete the entire cron run history. The
//     SQLite-recommended "12-step" rebuild dance (disable foreign_keys, rebuild,
//     re-enable) cannot run cleanly here because the foreign_keys pragma is a
//     no-op inside a transaction and toggling it across the pooled migration
//     connection is fragile. (See migrateV35toV36's note: a shadow rebuild is
//     only safe "even with foreign_keys enforced" when the table is NOT
//     referenced by FKs elsewhere — which agent_cron_jobs is.)
//   - The spawn/clone history tables are single-column and have no UNIQUE/PK
//     collapse to worry about (multiple rate-limit rows per actor is the point),
//     so a rename + transform is both simpler and lossless.
//
// RENAME COLUMN preserves the table's id/AUTOINCREMENT sequence (cron_runs keep
// pointing at the same job ids) and auto-updates the column's indexes, so no
// index recreation is needed. The whole thing runs in ONE transaction (SQLite
// DDL is transactional), so a failure rolls back and the version stays at 73.
//
// Safety mirrors v73:
//   - A VACUUM INTO snapshot is written before any destructive change.
//   - backfillAgents is re-run first so every conv referenced by these tables
//     maps to an actor (collectAgentConvs already reaches owner_conv /
//     target_conv / spawner_conv_id / source_conv_id).
//   - A STRICT coverage gate (unmappedV74Rows) aborts — version stays 73,
//     snapshot intact — if any non-empty ref has no actor mapping after the
//     backfill, rather than letting the value transform silently blank it.
func migrateV73toV74(db *sql.DB) error {
	// Pre-cutover snapshot (best-effort).
	vacuumBackup(db, ".pre-v74-agentkey.bak")

	// agent_conversations is the resolution spine. Probe it FIRST: on a
	// partial-schema heal DB that lacks it there is nothing to cut over, and
	// the coverage backfill would just error on the missing tables. Normally
	// v72 created+populated it, so the probe passes and we proceed.
	if ok, err := tableExists(db, "agent_conversations"); err != nil {
		return fmt.Errorf("migrate v73→v74 (probe agent_conversations): %w", err)
	} else if !ok {
		if _, err := db.Exec(`UPDATE schema_version SET version = 74`); err != nil {
			return fmt.Errorf("migrate v73→v74 (version, no-op): %w", err)
		}
		return nil
	}

	// Guarantee every conv referenced by these tables maps to an actor, so the
	// conv_id → agent_id transform below never blanks a row. Idempotent.
	if err := backfillAgents(db); err != nil {
		return fmt.Errorf("migrate v73→v74 (coverage backfill): %w", err)
	}

	// Strict coverage gate. The value transform below maps each non-empty
	// conv ref to its actor and would blank (COALESCE → '') any ref with no
	// mapping. backfillAgents above is meant to guarantee full coverage, but it
	// degrades gracefully (logs + skips) on a corrupted succession mapping — a
	// posture that is right for the additive backfill but wrong at this
	// destructive cutover. So before touching anything we refuse to proceed if a
	// single ref would be dropped: the version stays at 73, the snapshot is
	// intact, and the operator can investigate (cron jobs in particular are
	// precious, human-authored schedules we must not silently de-target).
	if unmapped, err := unmappedV74Rows(db); err != nil {
		return fmt.Errorf("migrate v73→v74 (coverage check): %w", err)
	} else if len(unmapped) > 0 {
		return fmt.Errorf("migrate v73→v74: refusing to cut over — identity refs have no agent mapping "+
			"after backfill (would be blanked by the transform): %v; the pre-cutover DB is unchanged "+
			"(snapshot at <db>.pre-v74-agentkey.bak)", unmapped)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v73→v74: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	renames := []agentKeyColRename{
		{table: "agent_cron_jobs", oldCol: "owner_conv", newCol: "owner_agent"},
		{table: "agent_cron_jobs", oldCol: "target_conv", newCol: "target_agent"},
		{table: "agent_spawn_history", oldCol: "spawner_conv_id", newCol: "spawner_agent_id"},
		{table: "agent_clone_history", oldCol: "source_conv_id", newCol: "source_agent_id"},
	}
	for _, r := range renames {
		if err := r.run(tx); err != nil {
			return fmt.Errorf("migrate v73→v74 (%s.%s): %w", r.table, r.oldCol, err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 74`); err != nil {
		return fmt.Errorf("migrate v73→v74 (version): %w", err)
	}
	return tx.Commit()
}

// agentKeyColRename describes one in-place conv_id → agent_id column cutover:
// rename the column, then rewrite each non-empty value from its conv to that
// conv's owning actor. Guarded so a re-run (column already renamed) or a
// partial-schema heal DB (table absent) is a clean no-op.
type agentKeyColRename struct {
	table  string
	oldCol string // the conv-keyed column to rename + transform
	newCol string // the agent-keyed column name it becomes
}

func (r agentKeyColRename) run(tx *sql.Tx) error {
	// Table present? A partial-schema heal DB may not have created it.
	var haveTable int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, r.table,
	).Scan(&haveTable); err != nil {
		return fmt.Errorf("probe table: %w", err)
	}
	if haveTable == 0 {
		return nil
	}
	// Old column still present? Idempotent: a re-run after a half-applied
	// attempt finds it already renamed and skips.
	var haveOld int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, r.table, r.oldCol,
	).Scan(&haveOld); err != nil {
		return fmt.Errorf("probe column: %w", err)
	}
	if haveOld == 0 {
		return nil
	}

	// Rename keeps the data + the column's indexes (SQLite auto-rewrites index
	// references on RENAME COLUMN), so the column now holds conv ids under its
	// new name until the transform below rewrites them.
	if _, err := tx.Exec(
		`ALTER TABLE ` + r.table + ` RENAME COLUMN ` + r.oldCol + ` TO ` + r.newCol,
	); err != nil {
		return fmt.Errorf("rename column: %w", err)
	}

	// Transform each non-empty value conv → owning agent_id. The coverage gate
	// already guaranteed every non-empty ref maps, so COALESCE(...,'') is
	// belt-and-braces (an unmapped ref would survive as the gate-rejected case,
	// never reached here). An empty ref ('' — a group-target/owner-less cron
	// job, never a spawn/clone subject) is left as ''. The table / column names
	// come from the hardcoded rename list above, never user input.
	if _, err := tx.Exec(
		`UPDATE ` + r.table + ` SET ` + r.newCol +
			` = COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ` + r.table + `.` + r.newCol + `), '')` +
			` WHERE ` + r.newCol + ` != ''`,
	); err != nil {
		return fmt.Errorf("transform values: %w", err)
	}
	return nil
}

// unmappedV74Rows reports, per source ref, how many rows carry a non-empty conv
// that has no agent_conversations mapping — i.e. refs the conv → agent_id
// transform would blank. An empty map means full coverage (safe to cut over).
// Column-aware: a ref whose column is absent or already renamed contributes
// nothing, so the check is a no-op on a re-run or a partial-schema heal DB.
func unmappedV74Rows(d *sql.DB) (map[string]int, error) {
	out := map[string]int{}
	for _, ref := range []struct{ table, col string }{
		{"agent_cron_jobs", "owner_conv"},
		{"agent_cron_jobs", "target_conv"},
		{"agent_spawn_history", "spawner_conv_id"},
		{"agent_clone_history", "source_conv_id"},
	} {
		ok, err := columnExists(d, ref.table, ref.col)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue // table missing or already agent-keyed — nothing to map
		}
		// Only NON-EMPTY refs need an actor: '' is a legitimate ownerless /
		// targetless cron ref (a group-target job, or a human-scheduled job
		// with no owner attribution). conv_id is the PK of agent_conversations,
		// so NOT IN carries no NULL-semantics surprise. The names come from the
		// hardcoded list above, never user input.
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM `+ref.table+
			` WHERE `+ref.col+` != '' AND `+ref.col+
			` NOT IN (SELECT conv_id FROM agent_conversations)`).Scan(&n); err != nil {
			return nil, fmt.Errorf("%s.%s: %w", ref.table, ref.col, err)
		}
		if n > 0 {
			out[ref.table+"."+ref.col] = n
		}
	}
	return out, nil
}
