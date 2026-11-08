package db

import "database/sql"

// migrateV325toV326 adds federation_peer_questions (tk_976fe76c): a
// question this node's agent asked a linked node's operator
// (`taskboard ask-operator --peer`). The answer comes back as federation
// mail tagged with the task id; a mail that matches a row here is let in
// once, as a reply to the question would be, without a message.direct
// grant. A row is keyed on (peer, task, agent), single-use and bounded by
// expires_at.
func migrateV325toV326(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS federation_peer_questions (
			peer       TEXT NOT NULL,
			task       TEXT NOT NULL,
			agent      TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (peer, task, agent)
		) STRICT`,
		`UPDATE schema_version SET version = 326`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
