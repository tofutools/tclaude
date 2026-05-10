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
	return err
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

// MigrateCronJobConvRef rewrites every agent_cron_jobs row referencing
// oldConv (as either owner or target) to point at newConv instead.
// Returns the number of rows updated. Used by the reincarnate
// orchestrator to keep cron jobs pointing at the live conv.
//
// Why this and not a generic "migrate every reference" pass: each
// table's foreign-key story is different (some tables already get
// migrated by the reincarnate flow's group/permission code path, some
// like agent_messages we deliberately don't rewrite for audit). Cron
// jobs are a clean case — the references should always track the
// live conv.
func MigrateCronJobConvRef(oldConv, newConv string) (int64, error) {
	if oldConv == "" || newConv == "" {
		return 0, errors.New("MigrateCronJobConvRef: oldConv and newConv must be non-empty")
	}
	if oldConv == newConv {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_cron_jobs
		SET owner_conv  = CASE WHEN owner_conv  = ?1 THEN ?2 ELSE owner_conv END,
		    target_conv = CASE WHEN target_conv = ?1 THEN ?2 ELSE target_conv END
		WHERE owner_conv = ?1 OR target_conv = ?1`,
		oldConv, newConv)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
