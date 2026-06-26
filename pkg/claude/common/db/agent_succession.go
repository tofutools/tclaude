package db

import (
	"database/sql"
	"errors"
	"time"
)

// AgentConvSuccession is one row in agent_conv_succession — old → new
// pointer captured when a conv is replaced (reincarnate today). See
// migrateV14toV15 for the full design.
type AgentConvSuccession struct {
	OldConvID   string
	NewConvID   string
	Reason      string
	SucceededAt time.Time
}

// RecordConvSuccession inserts (oldConv → newConv, reason). Idempotent
// at the row level: a re-insert for the same oldConv replaces the
// pointer + bumps succeeded_at. (Mostly useful in tests; in practice
// reincarnate is the only writer and never replaces an old conv twice.)
//
// reason is a short tag — `reincarnate`, `clone-replace`, etc. Empty
// is allowed.
func RecordConvSuccession(oldConv, newConv, reason string) error {
	if oldConv == "" || newConv == "" {
		return errors.New("RecordConvSuccession: oldConv and newConv must be non-empty")
	}
	if oldConv == newConv {
		return errors.New("RecordConvSuccession: oldConv and newConv must differ")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = d.Exec(`INSERT INTO agent_conv_succession
		(old_conv_id, new_conv_id, reason, succeeded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(old_conv_id) DO UPDATE SET
			new_conv_id = excluded.new_conv_id,
			reason = excluded.reason,
			succeeded_at = excluded.succeeded_at`,
		oldConv, newConv, reason, now)
	if err != nil {
		return err
	}
	// The successor in a succession edge (a reincarnated instance) is
	// an agent — enroll it so it shows on the roster without waiting
	// for its first /v1 call. The predecessor keeps whatever
	// enrollment it already had; v1 does not auto-retire it.
	return EnrollAgent(newConv, "reincarnate")
}

// GetConvSuccessor returns the direct successor of convID, or "" if
// convID has no recorded successor. Use ResolveLatestConv when you
// want the chain walked to the end.
func GetConvSuccessor(convID string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var newID string
	err = d.QueryRow(`SELECT new_conv_id FROM agent_conv_succession
		WHERE old_conv_id = ?`, convID).Scan(&newID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return newID, nil
}

// ResolveLatestConv walks the succession chain forward from convID
// and returns the latest conv-id known to be alive in the chain.
// Returns convID itself if no row references it as old_conv_id.
//
// Cycle protection: stops after 32 hops (cycles shouldn't happen with
// our INSERT ... ON CONFLICT pattern, but DB corruption / manual
// edits could create one — fail-soft to the last known id rather
// than spinning forever).
func ResolveLatestConv(convID string) string {
	if convID == "" {
		return ""
	}
	current := convID
	for range 32 {
		next, err := GetConvSuccessor(current)
		if err != nil || next == "" {
			return current
		}
		if next == convID {
			// Cycle: chain points back at the original. Stop here so
			// callers don't loop. Returning the most-recent-progressed
			// id is more useful than the cycle-entry one.
			return current
		}
		current = next
	}
	return current
}

// GetConvPredecessor returns the conv that was directly replaced by
// convID (the old←new edge), or "" if convID is not recorded as the
// successor of anything (it was never born from a reincarnate/clone).
// This is the backward twin of GetConvSuccessor — the séance feature
// (JOH-25) walks it to find "the agent I succeeded" so a fresh
// incarnation can consult its predecessor's session.
//
// new_conv_id is not the table's primary key (old_conv_id is), so in
// principle two edges could point at the same successor; in practice
// reincarnate/clone mint a brand-new conv per succession, so a
// successor has at most one predecessor. We take the most recent edge
// defensively (ORDER BY succeeded_at DESC) rather than assume
// uniqueness.
func GetConvPredecessor(convID string) (string, error) {
	if convID == "" {
		return "", nil
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	var oldID string
	err = d.QueryRow(`SELECT old_conv_id FROM agent_conv_succession
		WHERE new_conv_id = ?
		ORDER BY succeeded_at DESC, rowid DESC
		LIMIT 1`, convID).Scan(&oldID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return oldID, nil
}

// ResolvePredecessorN walks the succession chain BACKWARD from convID by
// up to n generations and returns the ancestor conv-id reached, plus the
// number of hops actually taken. n must be >= 1.
//
//   - n == 1 returns the immediate predecessor.
//   - If the chain runs out before n hops (we reach the oldest ancestor),
//     the oldest reached ancestor is returned with hops < n — best-effort,
//     so "go back 5" on a 2-deep chain lands on the root rather than
//     erroring.
//   - If convID has no predecessor at all, returns ("", 0, nil) — the
//     caller surfaces "you have no predecessor to consult".
//
// Cycle protection mirrors ResolveLatestConv: a malformed back-edge that
// loops stops at the first repeat rather than spinning.
func ResolvePredecessorN(convID string, n int) (ancestor string, hops int, err error) {
	if convID == "" || n < 1 {
		return "", 0, nil
	}
	seen := map[string]bool{convID: true}
	current := convID
	for hops < n {
		prev, perr := GetConvPredecessor(current)
		if perr != nil {
			return "", hops, perr
		}
		if prev == "" || seen[prev] {
			break // reached the chain root (or a cycle) — stop, return what we have
		}
		seen[prev] = true
		current = prev
		hops++
	}
	if hops == 0 {
		return "", 0, nil
	}
	return current, hops, nil
}

// ListAgentConvSuccessions returns every recorded succession row
// (most recent first). Used by the dashboard / audit views; not
// performance-critical.
func ListAgentConvSuccessions() ([]*AgentConvSuccession, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Tie-break by rowid DESC so two writes within the same RFC3339
	// second still have a deterministic order in the listing.
	rows, err := d.Query(`SELECT old_conv_id, new_conv_id, reason, succeeded_at
		FROM agent_conv_succession ORDER BY succeeded_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*AgentConvSuccession
	for rows.Next() {
		var (
			s     AgentConvSuccession
			tsRaw string
		)
		if err := rows.Scan(&s.OldConvID, &s.NewConvID, &s.Reason, &tsRaw); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, tsRaw); err == nil {
			s.SucceededAt = t
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}
