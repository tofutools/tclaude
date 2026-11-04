package db

import (
	"strings"
	"time"
)

// RelaunchPolicyRow is one stored relaunch-policy answer: in Scope, Field
// relaunches with Mode. The vocabulary (scopes, fields, modes) is
// launchspec's; callers validate against it before writing.
type RelaunchPolicyRow struct {
	Scope     string
	Field     string
	Mode      string
	UpdatedAt time.Time
	UpdatedBy string
}

// ListRelaunchPolicyRows returns every stored relaunch-policy row, ordered by
// scope and field.
func ListRelaunchPolicyRows() ([]RelaunchPolicyRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT scope, field, mode, updated_at, updated_by FROM relaunch_policy ORDER BY scope, field`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []RelaunchPolicyRow{}
	for rows.Next() {
		var row RelaunchPolicyRow
		var at dbTimestamp
		if err := rows.Scan(&row.Scope, &row.Field, &row.Mode, &at, &row.UpdatedBy); err != nil {
			return nil, err
		}
		row.UpdatedAt = at.Time()
		out = append(out, row)
	}
	return out, rows.Err()
}

// SetRelaunchPolicyRow stores the mode for scope and field, replacing any
// earlier answer.
func SetRelaunchPolicyRow(scope, field, mode, by string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO relaunch_policy (scope, field, mode, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(scope, field) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		strings.TrimSpace(scope), strings.TrimSpace(field), strings.TrimSpace(mode), dbTime(time.Now()), by)
	return err
}

// ClearRelaunchPolicyRow removes the stored answer for scope and field, so
// the field takes its default again. It reports whether a row was removed.
func ClearRelaunchPolicyRow(scope, field string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM relaunch_policy WHERE scope = ? AND field = ?`, strings.TrimSpace(scope), strings.TrimSpace(field))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
