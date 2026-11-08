package db

import "database/sql"

// migrateV322toV323 lets the operator hold a task (tcl-p4t78f):
//
//   - assignee_kind is '' (assignee_id says who holds it, if anyone) or
//     'operator': the operator of this node holds it. assignee_id is an FK to
//     agents, so an operator task keeps it NULL; assigned_by and assigned_at
//     record who asked and when, as for an agent.
//   - operator_request is reserved for the operator's question payload
//     (options, the answer) as JSON, so question tasks (tcl-sghh7j) need no
//     further migration. Nothing writes it yet.
//   - blocked_on names the operator task a blocked task waits on. Closing
//     that task as done reopens the waiting task; a task blocked for any
//     other reason stays blocked.
func migrateV322toV323(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_tasks ADD COLUMN assignee_kind TEXT NOT NULL DEFAULT '' CHECK(assignee_kind IN ('','operator'))`,
		`ALTER TABLE group_tasks ADD COLUMN operator_request TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE group_tasks ADD COLUMN blocked_on TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_group_tasks_blocked_on ON group_tasks(blocked_on) WHERE blocked_on <> ''`,
		`UPDATE schema_version SET version = 323`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
