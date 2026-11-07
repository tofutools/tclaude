package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// TestMigrateV319toV320_RenamesBoardsToDrawingBoards seeds the v319 shape of
// the collab board tables and everything that stores the board kind or the
// boards.* slugs, migrates, and reads it all back under the new names.
func TestMigrateV319toV320_RenamesBoardsToDrawingBoards(t *testing.T) {
	d, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "v319.sqlite"))
	require.NoError(t, err)
	defer func() { _ = d.Close() }()

	_, err = d.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version (version) VALUES (319);
		CREATE TABLE agents (agent_id TEXT PRIMARY KEY, retired_at INTEGER);
		CREATE TABLE agent_groups (id INTEGER PRIMARY KEY);
		CREATE TABLE collab_boards (id TEXT PRIMARY KEY, title TEXT NOT NULL, group_id INTEGER REFERENCES agent_groups(id) ON DELETE SET NULL) STRICT;
		CREATE INDEX idx_collab_boards_group ON collab_boards(group_id);
		CREATE TABLE collab_board_elements (board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE, element_id TEXT NOT NULL, server_version INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(board_id, element_id)) STRICT;
		CREATE INDEX idx_collab_board_elements_server_version ON collab_board_elements(board_id, server_version);
		CREATE TABLE collab_board_participants (board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE, agent_id TEXT NOT NULL, PRIMARY KEY(board_id, agent_id)) STRICT;
		CREATE INDEX idx_collab_board_participants_agent ON collab_board_participants(agent_id);
		CREATE TABLE collab_board_history (id INTEGER PRIMARY KEY, board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE, actor TEXT NOT NULL, stack TEXT NOT NULL) STRICT;
		CREATE INDEX idx_collab_board_history_stack ON collab_board_history(board_id, actor, stack, id);
		CREATE TABLE collab_board_sessions (board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE, agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE, PRIMARY KEY(board_id, agent_id)) STRICT;
		CREATE INDEX idx_collab_board_sessions_agent ON collab_board_sessions(agent_id);
		CREATE TRIGGER collab_board_sessions_agent_retired
		 AFTER UPDATE OF retired_at ON agents WHEN NEW.retired_at IS NOT NULL BEGIN
		 DELETE FROM collab_board_sessions WHERE agent_id = NEW.agent_id;
		END;
		CREATE TABLE group_resource_locks (kind TEXT NOT NULL, resource_id TEXT NOT NULL, locked_at INTEGER NOT NULL, PRIMARY KEY (kind, resource_id)) STRICT;
		CREATE TABLE human_messages (id INTEGER PRIMARY KEY, action TEXT NOT NULL DEFAULT '');
		CREATE TABLE agent_permissions (agent_id TEXT NOT NULL, slug TEXT NOT NULL, scope_json TEXT NOT NULL DEFAULT '', PRIMARY KEY (agent_id, slug));

		INSERT INTO agents (agent_id) VALUES ('agt_a'), ('agt_b');
		INSERT INTO collab_boards (id, title) VALUES ('plan', 'Plan');
		INSERT INTO collab_board_elements (board_id, element_id) VALUES ('plan', 'e1');
		INSERT INTO collab_board_sessions (board_id, agent_id) VALUES ('plan', 'agt_a');
		INSERT INTO group_resource_locks VALUES ('board', 'plan', 1), ('process', 'release', 1);
		INSERT INTO human_messages (action) VALUES ('board:plan'), ('tab:groups'), ('terminal');
		INSERT INTO agent_permissions (agent_id, slug, scope_json) VALUES
			('agt_a', 'boards.read', ''),
			('agt_a', 'boards.write', ''),
			('agt_a', 'drawing-boards.write', ''),
			('agt_b', 'resources.read', '{"resource":["board:plan","process_template:release"]}');
	`)
	require.NoError(t, err, "seed v319")

	require.NoError(t, migrateV319toV320(d))

	var version int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&version))
	assert.Equal(t, 320, version)

	var title, element string
	require.NoError(t, d.QueryRow(`SELECT b.title, e.element_id FROM collab_drawing_boards b
		JOIN collab_drawing_board_elements e ON e.drawing_board_id = b.id`).Scan(&title, &element))
	assert.Equal(t, "Plan", title)
	assert.Equal(t, "e1", element)

	var old int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE '%collab_board%'`).Scan(&old))
	assert.Zero(t, old, "no table, index or trigger keeps the old name")

	// The renamed trigger still ends a retired agent's sessions.
	_, err = d.Exec(`UPDATE agents SET retired_at = 1 WHERE agent_id = 'agt_a'`)
	require.NoError(t, err)
	var sessions int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM collab_drawing_board_sessions`).Scan(&sessions))
	assert.Zero(t, sessions)

	assert.Equal(t, []string{"drawing_board:plan", "process:release"},
		queryV320Strings(t, d, `SELECT kind || ':' || resource_id FROM group_resource_locks ORDER BY kind`))
	assert.Equal(t, []string{"drawing_board:plan", "tab:groups", "terminal"},
		queryV320Strings(t, d, `SELECT action FROM human_messages ORDER BY id`))
	assert.Equal(t, []string{"agt_a drawing-boards.read", "agt_a drawing-boards.write", "agt_b resources.read"},
		queryV320Strings(t, d, `SELECT agent_id || ' ' || slug FROM agent_permissions ORDER BY agent_id, slug`),
		"slugs move, and an already-canonical grant wins the collision")
	var scope string
	require.NoError(t, d.QueryRow(`SELECT scope_json FROM agent_permissions WHERE agent_id = 'agt_b'`).Scan(&scope))
	assert.JSONEq(t, `{"resource":["drawing_board:plan","process_template:release"]}`, scope)
}

func queryV320Strings(t *testing.T, d *sql.DB, query string) []string {
	t.Helper()
	rows, err := d.Query(query)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}
