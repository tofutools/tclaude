package db

import (
	"database/sql"
	"time"
)

// AgentWorkdir is an agent's live "current location" — distinct from
// sessions.cwd, which is the fixed dir Claude Code was launched in.
//
//   - Dir          the directory of the most-recent file the agent edited
//   - WorktreeRoot the git working-tree root containing Dir ("" if Dir
//                  isn't in a git repo)
//   - Branch       the git branch checked out at WorktreeRoot ("" likewise)
//
// The PostToolUse hook callback computes all three on every file edit
// and upserts them here; the daemon's read surfaces (dashboard,
// `agent ls`, `agent dir`) report them so an agent's location stays
// correct even as it hops between sub-repos of a monorepo launch dir.
//
// WorktreeRoot / Branch are empty on rows last written by a pre-v28
// hook — readers fall back to an on-demand git resolution then.
type AgentWorkdir struct {
	ConvID       string
	Dir          string
	WorktreeRoot string
	Branch       string
	UpdatedAt    time.Time
}

// UpsertAgentWorkdir records dir (plus its git worktree root + branch)
// as the conv's current location, overwriting any previous value.
// Called from the hook callback on every PostToolUse that touched a
// file, so it's on a hot path — a single-row upsert keyed by conv_id,
// no transaction.
//
// worktreeRoot / branch may be empty when dir isn't in a git repo (or
// git wasn't resolvable); that's recorded faithfully, not an error.
// Empty convID or dir is a silent no-op: the hook fires for plenty of
// tool calls that carry no usable path, and "nothing to record" is not
// an error.
func UpsertAgentWorkdir(convID, dir, worktreeRoot, branch string) error {
	if convID == "" || dir == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`INSERT INTO agent_workdir (conv_id, dir, worktree_root, branch, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
			dir = excluded.dir, worktree_root = excluded.worktree_root,
			branch = excluded.branch, updated_at = excluded.updated_at`,
		convID, dir, worktreeRoot, branch, time.Now().Format(time.RFC3339Nano))
	return err
}

// HealAgentWorkdirGit backfills the git worktree root + branch on a
// row whose hook left them empty — a row written by a pre-v28 hook, or
// one whose edit-time git resolution failed. A reader that resolves the
// repo root on demand (see agent.ResolveLocation) calls this so the
// next read is a pure DB lookup again, honouring the v28 design goal of
// not shelling out to git per dashboard refresh.
//
// The `worktree_root = ''` guard makes it a no-op once any writer — an
// earlier heal, or a fresh PostToolUse hook — has populated the row, so
// a stale heal can never clobber a real edit. updated_at is left
// untouched: a heal corrects stored data, it is not a new edit.
//
// Empty convID or worktreeRoot is a silent no-op: there is nothing to
// heal a row to.
func HealAgentWorkdirGit(convID, worktreeRoot, branch string) error {
	if convID == "" || worktreeRoot == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`UPDATE agent_workdir SET worktree_root = ?, branch = ?
		WHERE conv_id = ? AND worktree_root = ''`,
		worktreeRoot, branch, convID)
	return err
}

// GetAgentWorkdir returns the recorded current location for convID.
// Returns a zero-value AgentWorkdir (and nil error) when no row exists
// — the caller falls back to the launch cwd.
func GetAgentWorkdir(convID string) (AgentWorkdir, error) {
	conn, err := Open()
	if err != nil {
		return AgentWorkdir{}, err
	}
	var w AgentWorkdir
	var updatedStr string
	err = conn.QueryRow(`SELECT conv_id, dir, worktree_root, branch, updated_at
		FROM agent_workdir WHERE conv_id = ?`, convID).
		Scan(&w.ConvID, &w.Dir, &w.WorktreeRoot, &w.Branch, &updatedStr)
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
