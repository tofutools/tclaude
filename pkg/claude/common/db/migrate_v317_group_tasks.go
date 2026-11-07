package db

import "database/sql"

// migrateV316toV317 adds the group task board (tcl-uzmtah, epic tcl-n7x41z):
// a short-term, ordered list of tasks per agent group, with dependencies and a
// bounded history. The board is a plain store: it holds no execution state.
//
// stage is the operator's own line between a captured idea and the queue;
// status is what happened to the task. rank is a lexicographic key ordering
// tasks within a stage, so a move rewrites one row. The assignee columns are
// present for the assign/report slice and unused until then; assignee_id is
// NULL rather than empty when unset so the foreign key holds.
func migrateV316toV317(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_tasks (
			id           TEXT PRIMARY KEY,
			group_id     INTEGER NOT NULL REFERENCES agent_groups(id) ON DELETE CASCADE,
			title        TEXT NOT NULL,
			body         TEXT NOT NULL DEFAULT '',
			stage        TEXT NOT NULL CHECK(stage IN ('idea','next')),
			status       TEXT NOT NULL CHECK(status IN ('open','blocked','done','dropped')),
			rank         TEXT NOT NULL,
			block_reason TEXT NOT NULL DEFAULT '',
			ref_kind     TEXT NOT NULL DEFAULT '' CHECK(ref_kind IN ('','awb','linear','github','url')),
			ref_url      TEXT NOT NULL DEFAULT '',
			ref_label    TEXT NOT NULL DEFAULT '',
			assignee_id  TEXT REFERENCES agents(agent_id) ON DELETE SET NULL,
			assigned_by  TEXT NOT NULL DEFAULT '',
			assigned_at  INTEGER,
			outcome      TEXT NOT NULL DEFAULT '' CHECK(outcome IN ('','succeeded','failed')),
			outcome_note TEXT NOT NULL DEFAULT '',
			failures     INTEGER NOT NULL DEFAULT 0,
			created_by   TEXT NOT NULL,
			created_at   INTEGER NOT NULL,
			updated_at   INTEGER NOT NULL,
			closed_at    INTEGER,
			archived_at  INTEGER
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_group ON group_tasks(group_id, status, stage, rank)`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_closed ON group_tasks(closed_at) WHERE closed_at IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS group_task_deps (
			task_id    TEXT NOT NULL REFERENCES group_tasks(id) ON DELETE CASCADE,
			depends_on TEXT NOT NULL REFERENCES group_tasks(id) ON DELETE CASCADE,
			PRIMARY KEY (task_id, depends_on),
			CHECK (task_id <> depends_on)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_deps_on ON group_task_deps(depends_on)`,
		`CREATE TABLE IF NOT EXISTS group_task_events (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL REFERENCES group_tasks(id) ON DELETE CASCADE,
			at      INTEGER NOT NULL,
			actor   TEXT NOT NULL,
			kind    TEXT NOT NULL,
			detail  TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_events_task ON group_task_events(task_id, id)`,
		`UPDATE schema_version SET version = 317`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
