package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ProcessPhase is one phase of a template's declarative process spec
// (JOH-242): an ordered chapter of the group's work. Name is the phase's
// handle (unique within the process, case-insensitively); Roles are the role
// labels active in the phase (matched case-insensitively against a member's
// role, the same rule work-pattern --role routing uses; the literal "all"
// means every member is active); Criteria is free PROSE — entry / exit /
// handoff described in words, deliberately NOT a DSL or a condition engine.
//
// The whole feature is ADVISORY: the runtime records and surfaces the phase a
// group is in and nudges the entering roles on advance, but enforces nothing.
type ProcessPhase struct {
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	Criteria string   `json:"criteria,omitempty"`
}

// processToJSON marshals a process spec for a TEXT column (group_templates.
// process / group_process_state.process). An empty process stores as "[]" (the
// permsToJSON / workPatternToJSON convention) so a reader can json.Unmarshal it
// unconditionally; legacy template rows hold '' and read back as empty.
func processToJSON(phases []ProcessPhase) string {
	if len(phases) == 0 {
		return "[]"
	}
	b, err := json.Marshal(phases)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// processFromJSON parses a process TEXT column back into a slice. A blank
// ('' — pre-v92 template rows) or malformed value yields an empty (non-nil)
// slice, with each phase's Roles non-nil so callers can range safely.
func processFromJSON(s string) []ProcessPhase {
	out := []ProcessPhase{}
	if s == "" {
		return out
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return []ProcessPhase{}
	}
	for i := range out {
		if out[i].Roles == nil {
			out[i].Roles = []string{}
		}
	}
	return out
}

// GroupProcessState is the advisory runtime state of one group's process
// (JOH-242): the phase list snapshotted from the template at instantiate (so
// the runtime is self-contained — a later template edit / delete never
// disturbs a live group), the current phase's name, and when it was entered.
// A group with no process has no row at all (absence = feature off).
type GroupProcessState struct {
	GroupID        int64
	Process        []ProcessPhase
	CurrentPhase   string
	PhaseStartedAt time.Time
}

// PhaseIndex returns the zero-based index of CurrentPhase within Process, or
// -1 if the current phase name isn't found (a snapshot/current-phase drift
// that shouldn't happen but is surfaced honestly rather than guessed).
func (s *GroupProcessState) PhaseIndex() int {
	for i, p := range s.Process {
		if p.Name == s.CurrentPhase {
			return i
		}
	}
	return -1
}

// GroupProcessTransition is one row in the append-only phase-change log: the
// group, the phase moved from (""" for the initial entry), the phase moved to,
// when, and the acting identity label.
type GroupProcessTransition struct {
	ID        int64
	GroupID   int64
	FromPhase string
	ToPhase   string
	At        time.Time
	Actor     string
}

// InitGroupProcess initializes a group's advisory process state at instantiate
// (JOH-242): it snapshots the ordered phase list, sets the current phase to the
// FIRST phase, and records an initial transition from "" → first-phase with the
// acting identity. A no-op (returns nil) when phases is empty — a group with no
// process simply has no state. Uses INSERT OR REPLACE so a re-instantiate of a
// group name (only possible after a delete) starts clean.
func InitGroupProcess(groupID int64, phases []ProcessPhase, actor string) error {
	if len(phases) == 0 {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	first := phases[0].Name
	now := time.Now()
	nowStr := now.Format(time.RFC3339Nano)
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO group_process_state (group_id, process, current_phase, phase_started_at)
		 VALUES (?, ?, ?, ?)`,
		groupID, processToJSON(phases), first, nowStr); err != nil {
		return err
	}
	// Re-init safe: clear any prior transition log for this group before
	// seeding the initial "" → first-phase entry, so a re-init (matching the
	// INSERT OR REPLACE on the state row above) never leaves two initial
	// transitions behind. In production this is a fresh group so there is
	// nothing to clear.
	if _, err := tx.Exec(`DELETE FROM group_process_transitions WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO group_process_transitions (group_id, from_phase, to_phase, at, actor)
		 VALUES (?, '', ?, ?, ?)`,
		groupID, first, nowStr, actor); err != nil {
		return err
	}
	return tx.Commit()
}

// GetGroupProcessState returns a group's advisory process state, or (nil, nil)
// when the group has no process.
func GetGroupProcessState(groupID int64) (*GroupProcessState, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var st GroupProcessState
	var proc, started string
	err = d.QueryRow(
		`SELECT group_id, process, current_phase, phase_started_at
		 FROM group_process_state WHERE group_id = ?`, groupID).
		Scan(&st.GroupID, &proc, &st.CurrentPhase, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.Process = processFromJSON(proc)
	st.PhaseStartedAt = parseTimeOrZero(started)
	return &st, nil
}

// AdvanceGroupProcess moves a group's process to toPhase and appends a
// transition (from the prior current phase → toPhase) with the acting
// identity. The caller validates toPhase is a real phase in the snapshot; this
// records the move verbatim (still advisory). It returns the phase actually
// moved FROM — read inside the same transaction, so the caller reports the true
// recorded transition even if another advance interleaved between its own
// pre-read and this write. Returns sql.ErrNoRows when the group has no process
// state.
func AdvanceGroupProcess(groupID int64, toPhase, actor string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var from string
	err = tx.QueryRow(`SELECT current_phase FROM group_process_state WHERE group_id = ?`, groupID).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", err
	}
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`UPDATE group_process_state SET current_phase = ?, phase_started_at = ? WHERE group_id = ?`,
		toPhase, now, groupID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(
		`INSERT INTO group_process_transitions (group_id, from_phase, to_phase, at, actor)
		 VALUES (?, ?, ?, ?, ?)`,
		groupID, from, toPhase, now, actor); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return from, nil
}

// ListGroupProcessTransitions returns a group's phase-change log oldest-first
// (ORDER BY id — never by the RFC3339Nano `at` string, which misorders rows
// inside the same whole second). Returns an empty (non-nil) slice when there
// are none.
func ListGroupProcessTransitions(groupID int64) ([]GroupProcessTransition, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT id, group_id, from_phase, to_phase, at, actor
		 FROM group_process_transitions WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []GroupProcessTransition{}
	for rows.Next() {
		var tr GroupProcessTransition
		var at string
		if err := rows.Scan(&tr.ID, &tr.GroupID, &tr.FromPhase, &tr.ToPhase, &at, &tr.Actor); err != nil {
			return nil, err
		}
		tr.At = parseTimeOrZero(at)
		out = append(out, tr)
	}
	return out, rows.Err()
}
