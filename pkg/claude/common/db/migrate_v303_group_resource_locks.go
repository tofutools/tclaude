package db

import "database/sql"

// migrateV302toV303 adds group_resource_locks (tcl-dwzsq0): the operator's
// lock that keeps every agent from changing a group resource (a process
// template, a board). A row means locked; only the operator's dashboard sets
// or clears one (group_resource_locks.go).
func migrateV302toV303(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS group_resource_locks (
		kind        TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		locked_at   INTEGER NOT NULL,
		PRIMARY KEY (kind, resource_id)
	) STRICT`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 303`); err != nil {
		return err
	}
	return tx.Commit()
}
