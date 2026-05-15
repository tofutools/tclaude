package db

import (
	"database/sql"
	"fmt"
	"time"
)

// SessionRow represents a session row in the database.
type SessionRow struct {
	ID             string
	TmuxSession    string
	PID            int
	Cwd            string
	ConvID         string
	Status         string
	StatusDetail   string
	SubagentCount  int
	AutoRegistered bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastHook       time.Time
}

// SaveSession inserts or replaces a session, setting updated_at to now.
func SaveSession(s *SessionRow) error {
	db, err := Open()
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	_, err = db.Exec(`INSERT OR REPLACE INTO sessions
		(id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, auto_registered, created_at, updated_at, last_hook)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.TmuxSession, s.PID, s.Cwd, s.ConvID,
		s.Status, s.StatusDetail, s.SubagentCount, boolToInt(s.AutoRegistered),
		s.CreatedAt.Format(time.RFC3339Nano), s.UpdatedAt.Format(time.RFC3339Nano), s.LastHook.Format(time.RFC3339Nano))
	return err
}

// LoadSession loads a session by primary key.
func LoadSession(id string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// DeleteSession removes a session by ID.
func DeleteSession(id string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// ListSessions returns all sessions.
func ListSessions() ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook FROM sessions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSessions(rows)
}

// FindSessionByConvID finds a session by conversation ID using the index.
// When multiple rows exist for the same conv_id (e.g. auto-register
// created a new row alongside an old one with a different short id), we
// return the most recently updated one.
func FindSessionByConvID(convID string) (*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook FROM sessions WHERE conv_id = ?
		ORDER BY updated_at DESC LIMIT 1`, convID)
	s, err := scanSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// FindSessionsByConvID returns every row for the given conv_id, most
// recently updated first. Used by the agent daemon to find a row whose
// tmux session is actually alive when several stale rows coexist.
func FindSessionsByConvID(convID string) ([]*SessionRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count,
		auto_registered, created_at, updated_at, last_hook FROM sessions WHERE conv_id = ?
		ORDER BY updated_at DESC`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanSessions(rows)
}

// SessionExists checks whether a session with the given ID exists.
func SessionExists(id string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, id).Scan(&count)
	return count > 0, err
}

// CleanupOldExited deletes exited sessions older than maxAge and returns the count deleted.
func CleanupOldExited(maxAge time.Duration) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-maxAge).Format(time.RFC3339Nano)
	result, err := db.Exec(`DELETE FROM sessions WHERE status = 'exited' AND updated_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MaxUpdatedAt returns the most recent updated_at across all sessions.
// Returns zero time if no sessions exist.
func MaxUpdatedAt() (time.Time, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	var s sql.NullString
	err = db.QueryRow(`SELECT MAX(updated_at) FROM sessions`).Scan(&s)
	if err != nil || !s.Valid {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s.String)
}

// scanSession scans a single session row.
func scanSession(row *sql.Row) (*SessionRow, error) {
	var s SessionRow
	var autoReg int
	var createdStr, updatedStr, lastHookStr string
	err := row.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
		&s.Status, &s.StatusDetail, &s.SubagentCount, &autoReg, &createdStr, &updatedStr, &lastHookStr)
	if err != nil {
		return nil, err
	}
	s.AutoRegistered = autoReg != 0
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
	s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	if lastHookStr != "" {
		s.LastHook, _ = time.Parse(time.RFC3339Nano, lastHookStr)
	}
	return &s, nil
}

// scanSessions scans multiple session rows.
func scanSessions(rows *sql.Rows) ([]*SessionRow, error) {
	var result []*SessionRow
	for rows.Next() {
		var s SessionRow
		var autoReg int
		var createdStr, updatedStr, lastHookStr string
		err := rows.Scan(&s.ID, &s.TmuxSession, &s.PID, &s.Cwd, &s.ConvID,
			&s.Status, &s.StatusDetail, &s.SubagentCount, &autoReg, &createdStr, &updatedStr, &lastHookStr)
		if err != nil {
			return nil, fmt.Errorf("scanning session row: %w", err)
		}
		s.AutoRegistered = autoReg != 0
		s.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdStr)
		s.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
		if lastHookStr != "" {
			s.LastHook, _ = time.Parse(time.RFC3339Nano, lastHookStr)
		}
		result = append(result, &s)
	}
	return result, rows.Err()
}

