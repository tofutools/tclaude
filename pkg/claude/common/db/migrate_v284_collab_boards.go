package db

import "database/sql"

// Collab boards (tcl-4f86kp): a shared drawing board for the operator and
// agents. Elements are a bounded Excalidraw subset kept as normalized JSON;
// element_version / nonce drive the per-element merge and server_version is
// the board version that last accepted the element (for `since` deltas).
const collabBoardsSchema = `CREATE TABLE IF NOT EXISTS collab_boards (
 id TEXT PRIMARY KEY,
 title TEXT NOT NULL,
 version INTEGER NOT NULL DEFAULT 0,
 created_at INTEGER NOT NULL,
 updated_at INTEGER NOT NULL,
 updated_by TEXT NOT NULL DEFAULT '',
 snapshot_version INTEGER,
 snapshot_at INTEGER
) STRICT`

const collabBoardElementsSchema = `CREATE TABLE IF NOT EXISTS collab_board_elements (
 board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE,
 element_id TEXT NOT NULL,
 element_version INTEGER NOT NULL,
 nonce INTEGER NOT NULL,
 server_version INTEGER NOT NULL,
 is_deleted INTEGER NOT NULL DEFAULT 0,
 body TEXT NOT NULL,
 PRIMARY KEY(board_id, element_id)
) STRICT`

const collabBoardParticipantsSchema = `CREATE TABLE IF NOT EXISTS collab_board_participants (
 board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
 added_at INTEGER NOT NULL,
 PRIMARY KEY(board_id, agent_id)
) STRICT`

func migrateV283toV284(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		collabBoardsSchema,
		collabBoardElementsSchema,
		`CREATE INDEX IF NOT EXISTS idx_collab_board_elements_server_version ON collab_board_elements(board_id, server_version)`,
		collabBoardParticipantsSchema,
		`CREATE INDEX IF NOT EXISTS idx_collab_board_participants_agent ON collab_board_participants(agent_id)`,
		`UPDATE schema_version SET version = 284`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
