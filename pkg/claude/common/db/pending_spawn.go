package db

import (
	"database/sql"
	"time"
)

// pending_spawns is the durable record of a dashboard spawn whose conv-id
// has not materialised yet (JOH-205 inc2). A Codex agent generates its
// conv-id at launch but only exposes it after its first turn; an unattended
// pane stuck behind a startup gate (untrusted dir, a new-hooks-config
// prompt, the OpenAI auth modal) never takes that turn, so executeSpawn
// cannot resolve the conv-id synchronously. The dashboard spawn persists
// its full enrollment intent here, keyed by spawn label, and returns a
// PENDING agent the operator can focus to clear the gate; a sweeper
// back-fills the enrollment once the conv-id appears, then deletes the row.
//
// The row carries everything finishSpawnEnrollment needs to complete the
// enrollment WITHOUT the original request in memory, so a daemon restart
// mid-pending loses nothing. label is the spawn label, which is also the
// session-row id — the sweeper resolves the conv-id via LoadSession(label).

// PendingSpawn is one not-yet-enrolled dashboard spawn, mirroring the
// pending_spawns row. The fields reconstruct the spawnParams subset
// finishSpawnEnrollment consumes plus the group_id that locates the group.
type PendingSpawn struct {
	Label          string
	GroupID        int64
	Role           string
	Descr          string
	Name           string
	InitialMessage string
	GroupContext   string
	ReplyToConv    string
	SpawnedByConv  string
	// ReplyToAgent / SpawnedByAgent are the stable agent_id companions of
	// ReplyToConv / SpawnedByConv (JOH-321 F2), DERIVED from them at insert via
	// agent_conversations. The pending-spawn sweeper reconstructs this row minutes
	// later — long enough for the spawner to have rotated — so the durable agent
	// refs let the briefing reply-target + welcome attribution re-resolve the
	// spawner's LIVE generation rather than the stale recorded conv. Empty for a
	// human-initiated spawn / a non-actor conv; the read path falls back to the
	// conv snapshot then.
	ReplyToAgent   string
	SpawnedByAgent string
	WorktreePath   string
	WorktreeBranch string
	// CreatedAt is the RFC3339Nano spawn time, stamped by InsertPendingSpawn.
	CreatedAt string
}

// InsertPendingSpawn records a pending spawn. created_at is stamped here
// (callers leave PendingSpawn.CreatedAt empty). label is the primary key;
// INSERT OR REPLACE keeps the call idempotent should a label ever be
// re-recorded — labels are random per spawn, so in practice this never
// collides.
func InsertPendingSpawn(p *PendingSpawn) error {
	db, err := Open()
	if err != nil {
		return err
	}
	// Dual-write the stable routing/provenance refs (JOH-321 F2): reply_to_agent /
	// spawned_by_agent are DERIVED from reply_to_conv / spawned_by_conv via
	// agent_conversations (agentForConvExpr), the same boundary the v81 backfill
	// used. Any ReplyToAgent/SpawnedByAgent preset on the struct is ignored — the
	// conv columns are the source of truth, so the denormalised refs can't drift.
	_, err = db.Exec(`
		INSERT OR REPLACE INTO pending_spawns
			(label, group_id, role, descr, name, initial_message, group_context,
			 reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			 worktree_path, worktree_branch, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, `+agentForConvExpr+`, `+agentForConvExpr+`, ?, ?, ?)`,
		p.Label, p.GroupID, p.Role, p.Descr, p.Name, p.InitialMessage, p.GroupContext,
		p.ReplyToConv, p.SpawnedByConv, p.ReplyToConv, p.SpawnedByConv,
		p.WorktreePath, p.WorktreeBranch,
		time.Now().Format(time.RFC3339Nano))
	return err
}

// GetPendingSpawn returns the pending spawn with the given label, or
// (nil, nil) when none exists (the sweeper treats that as "already
// enrolled / already cleaned up").
func GetPendingSpawn(label string) (*PendingSpawn, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := db.QueryRow(`
		SELECT label, group_id, role, descr, name, initial_message, group_context,
			reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			worktree_path, worktree_branch, created_at
		FROM pending_spawns WHERE label = ?`, label)
	p, err := scanPendingSpawn(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return p, err
}

// ListPendingSpawns returns every pending spawn, oldest first — the order
// the sweeper processes them.
func ListPendingSpawns() ([]*PendingSpawn, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT label, group_id, role, descr, name, initial_message, group_context,
			reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			worktree_path, worktree_branch, created_at
		FROM pending_spawns ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*PendingSpawn
	for rows.Next() {
		p, err := scanPendingSpawn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingSpawn removes a pending spawn by label. Deleting a missing
// label is a no-op — the sweeper deletes after a successful enrollment and
// must tolerate a concurrent delete (e.g. the human retired the agent).
func DeletePendingSpawn(label string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM pending_spawns WHERE label = ?`, label)
	return err
}

// scanPendingSpawn reads one row into a PendingSpawn. rowScanner (defined
// in agent.go) is the shared Scan surface of *sql.Row and *sql.Rows, so the
// single-row Get and the multi-row List share this helper.
func scanPendingSpawn(s rowScanner) (*PendingSpawn, error) {
	var p PendingSpawn
	if err := s.Scan(&p.Label, &p.GroupID, &p.Role, &p.Descr, &p.Name,
		&p.InitialMessage, &p.GroupContext, &p.ReplyToConv, &p.SpawnedByConv,
		&p.ReplyToAgent, &p.SpawnedByAgent,
		&p.WorktreePath, &p.WorktreeBranch, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}
