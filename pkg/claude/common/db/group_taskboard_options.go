package db

import (
	"database/sql"
	"errors"
	"time"
)

// A task board's own options (migrateV333toV334, docs/taskboard.md "Board
// options"). The store keeps the document as text; pkg/claude/common/boardopts
// reads, checks and resolves it.

// GroupTaskboardOptions is a board's stored options. A board that never set
// any reads as Options "" at Version 0.
type GroupTaskboardOptions struct {
	GroupID   int64
	Options   string
	Version   int64
	UpdatedBy string
	UpdatedAt time.Time
}

// AnyTaskboardOptionsVersion lets UpdateGroupTaskboardOptions write over
// whatever version is stored.
const AnyTaskboardOptionsVersion = -1

// ErrTaskboardOptionsStale is a write that expected a version other than
// the stored one: someone changed the options since the writer read them.
var ErrTaskboardOptionsStale = errors.New("the board options changed since they were read")

func getGroupTaskboardOptionsTx(q taskTx, groupID int64) (GroupTaskboardOptions, error) {
	o := GroupTaskboardOptions{GroupID: groupID}
	var at dbTimestamp
	err := q.QueryRow(`SELECT options, version, updated_by, updated_at FROM group_taskboard_options WHERE group_id = ?`, groupID).
		Scan(&o.Options, &o.Version, &o.UpdatedBy, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return o, nil
	}
	o.UpdatedAt = at.Time()
	return o, err
}

// GetGroupTaskboardOptions reads a group's board options.
func GetGroupTaskboardOptions(groupID int64) (GroupTaskboardOptions, error) {
	d, err := Open()
	if err != nil {
		return GroupTaskboardOptions{}, err
	}
	return getGroupTaskboardOptionsTx(d, groupID)
}

// UpdateGroupTaskboardOptions changes a group's board options in one
// transaction: fn gets the stored document and returns the new one. expect
// is the version the writer read (AnyTaskboardOptionsVersion for none); a
// different stored version is ErrTaskboardOptionsStale and nothing is
// written. A document fn returns unchanged is not written and keeps its
// version.
func UpdateGroupTaskboardOptions(groupID int64, expect int64, actor string, fn func(current string) (string, error)) (GroupTaskboardOptions, error) {
	d, err := Open()
	if err != nil {
		return GroupTaskboardOptions{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return GroupTaskboardOptions{}, err
	}
	defer func() { _ = tx.Rollback() }()
	cur, err := getGroupTaskboardOptionsTx(tx, groupID)
	if err != nil {
		return GroupTaskboardOptions{}, err
	}
	if expect != AnyTaskboardOptionsVersion && expect != cur.Version {
		return cur, ErrTaskboardOptionsStale
	}
	next, err := fn(cur.Options)
	if err != nil {
		return cur, err
	}
	if next == cur.Options || (cur.Version == 0 && next == "{}") {
		return cur, nil
	}
	now := time.Now()
	if _, err := tx.Exec(`INSERT INTO group_taskboard_options (group_id, options, version, updated_by, updated_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET options = excluded.options, version = version + 1,
			updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		groupID, next, actor, dbTime(now)); err != nil {
		return cur, err
	}
	out, err := getGroupTaskboardOptionsTx(tx, groupID)
	if err != nil {
		return cur, err
	}
	return out, tx.Commit()
}
