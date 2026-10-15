package db

import (
	"database/sql"
	"errors"
	"time"
)

// Process templates in the library and on groups (schema v265). See
// migrateV264toV265: listing and pins are tclaude-side metadata, kept out of
// template content.

// MaxProcessGroupPins bounds one group's pinned processes.
const MaxProcessGroupPins = 50

// ErrProcessGroupPinsFull is returned when a group already holds
// MaxProcessGroupPins processes.
var ErrProcessGroupPinsFull = errors.New("the group already has the maximum number of saved processes")

// ProcessTemplateListed reports whether a template lists in the library. A
// template without a listing row is listed.
func ProcessTemplateListed(templateID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var listed int
	err = d.QueryRow(`SELECT listed FROM process_template_listing WHERE template_id = ?`, templateID).Scan(&listed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return listed == 1, err
}

// ProcessTemplateUnlisted returns the ids of every unlisted template.
func ProcessTemplateUnlisted() (map[string]bool, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT template_id FROM process_template_listing WHERE listed = 0`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetProcessTemplateListed records a template's library listing.
func SetProcessTemplateListed(templateID string, listed bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	value := 0
	if listed {
		value = 1
	}
	_, err = d.Exec(`INSERT INTO process_template_listing (template_id, listed, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(template_id) DO UPDATE SET listed = excluded.listed, updated_at = excluded.updated_at`,
		templateID, value, dbTime(time.Now().UTC()))
	return err
}

// PinProcessToGroup appends a template to a group's saved processes. Pinning
// an already pinned template keeps its place.
func PinProcessToGroup(groupID int64, templateID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists, count int
	var last sql.NullInt64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM process_group_pins WHERE group_id = ? AND template_id = ?`,
		groupID, templateID).Scan(&exists); err != nil {
		return err
	}
	if exists > 0 {
		return tx.Commit()
	}
	if err := tx.QueryRow(`SELECT COUNT(*), MAX(position) FROM process_group_pins WHERE group_id = ?`,
		groupID).Scan(&count, &last); err != nil {
		return err
	}
	if count >= MaxProcessGroupPins {
		return ErrProcessGroupPinsFull
	}
	if _, err := tx.Exec(`INSERT INTO process_group_pins (group_id, template_id, position, pinned_at) VALUES (?, ?, ?, ?)`,
		groupID, templateID, last.Int64+1, dbTime(time.Now().UTC())); err != nil {
		return err
	}
	return tx.Commit()
}

// ProcessGroupPinsFull reports whether pinning templateID to a group would
// exceed MaxProcessGroupPins: the group is full and does not already hold it.
func ProcessGroupPinsFull(groupID int64, templateID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var count, held int
	err = d.QueryRow(`SELECT COUNT(*), COALESCE(SUM(template_id = ?), 0) FROM process_group_pins WHERE group_id = ?`,
		templateID, groupID).Scan(&count, &held)
	return held == 0 && count >= MaxProcessGroupPins, err
}

// UnpinProcessFromGroup removes a template from a group's saved processes and
// reports whether it was pinned.
func UnpinProcessFromGroup(groupID int64, templateID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM process_group_pins WHERE group_id = ? AND template_id = ?`, groupID, templateID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListProcessGroupPins returns a group's pinned template ids in pin order.
func ListProcessGroupPins(groupID int64) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT template_id FROM process_group_pins WHERE group_id = ? ORDER BY position, template_id LIMIT ?`,
		groupID, MaxProcessGroupPins)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ForgetProcessTemplate removes a deleted template's listing and pins.
func ForgetProcessTemplate(templateID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	if _, err := d.Exec(`DELETE FROM process_group_pins WHERE template_id = ?`, templateID); err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM process_template_listing WHERE template_id = ?`, templateID)
	return err
}
