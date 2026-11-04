package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Group pages (tcl-1lth8e): the live pages shared in an agent group, saved so
// they belong to the group rather than to one daemon run. The live page
// itself (its stage origin and token) is still in agentd's memory; this row
// is what reopens it. State is "open" while a stage serves it and "closed"
// otherwise (closed from the dashboard or by its agent, its owner retired,
// or the daemon restarted). Deleting the row is a separate, explicit action.

const (
	GroupPageOpen   = "open"
	GroupPageClosed = "closed"

	// GroupPageClosedKept is how many closed pages a group keeps; the oldest
	// closed ones beyond it are dropped when another closes.
	GroupPageClosedKept = 64
)

type GroupPage struct {
	ID           string
	GroupID      int64
	Owner        string // agent ID, or "group:<id>" for a page the operator created
	Name         string
	Title        string
	Kind         string
	Target       string
	Path         string
	Worktree     string
	Port         int
	AgentsLocked bool
	State        string
	ClosedReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClosedAt     time.Time
}

const groupPageCols = `id, group_id, owner, name, title, kind, target, path, worktree, port,
	agents_locked, state, closed_reason, created_at, updated_at, closed_at`

func scanGroupPage(s rowScanner) (*GroupPage, error) {
	var p GroupPage
	var locked int
	var created, updated, closed dbTimestamp
	if err := s.Scan(&p.ID, &p.GroupID, &p.Owner, &p.Name, &p.Title, &p.Kind, &p.Target, &p.Path, &p.Worktree, &p.Port,
		&locked, &p.State, &p.ClosedReason, &created, &updated, &closed); err != nil {
		return nil, err
	}
	p.AgentsLocked = locked != 0
	p.CreatedAt, p.UpdatedAt, p.ClosedAt = created.Time(), updated.Time(), closed.Time()
	return &p, nil
}

// SaveOpenGroupPage records a page as open in its group, creating the row or
// updating it (a re-show, a move to another group).
func SaveOpenGroupPage(p GroupPage) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Another saved page under the same owner and name in that group (one an
	// adopting agent or a fresh show replaces) gives way to this one, unless
	// the operator locked it: then the upsert fails on the unique key and the
	// locked row stays (callers refuse a locked name first).
	if _, err := tx.Exec(`DELETE FROM group_pages WHERE group_id = ? AND owner = ? AND name = ? AND id <> ? AND agents_locked = 0`,
		p.GroupID, p.Owner, p.Name, p.ID); err != nil {
		return err
	}
	now := time.Now()
	_, err = tx.Exec(`INSERT INTO group_pages (`+groupPageCols+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', '', ?, ?, NULL)
		ON CONFLICT(id) DO UPDATE SET
			group_id = excluded.group_id, owner = excluded.owner, name = excluded.name,
			title = excluded.title, kind = excluded.kind, target = excluded.target, path = excluded.path,
			worktree = excluded.worktree, port = excluded.port, agents_locked = excluded.agents_locked,
			state = 'open', closed_reason = '', updated_at = excluded.updated_at, closed_at = NULL`,
		p.ID, p.GroupID, p.Owner, p.Name, p.Title, p.Kind, p.Target, p.Path, p.Worktree, p.Port,
		boolToInt(p.AgentsLocked), dbTime(now), dbTime(now))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// CloseGroupPage marks a saved page closed, with why, and drops the group's
// oldest closed pages beyond GroupPageClosedKept (a page the operator
// locked is never dropped). No row is not an error.
func CloseGroupPage(id, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	if _, err := tx.Exec(`UPDATE group_pages SET state = 'closed', closed_reason = ?, closed_at = ?, updated_at = ?
		WHERE id = ? AND state = 'open'`, strings.TrimSpace(reason), dbTime(now), dbTime(now), id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM group_pages WHERE id IN (
			SELECT id FROM group_pages WHERE state = 'closed' AND agents_locked = 0
			  AND group_id = (SELECT group_id FROM group_pages WHERE id = ?)
			ORDER BY closed_at DESC, id DESC LIMIT -1 OFFSET ?)`, id, GroupPageClosedKept); err != nil {
		return err
	}
	return tx.Commit()
}

// SetGroupPageLocked records the operator's agent lock on a saved page.
func SetGroupPageLocked(id string, locked bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE group_pages SET agents_locked = ? WHERE id = ?`, boolToInt(locked), id)
	return err
}

// DeleteGroupPage removes a saved page. It reports whether one existed.
func DeleteGroupPage(id string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM group_pages WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetGroupPage returns a saved page by id, nil when none.
func GetGroupPage(id string) (*GroupPage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanGroupPage(d.QueryRow(`SELECT `+groupPageCols+` FROM group_pages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// FindGroupPage returns the saved page an owner keeps under name in a group,
// nil when none: a re-show of that name reopens it under the same id.
func FindGroupPage(groupID int64, owner, name string) (*GroupPage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanGroupPage(d.QueryRow(`SELECT `+groupPageCols+` FROM group_pages
		WHERE group_id = ? AND owner = ? AND name = ?`, groupID, owner, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// ListGroupPages returns every saved page, by group then name. It is bounded
// by the per-group caps (open pages by the daemon's, closed ones by
// GroupPageClosedKept).
func ListGroupPages() ([]*GroupPage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ` + groupPageCols + ` FROM group_pages ORDER BY group_id, name, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*GroupPage
	for rows.Next() {
		p, err := scanGroupPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CloseOpenGroupPages marks every page still recorded open as closed, with
// why: at daemon start no stage serves any of them.
func CloseOpenGroupPages(reason string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	res, err := d.Exec(`UPDATE group_pages SET state = 'closed', closed_reason = ?, closed_at = ?, updated_at = ?
		WHERE state = 'open'`, reason, dbTime(now), dbTime(now))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteGroupPagesOwnedBy removes every saved page an agent showed: the agent
// is being deleted, so none of them can reopen.
func DeleteGroupPagesOwnedBy(owner string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM group_pages WHERE owner = ?`, owner)
	return err
}

// SetGroupPageTitle renames a saved page. No row is not an error.
func SetGroupPageTitle(id, title string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE group_pages SET title = ?, updated_at = ? WHERE id = ?`, title, dbTime(time.Now()), id)
	return err
}

// ErrGroupPageNameTaken is a move into a group that already keeps a page
// under that owner and name.
var ErrGroupPageNameTaken = errors.New("the group already has a page of that name")

// MoveGroupPage moves a saved page to another group, under owner there (an
// agent's page keeps its agent; a group's own page takes the new group's
// key). It reports whether the page existed.
func MoveGroupPage(id string, groupID int64, owner string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE group_pages SET group_id = ?, owner = ?, updated_at = ? WHERE id = ?`, groupID, owner, dbTime(time.Now()), id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return false, ErrGroupPageNameTaken
		}
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