// UpdateSessionLastHook writes only the last_hook column for a session,
// leaving updated_at unchanged so watch-mode polling is not perturbed.
func UpdateSessionLastHook(id string, t time.Time) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET last_hook = ? WHERE id = ?`, t.Format(time.RFC3339Nano), id)
	return err
}

// MarkSessionExitedIfUnchanged sets a session's status to "exited" —
// but only if the row still carries the status and updated_at the
// caller observed. It is a compare-and-swap: when the row changed
// underneath the caller (most often a resume's SessionStart hook
// flipping status back and bumping updated_at) the WHERE clause fails,
// nothing is written, and `false` is returned.
//
// The session reaper uses this so a session that resumed in the gap
// between "observed dead" and "write exited" is never clobbered. A
// false return is benign — the reaper re-evaluates the row next sweep.
func MarkSessionExitedIfUnchanged(id, observedStatus string, observedUpdatedAt time.Time) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE sessions
		SET status = 'exited', status_detail = '', updated_at = ?
		WHERE id = ? AND status = ? AND updated_at = ?`,
		time.Now().Format(time.RFC3339Nano),
		id, observedStatus, observedUpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpdateContextPct stores the latest context window usage percentage for a session.
func UpdateContextPct(sessionID string, pct float64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET context_pct = ? WHERE id = ?`, pct, sessionID)
	return err
}

// UpdateContextSnapshot stores the full last-API-response context-window
// snapshot from Claude Code's statusline. Tokens come from the most
// recent API response (input includes cache reads/writes), windowSize
// is the model's actual context limit (200000 or 1000000) — no
// reverse-engineering or per-model lookup needed once this is populated.
//
// All four fields are written together so a partial update can never
// leave the row in a state where pct disagrees with abs counts.
func UpdateContextSnapshot(sessionID string, pct float64, tokensInput, tokensOutput, windowSize int64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions
		SET context_pct = ?, tokens_input = ?, tokens_output = ?, context_window_size = ?
		WHERE id = ?`, pct, tokensInput, tokensOutput, windowSize, sessionID)
	return err
}

// ContextSnapshot is the full context-window state for a session.
// Zero values mean "not populated yet" — caller should fall back to
// the percentage-only display.
type ContextSnapshot struct {
	ContextPct        float64
	TokensInput       int64
	TokensOutput      int64
	ContextWindowSize int64
	CompactPending    float64
}

// GetContextSnapshot reads the full context-window state for a
// session. Returns zero values when the row isn't found.
func GetContextSnapshot(sessionID string) (ContextSnapshot, error) {
	db, err := Open()
	if err != nil {
		return ContextSnapshot{}, err
	}
	var s ContextSnapshot
	err = db.QueryRow(
		`SELECT context_pct, tokens_input, tokens_output, context_window_size, compact_pending
		 FROM sessions WHERE id = ?`, sessionID).
		Scan(&s.ContextPct, &s.TokensInput, &s.TokensOutput, &s.ContextWindowSize, &s.CompactPending)
	return s, err
}

// TryClaimCompact atomically sets compact_pending to the current unix timestamp
// if it is currently 0. Returns true if the claim was made (caller should send /compact).
func TryClaimCompact(sessionID string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	now := float64(time.Now().Unix())
	result, err := db.Exec(
		`UPDATE sessions SET compact_pending = ? WHERE id = ? AND compact_pending = 0`,
		now, sessionID)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// ResetCompact clears compact_pending and zeroes context_pct for a session.
// Also zeroes nudged_pct so a compacted session can be re-nudged from
// scratch as its context climbs again.
func ResetCompact(sessionID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions
		SET compact_pending = 0, context_pct = 0, nudged_pct = 0
		WHERE id = ?`, sessionID)
	return err
}

// GetCompactState returns the context_pct and compact_pending values for a session.
func GetCompactState(sessionID string) (contextPct float64, compactPending float64, err error) {
	db, err := Open()
	if err != nil {
		return 0, 0, err
	}
	err = db.QueryRow(`SELECT context_pct, compact_pending FROM sessions WHERE id = ?`, sessionID).
		Scan(&contextPct, &compactPending)
	return
}

// GetNudgedPct returns the highest threshold the context-nudge path
// has already fired for this session. 0 when the session has never
// been nudged or has been freshly compacted.
func GetNudgedPct(sessionID string) (float64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	var pct float64
	err = db.QueryRow(`SELECT nudged_pct FROM sessions WHERE id = ?`, sessionID).Scan(&pct)
	return pct, err
}

// SetNudgedPct stamps the highest-threshold-already-fired value
// after a successful nudge. Subsequent ticks at the same threshold
// no-op; the next climb beyond this value re-arms the nudge.
func SetNudgedPct(sessionID string, pct float64) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE sessions SET nudged_pct = ? WHERE id = ?`, pct, sessionID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
