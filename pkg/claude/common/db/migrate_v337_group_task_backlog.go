package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// migrateV336toV337 adds the task board's Backlog stage (tk_5d2547b0): parked
// work before the ideas, never started, queued or nudged.
//
// group_tasks.stage is CHECKed to idea and next, and SQLite cannot widen a
// CHECK in place, so the table is rebuilt from its own current DDL with only
// that constraint changed: create the replacement, copy the rows, drop the
// original, rename. Other tables reference group_tasks(id) by name, so the
// foreign keys are off for the rebuild and checked explicitly before commit,
// and every trigger that names group_tasks (its own, and those of other
// tables that update it) is suspended meanwhile and restored after the
// rename.
//
// Two triggers carry the stage's invariants: a parked task is never started
// (group_tasks_backlog_unstarted clears started_at), and it waits in no
// work-in-progress queue (group_tasks_queued_clear now clears on backlog as
// on idea). group_tasks_column_clear already takes a task out of its column
// on any stage but next.
func migrateV336toV337(d *sql.DB) error {
	ctx := context.Background()
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate v336→v337 (task board backlog): connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// foreign_keys is connection-scoped and may only be changed outside a
	// transaction.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("migrate v336→v337: disable foreign keys: %w", err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate v336→v337: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := rebuildGroupTasksForBacklog(ctx, tx); err != nil {
		return fmt.Errorf("migrate v336→v337: %w", err)
	}
	if err := assertForeignKeyGraphIntact(ctx, tx, "migrate v336→v337"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET version = 337`); err != nil {
		return fmt.Errorf("migrate v336→v337: version: %w", err)
	}
	return tx.Commit()
}

const (
	groupTasksStageCheckV317  = `CHECK(stage IN ('idea','next'))`
	groupTasksStageCheckV337  = `CHECK(stage IN ('backlog','idea','next'))`
	groupTasksQueuedIdeaV335  = `OR NEW.stage = 'idea'`
	groupTasksQueuedStageV337 = `OR NEW.stage IN ('idea','backlog')`
)

func rebuildGroupTasksForBacklog(ctx context.Context, tx *sql.Tx) error {
	var createSQL string
	if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'group_tasks'`).Scan(&createSQL); err != nil {
		return fmt.Errorf("read group_tasks schema: %w", err)
	}
	if strings.Contains(createSQL, groupTasksStageCheckV337) {
		return nil // already widened (a re-run)
	}
	if strings.Count(createSQL, groupTasksStageCheckV317) != 1 {
		return fmt.Errorf("group_tasks schema has no single %s to widen", groupTasksStageCheckV317)
	}
	const head = "CREATE TABLE group_tasks ("
	if !strings.HasPrefix(createSQL, head) {
		return fmt.Errorf("group_tasks schema does not start with %q", head)
	}
	const temp = "group_tasks_v337"
	replacement := "CREATE TABLE " + temp + " (" + strings.TrimPrefix(
		strings.Replace(createSQL, groupTasksStageCheckV317, groupTasksStageCheckV337, 1), head)

	var indexes []string
	rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = 'group_tasks'
		AND sql IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("read group_tasks indexes: %w", err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return err
		}
		indexes = append(indexes, s)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	triggers, err := suspendGroupTasksTriggers(ctx, tx)
	if err != nil {
		return fmt.Errorf("suspend triggers: %w", err)
	}
	for _, stmt := range []string{
		replacement,
		`INSERT INTO ` + temp + ` SELECT * FROM group_tasks`,
		`DROP TABLE group_tasks`,
		`ALTER TABLE ` + temp + ` RENAME TO group_tasks`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rebuild group_tasks: %w", err)
		}
	}
	for _, s := range indexes {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("recreate index: %w", err)
		}
	}
	for _, s := range triggers {
		if strings.Contains(s, "group_tasks_queued_clear") {
			s = strings.Replace(s, groupTasksQueuedIdeaV335, groupTasksQueuedStageV337, 1)
		}
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("restore trigger: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS group_tasks_backlog_unstarted
			AFTER UPDATE OF stage, started_at ON group_tasks
			WHEN NEW.stage = 'backlog' AND NEW.started_at IS NOT NULL
			BEGIN UPDATE group_tasks SET started_at = NULL WHERE id = NEW.id; END`)
	return err
}

// suspendGroupTasksTriggers drops every trigger whose definition names
// group_tasks and returns their statements, in creation order.
func suspendGroupTasksTriggers(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_master
		WHERE type = 'trigger' AND sql IS NOT NULL AND (tbl_name = 'group_tasks' OR sql LIKE '%group_tasks%') ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var names, statements []string
	for rows.Next() {
		var name, statement string
		if err := rows.Scan(&name, &statement); err != nil {
			_ = rows.Close()
			return nil, err
		}
		names = append(names, name)
		statements = append(statements, statement)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER `+quoteIdentifier(name)); err != nil {
			return nil, err
		}
	}
	return statements, nil
}
