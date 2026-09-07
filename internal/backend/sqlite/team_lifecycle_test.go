package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpenMigratesLegacyTeamDeploymentBeforeCreatingRequestIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-team.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE team_deployments (
		id TEXT PRIMARY KEY, definition_json BLOB NOT NULL, dependency_closure_json BLOB NOT NULL,
		mission TEXT NOT NULL, parameters_json BLOB NOT NULL, group_id TEXT NOT NULL,
		members_json BLOB NOT NULL, automation_rule_ids_json BLOB NOT NULL, work_run_id TEXT NOT NULL,
		advisory_phase INTEGER NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("open and migrate legacy database: %v", err)
	}
	defer store.Close()

	var indexName string
	if err = store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name='team_deployments_scoped_request'`).Scan(&indexName); err != nil {
		t.Fatalf("scoped request index was not created after column migration: %v", err)
	}
}
