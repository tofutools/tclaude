package db

import "database/sql"

// No historical clear edge is backfilled: legacy writers used that reason for
// both new histories and reference-only rotations. Intake seeds observed facts.
func migrateV226toV227(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS logical_conversations (
 id TEXT PRIMARY KEY, created_at INTEGER NOT NULL
) STRICT;
CREATE TABLE IF NOT EXISTS conversation_attempt_bindings (
 execution_id TEXT PRIMARY KEY, session_id TEXT NOT NULL,
 process_instance TEXT NOT NULL, main_process INTEGER NOT NULL CHECK(main_process IN (0, 1)),
 evidence_strength TEXT NOT NULL, evidence_source TEXT NOT NULL,
 observed_pid INTEGER NOT NULL, observed_tmux_session TEXT NOT NULL,
 observed_pane_id TEXT NOT NULL,
 harness TEXT NOT NULL, namespace TEXT NOT NULL,
 conversation_id TEXT NOT NULL, external_ref TEXT NOT NULL, revision INTEGER NOT NULL CHECK(revision > 0),
 FOREIGN KEY(conversation_id) REFERENCES logical_conversations(id)
) STRICT;
CREATE INDEX IF NOT EXISTS conversation_attempt_session ON conversation_attempt_bindings(session_id);
CREATE TABLE IF NOT EXISTS conversation_reference_bindings (
 execution_id TEXT NOT NULL, revision INTEGER NOT NULL,
 expected_revision INTEGER NOT NULL CHECK(expected_revision >= 0), evidence_id TEXT NOT NULL,
 session_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
 harness TEXT NOT NULL, namespace TEXT NOT NULL, external_ref TEXT NOT NULL,
 transition TEXT NOT NULL, process_instance TEXT NOT NULL,
 main_process INTEGER NOT NULL CHECK(main_process IN (0, 1)), evidence_strength TEXT NOT NULL,
 evidence_source TEXT NOT NULL, observed_pid INTEGER NOT NULL,
 observed_tmux_session TEXT NOT NULL, observed_pane_id TEXT NOT NULL,
 admitted_at INTEGER NOT NULL,
 PRIMARY KEY(execution_id, revision),
 UNIQUE(execution_id, evidence_id),
 FOREIGN KEY(conversation_id) REFERENCES logical_conversations(id)
) STRICT;
CREATE INDEX IF NOT EXISTS conversation_external_lookup ON conversation_reference_bindings(harness, namespace, external_ref);
UPDATE schema_version SET version = 227;
`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
