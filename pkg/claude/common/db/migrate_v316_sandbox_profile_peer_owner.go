package db

import "database/sql"

// migrateV315toV316 records which linked node created a sandbox profile
// (S10b): a restricted peer edits and deletes only the profiles it created.
// Keyed on the profile's ID, so a rename keeps it; the row goes with the
// profile.
func migrateV315toV316(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A sparse database with no sandbox profiles has nothing to own.
	var hasProfiles int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='sandbox_profiles')`).Scan(&hasProfiles); err != nil {
		return err
	}
	stmts := []string{`UPDATE schema_version SET version = 316`}
	if hasProfiles != 0 {
		stmts = append([]string{`CREATE TABLE IF NOT EXISTS sandbox_profile_peer_owners (
			profile_id INTEGER PRIMARY KEY REFERENCES sandbox_profiles(id) ON DELETE CASCADE,
			peer       TEXT NOT NULL
		)`}, stmts...)
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
