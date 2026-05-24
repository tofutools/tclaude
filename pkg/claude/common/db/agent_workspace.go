package db

import (
	"database/sql"
	"time"
)

// AgentWorkspace is the live "where the agent is right now" snapshot the
// statusbar (`tclaude status-bar`) refreshes on every Claude Code
// statusline render. Distinct from AgentWorkdir (PostToolUse-driven,
// fires only on tool calls) and conv_index.git_branch (turn-driven,
// updates only on .jsonl append): AgentWorkspace rides CC's render
// cadence, so a plain `git checkout` in an idle session's launch dir
// reaches the dashboard within the next statusline render rather than
// after the next turn.
//
//   - Cwd            CC's workspace.current_dir for the active session
//   - Branch         the git branch in Cwd ("" outside a git repo)
//   - RepoURL        https://github.com/owner/repo, "" for non-GitHub
//   - DefaultBranch  the repo's default branch (main/master/...)
//   - PRNumber       open PR # for Branch; 0 = none
//   - PRURL          web link to that PR
//   - PRState        open|merged|closed; "" = no PR
//   - UpdatedAt      freshness clock ResolveLocation compares against
type AgentWorkspace struct {
	ConvID        string
	Cwd           string
	Branch        string
	RepoURL       string
	DefaultBranch string
	PRNumber      int
	PRURL         string
	PRState       string
	UpdatedAt     time.Time
}

// UpsertAgentWorkspace records the statusbar's live workspace snapshot
// for convID, overwriting any previous value. Called on every statusline
// render — single-row upsert keyed by conv_id, no transaction. An empty
// convID is a silent no-op (a render before the daemon knows the agent
// has nothing to anchor).
func UpsertAgentWorkspace(w AgentWorkspace) error {
	if w.ConvID == "" {
		return nil
	}
	conn, err := Open()
	if err != nil {
		return err
	}
	if w.UpdatedAt.IsZero() {
		w.UpdatedAt = time.Now()
	}
	_, err = conn.Exec(`INSERT INTO agent_workspace
		(conv_id, cwd, branch, repo_url, default_branch, pr_number, pr_url, pr_state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
			cwd            = excluded.cwd,
			branch         = excluded.branch,
			repo_url       = excluded.repo_url,
			default_branch = excluded.default_branch,
			pr_number      = excluded.pr_number,
			pr_url         = excluded.pr_url,
			pr_state       = excluded.pr_state,
			updated_at     = excluded.updated_at`,
		w.ConvID, w.Cwd, w.Branch, w.RepoURL, w.DefaultBranch,
		w.PRNumber, w.PRURL, w.PRState,
		w.UpdatedAt.Format(time.RFC3339Nano))
	return err
}

// GetAgentWorkspace returns the recorded live snapshot for convID, or a
// zero-value AgentWorkspace (and nil error) when no row exists — the
// caller falls back to the older writers (agent_workdir, conv_index).
func GetAgentWorkspace(convID string) (AgentWorkspace, error) {
	conn, err := Open()
	if err != nil {
		return AgentWorkspace{}, err
	}
	var w AgentWorkspace
	var updatedStr string
	err = conn.QueryRow(`SELECT conv_id, cwd, branch, repo_url, default_branch,
			pr_number, pr_url, pr_state, updated_at
		FROM agent_workspace WHERE conv_id = ?`, convID).
		Scan(&w.ConvID, &w.Cwd, &w.Branch, &w.RepoURL, &w.DefaultBranch,
			&w.PRNumber, &w.PRURL, &w.PRState, &updatedStr)
	if err == sql.ErrNoRows {
		return AgentWorkspace{}, nil
	}
	if err != nil {
		return AgentWorkspace{}, err
	}
	w.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
	return w, nil
}

// DeleteAgentWorkspace drops the row for convID. Used when a
// conversation is wiped so the table doesn't accumulate dangling rows.
func DeleteAgentWorkspace(convID string) error {
	conn, err := Open()
	if err != nil {
		return err
	}
	_, err = conn.Exec(`DELETE FROM agent_workspace WHERE conv_id = ?`, convID)
	return err
}
