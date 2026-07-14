package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// registryIDByName resolves a human-facing registry name to its durable row
// id. A blank or currently missing name returns NULL, preserving the legacy
// dangling-reference behaviour without allowing a later name reuse to hijack
// an already-resolved reference.
func registryIDByName(tx *sql.Tx, table, name string) (sql.NullInt64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return sql.NullInt64{}, nil
	}
	if table != "spawn_profiles" && table != "group_templates" && table != "sandbox_profiles" {
		return sql.NullInt64{}, fmt.Errorf("unsupported registry %q", table)
	}
	var id int64
	query := "SELECT id FROM " + table + " WHERE name = ?"
	if table == "spawn_profiles" {
		query = `SELECT p.id FROM spawn_profiles p
			LEFT JOIN spawn_profile_aliases a ON a.profile_id = p.id
			WHERE p.name = ? OR a.alias = ? LIMIT 1`
		err := tx.QueryRow(query, name, name).Scan(&id)
		if err == sql.ErrNoRows {
			return sql.NullInt64{}, nil
		}
		if err != nil {
			return sql.NullInt64{}, err
		}
		return sql.NullInt64{Int64: id, Valid: true}, nil
	}
	err := tx.QueryRow(query, name).Scan(&id)
	if err == sql.ErrNoRows {
		return sql.NullInt64{}, nil
	}
	if err != nil {
		return sql.NullInt64{}, err
	}
	return sql.NullInt64{Int64: id, Valid: true}, nil
}

func registryIDByNameDB(d *sql.DB, table, name string) (sql.NullInt64, error) {
	tx, err := d.Begin()
	if err != nil {
		return sql.NullInt64{}, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := registryIDByName(tx, table, name)
	if err != nil {
		return sql.NullInt64{}, err
	}
	if err := tx.Commit(); err != nil {
		return sql.NullInt64{}, err
	}
	return id, nil
}
