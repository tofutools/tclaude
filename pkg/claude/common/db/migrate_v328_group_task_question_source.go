package db

import "database/sql"

// migrateV327toV328 gives a question for the operator its source
// (tk_8b2c86f7): a question mirrored onto the board from another place the
// operator answers ("notify:<message id>" for a notify-human question,
// "approval:<request id>" for an --ask-human prompt) names it, at most once,
// so mirroring is idempotent. A question asked on the board has none.
func migrateV327toV328(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE group_task_questions ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_group_task_questions_source ON group_task_questions(source) WHERE source <> ''`,
		`UPDATE schema_version SET version = 328`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
