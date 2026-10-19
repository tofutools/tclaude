package db

import (
	"errors"
	"time"
)

// SetCostConvIgnored excludes convID's spend from every cost total, or
// restores it. Its recorded history is never modified, so a restore brings the
// spend back exactly. Ignoring an already ignored conversation keeps the
// original time.
func SetCostConvIgnored(convID string, ignored bool) error {
	if convID == "" {
		return errors.New("cost ignore: empty conv id")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if !ignored {
		_, err = d.Exec(`DELETE FROM cost_ignored_convs WHERE conv_id = ?`, convID)
		return err
	}
	_, err = d.Exec(`INSERT INTO cost_ignored_convs (conv_id, ignored_at) VALUES (?, ?)
		ON CONFLICT(conv_id) DO NOTHING`, convID, dbTime(time.Now()))
	return err
}

// CostIgnoredConvs returns every ignored conversation with the time it was
// ignored.
func CostIgnoredConvs() (map[string]time.Time, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, ignored_at FROM cost_ignored_convs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var convID string
		var at dbTimestamp
		if err := rows.Scan(&convID, &at); err != nil {
			return nil, err
		}
		out[convID] = at.time
	}
	return out, rows.Err()
}
