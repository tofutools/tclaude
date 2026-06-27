package db

import (
	"database/sql"
	"errors"
	"time"
)

// ErrCloneRateLimited is the sentinel returned by ClaimCloneSlot when
// a clone of the same source conv occurred within the cooldown
// window. Callers translate this into a 429 at the HTTP layer.
var ErrCloneRateLimited = errors.New("clone rate limited")

// ClaimCloneSlot atomically reserves a "clone slot" for sourceConvID
// at the given instant. Returns ErrCloneRateLimited if a previous
// clone of the same source landed inside cooldown; nil on success
// (caller may proceed and is expected to actually carry out the clone).
//
// Implementation: a single SQL statement does the check + insert
// atomically, so two concurrent claim attempts can't both pass — the
// second one's INSERT...SELECT yields zero affected rows because the
// first one's row already satisfies the WHERE EXISTS clause.
//
// Why this table over a column on the source row: source convs may
// not have any per-source DB row of their own (they could be conv-
// indexed only, or live solely in agent_group_members). A dedicated
// history table keys exclusively on conv-id and avoids cross-table
// migrations when new source kinds emerge.
//
// Recording rule: we record the attempt up-front (as soon as the
// rate-limit check passes), not on successful completion. This means
// a failed clone (spawn timeout, copy failure, etc.) still consumes a
// slot — intentional, so a tight retry loop can't drain the rate-
// limit by burning failed attempts.
//
// now is supplied by the caller (rather than read from time.Now) so
// tests can advance a controlled clock. Production passes
// time.Now().UTC().
func ClaimCloneSlot(sourceConvID string, cooldown time.Duration, now time.Time) error {
	if sourceConvID == "" {
		return errors.New("ClaimCloneSlot: sourceConvID required")
	}
	if cooldown < 0 {
		cooldown = 0
	}
	d, err := Open()
	if err != nil {
		return err
	}
	// Key the cooldown on the source's stable actor (JOH-26 PR3a), so a
	// reincarnate / Claude Code /clear that rotates the source conv-id can't
	// bypass the per-source cooldown — it follows the actor across generations.
	// The clone source is an existing agent; EnsureAgentForConv resolves it,
	// allocating only in the pathological not-yet-enrolled case.
	sourceAgentID, _, err := EnsureAgentForConv(sourceConvID, "clone")
	if err != nil {
		return err
	}
	threshold := now.Add(-cooldown).Format(time.RFC3339Nano)
	nowStr := now.Format(time.RFC3339Nano)

	// INSERT only if no prior row for this source within cooldown.
	// SQLite executes INSERT ... SELECT ... WHERE NOT EXISTS as a
	// single statement under the database write lock (WAL mode), so
	// the read + write are atomic with respect to other writers.
	res, err := d.Exec(`
		INSERT INTO agent_clone_history (source_agent_id, cloned_at)
		SELECT ?, ?
		WHERE NOT EXISTS (
			SELECT 1 FROM agent_clone_history
			WHERE source_agent_id = ? AND cloned_at > ?
		)`,
		sourceAgentID, nowStr, sourceAgentID, threshold)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCloneRateLimited
	}
	return nil
}

// LatestCloneAt returns the timestamp of the most recent clone of
// sourceConvID, or zero time if none. Currently only used by tests;
// production callers go through ClaimCloneSlot.
func LatestCloneAt(sourceConvID string) (time.Time, error) {
	d, err := Open()
	if err != nil {
		return time.Time{}, err
	}
	// History is keyed on the source's actor (JOH-26 PR3a); resolve the conv.
	// An unmapped conv (never cloned, so never enrolled here) has no rows.
	sourceAgentID, err := AgentIDForConv(sourceConvID)
	if err != nil {
		return time.Time{}, err
	}
	if sourceAgentID == "" {
		return time.Time{}, nil
	}
	var s string
	err = d.QueryRow(`
		SELECT cloned_at FROM agent_clone_history
		WHERE source_agent_id = ?
		ORDER BY cloned_at DESC
		LIMIT 1`, sourceAgentID).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, s)
}
