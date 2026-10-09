package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Subjects of the agent message tclaude queues after a compaction or /clear
// boundary to re-inject the agent's startup context. Each boundary kind has
// its own subject because the subject is also the dedupe key
// InsertReinjectedContextMessage checks: a /clear shortly after a compaction
// is a separate boundary and must not be swallowed by it.
const (
	ReinjectedAfterCompactSubject = "Startup context (re-injected after compaction)"
	ReinjectedAfterClearSubject   = "Startup context (re-injected after /clear)"
)

// IsReinjectedContextSubject reports whether subject marks a re-injection.
func IsReinjectedContextSubject(subject string) bool {
	return subject == ReinjectedAfterCompactSubject || subject == ReinjectedAfterClearSubject
}

// AgentStartupSnapshot is the per-agent record of spawn-time facts a later
// compaction or /clear boundary needs to rebuild the startup context. See
// migrateV234toV235 for what is stored and, as importantly, what is not.
type AgentStartupSnapshot struct {
	AgentID        string
	SpawnGroupID   int64
	SpawnedByAgent string
	// IncludeGroupContext mirrors the spawn's include_group_context choice.
	IncludeGroupContext bool
	ProfileContext      string
	WorktreePath        string
	WorktreeBranch      string
	BriefMessageID      int64
	CreatedAt           time.Time
}

// UpsertAgentStartupSnapshot records (or replaces) an agent's snapshot.
func UpsertAgentStartupSnapshot(s AgentStartupSnapshot) error {
	agentID := strings.TrimSpace(s.AgentID)
	if agentID == "" {
		return errors.New("UpsertAgentStartupSnapshot: agent_id required")
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO agent_startup_snapshots
		(agent_id, spawn_group_id, spawned_by_agent, include_group_ctx, profile_context,
		 worktree_path, worktree_branch, brief_message_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			spawn_group_id   = excluded.spawn_group_id,
			spawned_by_agent = excluded.spawned_by_agent,
			include_group_ctx = excluded.include_group_ctx,
			profile_context  = excluded.profile_context,
			worktree_path    = excluded.worktree_path,
			worktree_branch  = excluded.worktree_branch,
			brief_message_id = excluded.brief_message_id,
			created_at       = excluded.created_at`,
		agentID, s.SpawnGroupID, strings.TrimSpace(s.SpawnedByAgent), boolToInt(s.IncludeGroupContext), s.ProfileContext,
		s.WorktreePath, s.WorktreeBranch, s.BriefMessageID, dbTime(s.CreatedAt))
	return err
}

// GetAgentStartupSnapshot returns the agent's snapshot, or nil when none was
// recorded (an agent that was not spawned by agentd, or one born before
// snapshots existed).
func GetAgentStartupSnapshot(agentID string) (*AgentStartupSnapshot, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var s AgentStartupSnapshot
	var created dbTimestamp
	var includeGroup int64
	err = d.QueryRow(`SELECT agent_id, spawn_group_id, spawned_by_agent, include_group_ctx, profile_context,
		worktree_path, worktree_branch, brief_message_id, created_at
		FROM agent_startup_snapshots WHERE agent_id = ?`, agentID).Scan(
		&s.AgentID, &s.SpawnGroupID, &s.SpawnedByAgent, &includeGroup, &s.ProfileContext,
		&s.WorktreePath, &s.WorktreeBranch, &s.BriefMessageID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt = created.Time()
	s.IncludeGroupContext = includeGroup != 0
	return &s, nil
}

// AgentGroupMembership is one of an agent's group memberships together with
// the group row it belongs to.
type AgentGroupMembership struct {
	Group    *AgentGroup
	Role     string
	Descr    string
	JoinedAt time.Time
}

// ListAgentGroupMembershipsByJoin returns an agent's memberships oldest-joined
// first (ties broken by group id, so the order is stable).
func ListAgentGroupMembershipsByJoin(agentID string) ([]AgentGroupMembership, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT m.group_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m
		WHERE m.agent_id = ?
		ORDER BY m.joined_at, m.group_id`, agentID)
	if err != nil {
		return nil, err
	}
	type member struct {
		groupID     int64
		role, descr string
		joinedAt    time.Time
	}
	var members []member
	for rows.Next() {
		var m member
		var joined dbTimestamp
		if err := rows.Scan(&m.groupID, &m.role, &m.descr, &joined); err != nil {
			_ = rows.Close()
			return nil, err
		}
		m.joinedAt = joined.Time()
		members = append(members, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AgentGroupMembership, 0, len(members))
	for _, m := range members {
		g, err := GetAgentGroupByID(m.groupID)
		if err != nil {
			return nil, err
		}
		if g == nil {
			continue
		}
		out = append(out, AgentGroupMembership{Group: g, Role: m.role, Descr: m.descr, JoinedAt: m.joinedAt})
	}
	return out, nil
}

// InsertReinjectedContextMessage queues m (whose Subject must be one of the
// re-injection subjects) unless a message with the same subject was already
// queued for the same recipient actor within window. It returns the new message id, or 0 when the
// insert was skipped as a duplicate.
//
// The window exists because one boundary can be announced twice — Claude Code
// fires SessionStart(source=compact) and PostCompact for the same compaction —
// and both arrive in separate hook processes. The check and the insert share
// one transaction, and SQLite serializes writers, so two racing callers cannot
// both insert.
func InsertReinjectedContextMessage(m *AgentMessage, window time.Duration) (int64, error) {
	if m == nil || !IsReinjectedContextSubject(m.Subject) {
		return 0, errors.New("InsertReinjectedContextMessage: wrong subject")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var recent int
	err = tx.QueryRow(`SELECT COUNT(*) FROM agent_messages
		WHERE to_agent = COALESCE((SELECT agent_id FROM agent_conversations WHERE conv_id = ?), '')
		  AND to_agent != ''
		  AND subject = ?
		  AND created_at >= ?`,
		m.ToConv, m.Subject, dbTime(m.CreatedAt.Add(-window))).Scan(&recent)
	if err != nil {
		return 0, err
	}
	if recent > 0 {
		return 0, nil
	}
	id, err := insertAgentMessage(tx, m)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
