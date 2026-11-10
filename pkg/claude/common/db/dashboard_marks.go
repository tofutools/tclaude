package db

import (
	"database/sql"
	"errors"
	"time"
)

// A dashboard mark is something an agent drew on the operator's view. Its row
// is the mark: the dashboard draws the active rows, and an ended row stays as
// history, so an agent can see how things got here.

const (
	MarkStateActive       = "active"
	MarkStateExpired      = "expired"
	MarkStateDismissed    = "dismissed"
	MarkStateAcknowledged = "acknowledged"
	MarkStateCleared      = "cleared"
	MarkStateReplaced     = "replaced"

	// MarkEndedByOperator and MarkEndedBySystem fill ended_by; an agent that
	// ends a mark is recorded by its id.
	MarkEndedByOperator = "operator"
	MarkEndedBySystem   = "system"
)

var (
	ErrDashboardMarkLimit    = errors.New("too many active marks")
	ErrDashboardMarkNotFound = errors.New("no such mark")
	// ErrDashboardMarkEnded reports acting on a mark that is no longer active.
	ErrDashboardMarkEnded = errors.New("the mark has ended")
)

// DashboardMark is one mark. Spec is the drawing as JSON, opaque here.
type DashboardMark struct {
	ID        int64
	AgentID   string
	AgentName string
	Kind      string
	Spec      string
	Tip       string
	State     string
	EndedBy   string
	Reply     string
	RepliedAt time.Time
	CreatedAt time.Time
	ExpiresAt time.Time
	EndedAt   time.Time
}

const dashboardMarkColumns = `id, agent_id, agent_name, kind, spec, tip, state, ended_by, reply, replied_at, created_at, expires_at, ended_at`

func scanDashboardMark(row rowScanner) (*DashboardMark, error) {
	var m DashboardMark
	var replied, created, expires, ended dbTimestamp
	if err := row.Scan(&m.ID, &m.AgentID, &m.AgentName, &m.Kind, &m.Spec, &m.Tip, &m.State, &m.EndedBy, &m.Reply,
		&replied, &created, &expires, &ended); err != nil {
		return nil, err
	}
	m.RepliedAt, m.CreatedAt, m.ExpiresAt, m.EndedAt = replied.Time(), created.Time(), expires.Time(), ended.Time()
	return &m, nil
}

func expireDashboardMarksTx(tx *sql.Tx, now time.Time) error {
	_, err := tx.Exec(`UPDATE dashboard_marks SET state = ?, ended_by = ?, ended_at = expires_at
 WHERE state = ? AND expires_at <= ?`, MarkStateExpired, MarkEndedBySystem, MarkStateActive, dbTime(now))
	return err
}

