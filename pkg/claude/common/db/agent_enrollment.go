package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Enrollment states. A conv is an "agent" exactly when it has an
// agent_enrollment row whose retired_at is empty.
const (
	EnrollmentNone    = "none"    // no row — a plain conversation
	EnrollmentActive  = "active"  // row, not retired — a live agent
	EnrollmentRetired = "retired" // row, retired_at set — demoted
)

// AgentEnrollment is a row in agent_enrollment — the explicit record
// that a conversation is (or was) an agent. See migrateV29toV30 for
// the rationale: agent-ness used to be a read-time heuristic, which
// made offline ungrouped agents invisible and left no way to demote
// an agent without deleting its conversation.
type AgentEnrollment struct {
	ConvID       string
	EnrolledAt   time.Time
	EnrolledVia  string    // spawn | clone | reincarnate | group | grant | cli | promote | migration
	RetiredAt    time.Time // zero ⇒ active
	RetiredBy    string
	RetireReason string
	// PendingName is the agent's intended display name, recorded at
	// spawn time from `tclaude agent spawn --name`. It is a fallback the
	// title-resolution path (agent.FreshTitle) uses to show a meaningful
	// name on the dashboard before the agent's own /rename has landed —
	// instead of "(unknown)". Once a real custom title exists it is
	// never consulted again. Empty for agents not spawned with a name.
	PendingName string
}

// Active reports whether the enrollment is a live agent (a row that
// has not been retired). A nil receiver is not active.
func (e *AgentEnrollment) Active() bool {
	return e != nil && e.RetiredAt.IsZero()
}

// EnrollAgent records convID as an agent if it is not already known.
// This is the automatic path — spawn, group-add, permission grant, a
// /v1 call from the conv itself — so it is INSERT OR IGNORE: it never
// touches an existing row, and in particular never un-retires one. A
// retired conv stays retired until something explicitly reinstates it.
// `via` is a short audit tag for how the enrollment happened.
func EnrollAgent(convID, via string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return errors.New("EnrollAgent: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT OR IGNORE INTO agent_enrollment
		(conv_id, enrolled_at, enrolled_via) VALUES (?, ?, ?)`,
		convID, time.Now().Format(time.RFC3339Nano), via)
	return err
}

// SetEnrollmentPendingName records convID's intended display name — the
// `tclaude agent spawn --name` value. The caller must have enrolled the
// conv first (spawn does, via AddAgentGroupMember → EnrollAgent); this
// is a plain UPDATE, a no-op if the row is absent. The pending name is a
// display fallback only: agent.FreshTitle returns it until a real custom
// title exists, then never again — so it is never cleared, just outvoted.
func SetEnrollmentPendingName(convID, name string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return errors.New("SetEnrollmentPendingName: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE agent_enrollment SET pending_name = ? WHERE conv_id = ?`,
		strings.TrimSpace(name), convID)
	return err
}

// PromoteAgent makes convID an active agent — the explicit, deliberate
// path behind the dashboard "promote" / "reinstate" buttons and the
// `tclaude agent promote` CLI. Unlike EnrollAgent it always lands the
// conv in the active state: a brand-new conv gets a row, and a retired
// conv has its retire fields cleared. Returns the prior state so the
// caller can tell a promote ("none") from a reinstate ("retired") and
// report a no-op ("active") honestly.
func PromoteAgent(convID, via string) (prior string, err error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", errors.New("PromoteAgent: conv_id required")
	}
	prior, err = EnrollmentState(convID)
	if err != nil {
		return "", err
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	switch prior {
	case EnrollmentNone:
		_, err = d.Exec(`INSERT INTO agent_enrollment
			(conv_id, enrolled_at, enrolled_via) VALUES (?, ?, ?)`,
			convID, time.Now().Format(time.RFC3339Nano), via)
	case EnrollmentRetired:
		_, err = d.Exec(`UPDATE agent_enrollment
			SET retired_at = '', retired_by = '', retire_reason = ''
			WHERE conv_id = ?`, convID)
	}
	return prior, err
}

