package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
	Label string
	// AgentID is the stable actor identity reserved before the harness conv-id
	// materialises. Empty only for legacy rows written before schema v117.
	AgentID string
	// Launching protects a pre-launch reservation from the pending sweeper
	// until the session wrapper has created its row. It is cleared as soon as
	// executeSpawn observes that row; stale launch attempts age out in agentd.
	Launching      bool
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
	// IsOwner / PermissionOverrides are the birth-time access controls the
	// spawn dialog requested: make the agent a group owner, and/or
	// seed its per-slug permission overrides (slug → "grant" | "deny"). The
	// pending-spawn sweeper reconstructs them into spawnParams so enrollSpawnedConv
	// applies the same owner/perm writes the inline paths do. PermissionOverrides
	// is stored as a JSON object in the permission_overrides column (empty string
	// = no overrides); nil/empty here means none.
	IsOwner             bool
	PermissionOverrides map[string]string
	ProcessCommandID    string
	// EffectiveSandbox is the exact value snapshot authorized for the launch.
	// A nil value is reserved for legacy rows created before snapshot support;
	// recovery paths must not re-resolve mutable registry assignments for it.
	EffectiveSandbox *sandboxpolicy.Snapshot
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
	effectiveSandbox, err := marshalEffectiveSandboxSnapshot(p.EffectiveSandbox)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT OR REPLACE INTO pending_spawns
			(label, agent_id, launching, group_id, role, descr, name, initial_message, group_context,
			 reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			 worktree_path, worktree_branch, is_owner, permission_overrides, process_command_id,
			 effective_sandbox_config, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, `+agentForConvExpr+`, `+agentForConvExpr+`, ?, ?, ?, ?, ?, ?, ?)`,
		p.Label, p.AgentID, boolToInt(p.Launching), p.GroupID, p.Role, p.Descr, p.Name, p.InitialMessage, p.GroupContext,
		p.ReplyToConv, p.SpawnedByConv, p.ReplyToConv, p.SpawnedByConv,
		p.WorktreePath, p.WorktreeBranch, boolToInt(p.IsOwner), marshalPermissionOverrides(p.PermissionOverrides), p.ProcessCommandID,
		effectiveSandbox,
		time.Now().Format(time.RFC3339Nano))
	return err
}

// marshalPermissionOverrides encodes a birth-time override map for the
// permission_overrides column: "" for nil/empty (the common case), else a
// compact JSON object. A marshal failure (practically impossible for a
// map[string]string) logs and stores "" rather than failing the whole pending
// insert — the agent still enrolls, just without the overrides the sweeper
// would have applied.
func marshalPermissionOverrides(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		slog.Warn("pending-spawn: failed to marshal permission overrides; storing none", "error", err)
		return ""
	}
	return string(b)
}

// unmarshalPermissionOverrides decodes the permission_overrides column back
// into a map. "" (the common case) yields nil; a malformed blob logs and yields
// nil so a corrupt row still enrolls without overrides rather than wedging the
// sweeper.
func unmarshalPermissionOverrides(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		slog.Warn("pending-spawn: failed to unmarshal permission overrides; ignoring", "raw", s, "error", err)
		return nil
	}
	return m
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
		SELECT label, agent_id, launching, group_id, role, descr, name, initial_message, group_context,
			reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			worktree_path, worktree_branch, is_owner, permission_overrides, process_command_id,
			effective_sandbox_config, created_at
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
		SELECT label, agent_id, launching, group_id, role, descr, name, initial_message, group_context,
			reply_to_conv, spawned_by_conv, reply_to_agent, spawned_by_agent,
			worktree_path, worktree_branch, is_owner, permission_overrides, process_command_id,
			effective_sandbox_config, created_at
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

// ClaimPendingSpawn atomically claims a pending spawn for enrollment by
// deleting its row. Exactly one concurrent caller observes claimed=true; the
// loser sees false and must not run enrollment side effects. If the caller
// fails before enrollment commits, it may reinsert its saved PendingSpawn.
func ClaimPendingSpawn(label string) (bool, error) {
	db, err := Open()
	if err != nil {
		return false, err
	}
	res, err := db.Exec(`DELETE FROM pending_spawns WHERE label = ?`, label)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkPendingSpawnLaunched clears the short pre-launch protection once the
// session row exists. Missing rows are a benign no-op (another backfill may
// already have atomically claimed the reservation).
func MarkPendingSpawnLaunched(label string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE pending_spawns SET launching = 0 WHERE label = ?`, label)
	return err
}

