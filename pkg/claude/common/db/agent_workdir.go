package db

import (
	"database/sql"
	"time"
)

// AgentWorkdir is the most-recent directory an agent has been editing
// files in — distinct from sessions.cwd, which is where Claude Code was
// launched. The PostToolUse hook callback upserts the directory of
// every file the agent edits; the daemon's /v1/.../dir endpoints read
// it back so `tclaude agent dir` can report where an agent is actually
// building, not just where it started.
type AgentWorkdir struct {
	ConvID    string
	Dir       string
	UpdatedAt time.Time
}

// UpsertAgentWorkdir records dir as the conv's current working
// directory, overwriting any previous value. Called from the hook
// callback on every PostToolUse that touched a file, so it's on a hot
// path — a single-row upsert keyed by conv_id, no transaction.
//
// Empty convID or dir is a silent no-op: the hook fires for plenty of
// tool calls that carry no usable path, and "nothing to record" is not
// an error.
func UpsertAgentWorkdir(convID, dir string) error {
	if convID == "" || dir == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`INSERT INTO agent_workdir (conv_id, dir, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET dir = excluded.dir, updated_at = excluded.updated_at`,
		convID, dir, time.Now().Format(time.RFC3339Nano))
	return err
}

// GetAgentWorkdir returns the recorded working directory for convID.
// Returns a zero-value AgentWorkdir (and nil error) when no row exists
// — the caller falls back to the launch cwd.
func GetAgentWorkdir(convID string) (AgentWorkdir, error) {
	conn, err := Open()
	if err != nil {
		return AgentWorkdir{}, err
	}
	var w AgentWorkdir
	var updatedStr string
	err = conn.QueryRow(`SELECT conv_id, dir, updated_at FROM agent_workdir WHERE conv_id = ?`, convID).
		Scan(&w.ConvID, &w.Dir, &updatedStr)
	if err == sql.ErrNoRows {
		return AgentWorkdir{}, nil
	}
	if err != nil {
		return AgentWorkdir{}, err
	}
	w.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return w, nil
}

// DeleteAgentWorkdir drops the row for convID. Used when a conversation
// is wiped so the table doesn't accumulate dangling rows.
func DeleteAgentWorkdir(convID string) error {
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`DELETE FROM agent_workdir WHERE conv_id = ?`, convID)
	return err
}