// RetireAgent demotes an active agent to a plain conversation: it sets
// retired_at so the conv drops off every agent surface. The
// conversation data itself is untouched — this is the non-destructive
// half of cleanup. Callers must first revoke the conv's group
// memberships and permission grants; RetireAgent only flips the bit.
// Returns false (no error) when convID was not an active agent, so a
// repeated cleanup is idempotent.
func RetireAgent(convID, by, reason string) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, errors.New("RetireAgent: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_enrollment
		SET retired_at = ?, retired_by = ?, retire_reason = ?
		WHERE conv_id = ? AND retired_at = ''`,
		time.Now().Format(time.RFC3339Nano), by, reason, convID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReinstateAgent clears the retired flag, returning a retired conv to
// active-agent status. Its groups and grants do not come back — retire
// stripped those — so a reinstated agent starts fresh. Returns false
// (no error) when convID was not retired.
func ReinstateAgent(convID string) (bool, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return false, errors.New("ReinstateAgent: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE agent_enrollment
		SET retired_at = '', retired_by = '', retire_reason = ''
		WHERE conv_id = ? AND retired_at != ''`, convID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// EnrollmentState returns EnrollmentNone, EnrollmentActive or
// EnrollmentRetired for convID — the cheap probe the read paths use to
// decide whether a conv belongs on the agent roster.
func EnrollmentState(convID string) (string, error) {
	e, err := GetEnrollment(convID)
	if err != nil {
		return "", err
	}
	switch {
	case e == nil:
		return EnrollmentNone, nil
	case e.Active():
		return EnrollmentActive, nil
	default:
		return EnrollmentRetired, nil
	}
}

// GetEnrollment returns convID's enrollment row, or nil when the conv
// has never been an agent.
func GetEnrollment(convID string) (*AgentEnrollment, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT conv_id, enrolled_at, enrolled_via,
		retired_at, retired_by, retire_reason, pending_name
		FROM agent_enrollment WHERE conv_id = ?`, convID)
	e, err := scanEnrollment(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// ListActiveAgents returns every active (non-retired) enrollment —
// the canonical agent roster the dashboard and `tclaude agent ls`
// render, online and offline alike.
func ListActiveAgents() ([]*AgentEnrollment, error) {
	return listEnrollments(`retired_at = ''`)
}

// ListRetiredAgents returns every retired enrollment — the demoted
// agents the dashboard offers a "reinstate" button for.
func ListRetiredAgents() ([]*AgentEnrollment, error) {
	return listEnrollments(`retired_at != ''`)
}

// PendingNamesByConv returns conv_id → pending_name for every enrollment
// that recorded a non-empty spawn-time name. It is the bulk counterpart to
// GetEnrollment(...).PendingName, for listing surfaces that need the
// designated agent name as a display fallback (e.g. a Codex agent whose title
// write hasn't landed) without a per-row query. Conversations with no
// enrollment, or no pending name, are simply absent from the map.
func PendingNamesByConv() (map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, pending_name FROM agent_enrollment
		WHERE pending_name IS NOT NULL AND pending_name != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var convID, name string
		if err := rows.Scan(&convID, &name); err != nil {
			return nil, err
		}
		out[convID] = name
	}
	return out, rows.Err()
}

func listEnrollments(where string) ([]*AgentEnrollment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, enrolled_at, enrolled_via,
		retired_at, retired_by, retire_reason, pending_name
		FROM agent_enrollment WHERE ` + where + ` ORDER BY enrolled_at`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentEnrollment
	for rows.Next() {
		e, serr := scanEnrollment(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEnrollment removes the enrollment row entirely. Used by the
// conv-deletion cascade — once the conversation is gone there is no
// agent to enroll. Distinct from RetireAgent, which keeps the row.
func DeleteEnrollment(convID string) (int64, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM agent_enrollment WHERE conv_id = ?`, convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func scanEnrollment(s rowScanner) (*AgentEnrollment, error) {
	var e AgentEnrollment
	var enrolledAt, retiredAt string
	if err := s.Scan(&e.ConvID, &enrolledAt, &e.EnrolledVia,
		&retiredAt, &e.RetiredBy, &e.RetireReason, &e.PendingName); err != nil {
		return nil, err
	}
	e.EnrolledAt = parseTimeOrZero(enrolledAt)
	e.RetiredAt = parseTimeOrZero(retiredAt)
	return &e, nil
}