// CreateDashboardMark stores a new active mark, ending the one it replaces in
// the same step so a replacement never trips the limit, and prunes ended marks
// past keepEnded, oldest first.
func CreateDashboardMark(m *DashboardMark, replaces int64, maxActive, keepEnded int, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := expireDashboardMarksTx(tx, now); err != nil {
		return err
	}
	if replaces != 0 {
		by := m.AgentID
		if by == "" {
			by = MarkEndedByOperator
		}
		if err := endDashboardMarkTx(tx, replaces, MarkStateReplaced, by, now); err != nil {
			return err
		}
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM dashboard_marks WHERE state = ?`, MarkStateActive).Scan(&active); err != nil {
		return err
	}
	if active >= maxActive {
		return ErrDashboardMarkLimit
	}
	res, err := tx.Exec(`INSERT INTO dashboard_marks (agent_id, agent_name, kind, spec, tip, state, created_at, expires_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.AgentID, m.AgentName, m.Kind, m.Spec, m.Tip, MarkStateActive, dbTime(now), dbTime(m.ExpiresAt))
	if err != nil {
		return err
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM dashboard_marks WHERE state != ? AND id NOT IN (
 SELECT id FROM dashboard_marks WHERE state != ? ORDER BY id DESC LIMIT ?)`, MarkStateActive, MarkStateActive, keepEnded); err != nil {
		return err
	}
	m.State, m.CreatedAt = MarkStateActive, now
	return tx.Commit()
}

func endDashboardMarkTx(tx *sql.Tx, id int64, state, by string, now time.Time) error {
	res, err := tx.Exec(`UPDATE dashboard_marks SET state = ?, ended_by = ?, ended_at = ? WHERE id = ? AND state = ?`,
		state, by, dbTime(now), id, MarkStateActive)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	var exists int
	if err := tx.QueryRow(`SELECT 1 FROM dashboard_marks WHERE id = ?`, id).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrDashboardMarkNotFound
		}
		return err
	}
	return ErrDashboardMarkEnded
}

// EndDashboardMark ends an active mark, recording how and who.
func EndDashboardMark(id int64, state, by string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := expireDashboardMarksTx(tx, now); err != nil {
		return err
	}
	if err := endDashboardMarkTx(tx, id, state, by, now); err != nil {
		return err
	}
	return tx.Commit()
}

// EndActiveDashboardMarks ends every active mark, or only one agent's when
// agentID is set, and returns how many ended.
func EndActiveDashboardMarks(agentID, state, by string, now time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := expireDashboardMarksTx(tx, now); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`UPDATE dashboard_marks SET state = ?, ended_by = ?, ended_at = ?
 WHERE state = ? AND (? = '' OR agent_id = ?)`, state, by, dbTime(now), MarkStateActive, agentID, agentID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// ActiveDashboardMark returns a mark that is still up, or says why it is not.
func ActiveDashboardMark(id int64, now time.Time) (*DashboardMark, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	m, err := scanDashboardMark(d.QueryRow(`SELECT `+dashboardMarkColumns+` FROM dashboard_marks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDashboardMarkNotFound
	}
	if err != nil {
		return nil, err
	}
	if m.State != MarkStateActive || !m.ExpiresAt.After(now) {
		return nil, ErrDashboardMarkEnded
	}
	return m, nil
}

// RecordDashboardMarkReply keeps the operator's answer on a mark. The mark stays
// up: the agent decides what happens next.
func RecordDashboardMarkReply(id int64, reply string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE dashboard_marks SET reply = ?, replied_at = ? WHERE id = ?`, reply, dbTime(now), id)
	return err
}

// ListDashboardMarks returns marks newest first, optionally of one state,
// starting below the id before (0 for the newest). Marks past their time read
// as expired whether or not anything has looked at them yet.
func ListDashboardMarks(state string, before int64, limit int, now time.Time) ([]*DashboardMark, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := expireDashboardMarksTx(tx, now); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT `+dashboardMarkColumns+` FROM dashboard_marks
 WHERE (? = '' OR state = ?) AND (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`, state, state, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*DashboardMark{}
	for rows.Next() {
		m, err := scanDashboardMark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	return out, tx.Commit()
}

// ActiveDashboardMarks returns the marks up now, oldest first. It only reads:
// every open tab asks several times a minute.
func ActiveDashboardMarks(now time.Time) ([]*DashboardMark, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+dashboardMarkColumns+` FROM dashboard_marks
 WHERE state = ? AND expires_at > ? ORDER BY id`, MarkStateActive, dbTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*DashboardMark{}
	for rows.Next() {
		m, err := scanDashboardMark(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DashboardCapture is a kept capture. Its image is a file the daemon owns.
type DashboardCapture struct {
	ID        int64
	AgentID   string
	AgentName string
	Label     string
	Context   string
	Size      int64
	CreatedAt time.Time
}

const dashboardCaptureColumns = `id, agent_id, agent_name, label, context, size, created_at`

func scanDashboardCapture(row rowScanner) (*DashboardCapture, error) {
	var c DashboardCapture
	var created dbTimestamp
	if err := row.Scan(&c.ID, &c.AgentID, &c.AgentName, &c.Label, &c.Context, &c.Size, &created); err != nil {
		return nil, err
	}
	c.CreatedAt = created.Time()
	return &c, nil
}

// SaveDashboardCapture records a kept capture.
func SaveDashboardCapture(c *DashboardCapture, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`INSERT INTO dashboard_captures (agent_id, agent_name, label, context, size, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.AgentID, c.AgentName, c.Label, c.Context, c.Size, dbTime(now))
	if err != nil {
		return err
	}
	c.CreatedAt = now
	c.ID, err = res.LastInsertId()
	return err
}

// DeleteDashboardCapture removes a kept capture's row.
func DeleteDashboardCapture(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM dashboard_captures WHERE id = ?`, id)
	return err
}

// GetDashboardCapture returns one kept capture, or nil.
func GetDashboardCapture(id int64) (*DashboardCapture, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	c, err := scanDashboardCapture(d.QueryRow(`SELECT `+dashboardCaptureColumns+` FROM dashboard_captures WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

// ListDashboardCaptures returns kept captures newest first, below the id before
// (0 for the newest).
func ListDashboardCaptures(before int64, limit int) ([]*DashboardCapture, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+dashboardCaptureColumns+` FROM dashboard_captures
 WHERE (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?`, before, before, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*DashboardCapture{}
	for rows.Next() {
		c, err := scanDashboardCapture(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneDashboardCaptures deletes the oldest kept captures until at most
// maxCount remain and their images total at most maxBytes, and returns the ids
// removed so the caller can delete their files. The newest always stays.
func PruneDashboardCaptures(maxCount int, maxBytes int64) ([]int64, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT id, size FROM dashboard_captures ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var stale []int64
	var total int64
	kept := 0
	for rows.Next() {
		var id, size int64
		if err := rows.Scan(&id, &size); err != nil {
			return nil, err
		}
		total += size
		if kept > 0 && (kept >= maxCount || total > maxBytes) {
			stale = append(stale, id)
			continue
		}
		kept++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	for _, id := range stale {
		if _, err := tx.Exec(`DELETE FROM dashboard_captures WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	return stale, tx.Commit()
}
