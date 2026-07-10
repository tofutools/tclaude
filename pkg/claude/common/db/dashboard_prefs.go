package db

import (
	"database/sql"
	"errors"
	"time"
)

// dashboard_prefs is a flat key→value store for the browser dashboard's
// "sticky" view/config preferences — the settings that used to live in
// the browser's localStorage but were silently reset on every daemon
// restart because the dashboard is served on a fresh random loopback
// port each time (and localStorage is partitioned by origin, port
// included). Keys are the dashboard's own namespaced strings
// (e.g. "tclaude.dash.group.<name>", "tclaude.dash.sort"); values are
// stored verbatim. Most are opaque UI state; the global spawn-profile key is
// also read by agentd's spawn resolver so dashboard and CLI spawns share one
// default.

// SetDashboardPref upserts a single preference. value is stored as-is,
// including the empty string (distinct from "absent" — use
// DeleteDashboardPref for that, mirroring localStorage's removeItem).
func SetDashboardPref(key, value string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(
		`INSERT OR REPLACE INTO dashboard_prefs (key, value, updated_at) VALUES (?, ?, ?)`,
		key, value, time.Now().Format(time.RFC3339Nano))
	return err
}

// DeleteDashboardPref removes a preference. Deleting a missing key is a
// no-op (the dashboard's removeItem is likewise idempotent).
func DeleteDashboardPref(key string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM dashboard_prefs WHERE key = ?`, key)
	return err
}

// GetDashboardPref returns one stored preference and whether it exists. The
// value may itself be empty, so callers must use the bool to distinguish an
// explicitly stored empty string from a missing key.
func GetDashboardPref(key string) (string, bool, error) {
	db, err := Open()
	if err != nil {
		return "", false, err
	}
	var value string
	err = db.QueryRow(`SELECT value FROM dashboard_prefs WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetDashboardProfileRef atomically stores the durable profile id used by the
// spawn resolver and the current-name snapshot consumed by older dashboards.
func SetDashboardProfileRef(nameKey, idKey, name string, id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT OR REPLACE INTO dashboard_prefs (key, value, updated_at) VALUES (?, ?, ?)`, nameKey, name, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO dashboard_prefs (key, value, updated_at) VALUES (?, CAST(? AS TEXT), ?)`, idKey, id, now); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteDashboardProfileRef atomically clears both halves of the reference.
func DeleteDashboardProfileRef(nameKey, idKey string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM dashboard_prefs WHERE key IN (?, ?)`, nameKey, idKey)
	return err
}

// ListDashboardPrefs returns every stored preference as a key→value
// map — the whole set the dashboard loads in one shot on page open.
func ListDashboardPrefs() (map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT key, value FROM dashboard_prefs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
