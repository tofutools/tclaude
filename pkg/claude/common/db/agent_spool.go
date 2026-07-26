package db

import (
	"errors"
	"strings"
	"time"
)

// SpoolBinding maps one file-spool directory to the conversation it was
// provisioned for. See migrateV159toV160 and pkg/claude/common/agentipc's
// spool documentation: the daemon derives spool-request caller identity
// from these rows, so they are written only by trusted spawn-side code,
// never from anything an agent controls.
type SpoolBinding struct {
	SpoolID   string
	ConvID    string
	Dir       string
	CreatedAt time.Time
}

// CreateSpoolBinding records a freshly provisioned spool directory for a
// conv. The spool id is the directory's basename and primary key; inserting
// an existing id fails, which is the desired behaviour for a random
// capability that must never be reused.
func CreateSpoolBinding(spoolID, convID, dir string) error {
	spoolID = strings.TrimSpace(spoolID)
	convID = strings.TrimSpace(convID)
	dir = strings.TrimSpace(dir)
	if spoolID == "" || convID == "" || dir == "" {
		return errors.New("CreateSpoolBinding: spool_id, conv_id and dir are all required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO agent_spool_bindings (spool_id, conv_id, dir, created_at)
		VALUES (?, ?, ?, ?)`,
		spoolID, convID, dir, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ListActiveSpoolBindings returns every non-revoked spool binding. The
// daemon's spool consumer loads this set at startup and on its periodic
// rescan to know which directories to serve.
func ListActiveSpoolBindings() ([]SpoolBinding, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT spool_id, conv_id, dir, created_at
		FROM agent_spool_bindings WHERE revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SpoolBinding
	for rows.Next() {
		var b SpoolBinding
		var createdAt string
		if err := rows.Scan(&b.SpoolID, &b.ConvID, &b.Dir, &createdAt); err != nil {
			return nil, err
		}
		b.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		out = append(out, b)
	}
	return out, rows.Err()
}

// RevokeSpoolBindingsForConv marks every binding for a conv revoked. The
// consumer stops serving a revoked directory on its next rescan; the
// directory itself is swept separately. Returns the number of bindings
// revoked.
func RevokeSpoolBindingsForConv(convID string) (int64, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return 0, errors.New("RevokeSpoolBindingsForConv: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_spool_bindings SET revoked_at = ?
		WHERE conv_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