// ClaimPendingSpawnAndBindAgent atomically replaces a pending reservation with
// its enrolled actor identity. On commit, observers see either the pending row
// (and can discover its reserved agent_id) or the conv→agent binding — never a
// gap where generic hook/reaper enrollment could mint a competing actor.
//
// Legacy pending rows have an empty reservedAgentID; those retain the old
// claim-only behavior and let finishSpawnEnrollment allocate from the conv.
func ClaimPendingSpawnAndBindAgent(label, convID, reservedAgentID, via string) (bool, error) {
	label = strings.TrimSpace(label)
	convID = strings.TrimSpace(convID)
	reservedAgentID = strings.TrimSpace(reservedAgentID)
	if label == "" || convID == "" {
		return false, errors.New("ClaimPendingSpawnAndBindAgent: label and conv_id required")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var storedAgentID string
	err = tx.QueryRow(`SELECT agent_id FROM pending_spawns WHERE label = ?`, label).Scan(&storedAgentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if storedAgentID != "" {
		if reservedAgentID != "" && reservedAgentID != storedAgentID {
			return false, fmt.Errorf("ClaimPendingSpawnAndBindAgent: pending %s reserves %s, not %s",
				label, storedAgentID, reservedAgentID)
		}
		reservedAgentID = storedAgentID
	}

	if reservedAgentID != "" {
		if !strings.HasPrefix(reservedAgentID, AgentIDPrefix) {
			return false, fmt.Errorf("ClaimPendingSpawnAndBindAgent: invalid agent_id %q", reservedAgentID)
		}
		existing, err := agentIDForConvTx(tx, convID)
		if err != nil {
			return false, err
		}
		switch {
		case existing == reservedAgentID:
			// A generic enrollment path saw the still-visible reservation and
			// already made the exact binding. Claiming only removes the intent.
		case existing != "":
			return false, fmt.Errorf("ClaimPendingSpawnAndBindAgent: conv %s belongs to %s, not reserved %s",
				convID, existing, reservedAgentID)
		default:
			var occupiedConv string
			err = tx.QueryRow(`SELECT current_conv_id FROM agents WHERE agent_id = ?`, reservedAgentID).Scan(&occupiedConv)
			switch {
			case err == nil:
				return false, fmt.Errorf("ClaimPendingSpawnAndBindAgent: agent %s already heads conv %s",
					reservedAgentID, occupiedConv)
			case !errors.Is(err, sql.ErrNoRows):
				return false, err
			}
			now := time.Now()
			if err := insertAgentTx(tx, reservedAgentID, convID, via, now); err != nil {
				return false, err
			}
			if err := linkConvTx(tx, convID, reservedAgentID, ConvRoleHead, via, now); err != nil {
				return false, err
			}
		}
	}
	res, err := tx.Exec(`DELETE FROM pending_spawns WHERE label = ?`, label)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// scanPendingSpawn reads one row into a PendingSpawn. rowScanner (defined
// in agent.go) is the shared Scan surface of *sql.Row and *sql.Rows, so the
// single-row Get and the multi-row List share this helper.
func scanPendingSpawn(s rowScanner) (*PendingSpawn, error) {
	var p PendingSpawn
	var launching int
	var isOwner int
	var permOverrides string
	var effectiveSandbox string
	if err := s.Scan(&p.Label, &p.AgentID, &launching, &p.GroupID, &p.Role, &p.Descr, &p.Name,
		&p.InitialMessage, &p.GroupContext, &p.ReplyToConv, &p.SpawnedByConv,
		&p.ReplyToAgent, &p.SpawnedByAgent,
		&p.WorktreePath, &p.WorktreeBranch, &isOwner, &permOverrides, &p.ProcessCommandID,
		&effectiveSandbox, &p.CreatedAt); err != nil {
		return nil, err
	}
	p.Launching = launching != 0
	p.IsOwner = isOwner != 0
	p.PermissionOverrides = unmarshalPermissionOverrides(permOverrides)
	var err error
	p.EffectiveSandbox, err = unmarshalEffectiveSandboxSnapshot(effectiveSandbox)
	if err != nil {
		return nil, fmt.Errorf("decode pending spawn %q effective sandbox: %w", p.Label, err)
	}
	return &p, nil
}

// PendingAgentIDForConv resolves the stable identity reserved by a pending
// spawn once its session row has acquired convID. Generic enrollment paths use
// this to avoid minting a competing actor before the pending sweeper runs.
func PendingAgentIDForConv(convID string) (string, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return "", nil
	}
	d, err := Open()
	if err != nil {
		return "", err
	}
	var agentID string
	err = d.QueryRow(`
		SELECT p.agent_id
		FROM pending_spawns p
		JOIN sessions s ON s.id = p.label
		WHERE s.conv_id = ? AND p.agent_id <> ''
		ORDER BY p.created_at ASC
		LIMIT 1`, convID).Scan(&agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return agentID, err
}

func marshalEffectiveSandboxSnapshot(snapshot *sandboxpolicy.Snapshot) (string, error) {
	if snapshot == nil {
		return "", nil
	}
	normalized, err := sandboxpolicy.NormalizeSnapshotVersion(*snapshot)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(&normalized)
	if err != nil {
		return "", fmt.Errorf("marshal effective sandbox snapshot: %w", err)
	}
	return string(b), nil
}

func unmarshalEffectiveSandboxSnapshot(raw string) (*sandboxpolicy.Snapshot, error) {
	if raw == "" {
		return nil, nil
	}
	var snapshot sandboxpolicy.Snapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil, err
	}
	normalized, err := sandboxpolicy.NormalizeSnapshotVersion(snapshot)
	if err != nil {
		return nil, err
	}
	// Frozen snapshots are bookkeeping data until a launch or lifecycle
	// boundary consumes them. Do not touch the filesystem while scanning rows:
	// a deleted worktree or temporary grant must not wedge unrelated session or
	// pending-spawn listings. Every authority-use boundary revalidates the
	// snapshot immediately before applying it.
	return &normalized, nil
}
