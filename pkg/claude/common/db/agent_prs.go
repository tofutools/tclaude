package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// AgentPR is one explicitly presented pull request. The agent_id is the stable
// actor key; (agent_id, PRURL) is unique so one agent can refresh its own PR
// badge while another agent may present the same PR independently. State is
// intentionally loose: callers currently use "", "open", "merged", "closed",
// or "handled".
type AgentPR struct {
	ID        int64
	AgentID   string
	PRURL     string
	Summary   string
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertAgentPR presents or refreshes a PR for an agent, deduped by
// (agent_id, PR URL).
func UpsertAgentPR(agentID, prURL, summary, state string) (AgentPR, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	summary = strings.TrimSpace(summary)
	state = strings.TrimSpace(state)
	if agentID == "" {
		return AgentPR{}, errors.New("UpsertAgentPR: agent_id required")
	}
	if prURL == "" {
		return AgentPR{}, errors.New("UpsertAgentPR: pr_url required")
	}
	now := time.Now().UTC()
	d, err := Open()
	if err != nil {
		return AgentPR{}, err
	}
	if _, err := d.Exec(`INSERT INTO agent_prs
		(agent_id, pr_url, summary, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, pr_url) DO UPDATE SET
			summary = excluded.summary,
			state = excluded.state,
			updated_at = excluded.updated_at`,
		agentID, prURL, summary, state, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		return AgentPR{}, err
	}
	return GetAgentPR(agentID, prURL)
}

// MarkAgentPRHandled marks one PR as handled without deleting the historical
// row. Handled PRs are omitted from dashboard presentation.
func MarkAgentPRHandled(agentID, prURL string) (int64, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	if agentID == "" {
		return 0, errors.New("MarkAgentPRHandled: agent_id required")
	}
	if prURL == "" {
		return 0, errors.New("MarkAgentPRHandled: pr_url required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_prs
		SET state = 'handled', updated_at = ?
		WHERE agent_id = ? AND pr_url = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), agentID, prURL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAgentPR returns the row for an agent+URL pair, or the zero value when
// missing.
func GetAgentPR(agentID, prURL string) (AgentPR, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	if agentID == "" {
		return AgentPR{}, errors.New("GetAgentPR: agent_id required")
	}
	if prURL == "" {
		return AgentPR{}, errors.New("GetAgentPR: pr_url required")
	}
	d, err := Open()
	if err != nil {
		return AgentPR{}, err
	}
	var row AgentPR
	var created, updated string
	err = d.QueryRow(`SELECT id, agent_id, pr_url, summary, state, created_at, updated_at
		FROM agent_prs WHERE agent_id = ? AND pr_url = ?`, agentID, prURL).
		Scan(&row.ID, &row.AgentID, &row.PRURL, &row.Summary, &row.State, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentPR{}, nil
	}
	if err != nil {
		return AgentPR{}, err
	}
	row.CreatedAt = parseTimeOrZero(created)
	row.UpdatedAt = parseTimeOrZero(updated)
	return row, nil
}

// ListUnhandledAgentPRs returns all explicitly presented PRs whose state is not
// handled, keyed by agent_id for dashboard preloading.
func ListUnhandledAgentPRs() (map[string][]AgentPR, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, agent_id, pr_url, summary, state, created_at, updated_at
		FROM agent_prs
		WHERE state <> 'handled'
		ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]AgentPR{}
	for rows.Next() {
		var row AgentPR
		var created, updated string
		if err := rows.Scan(&row.ID, &row.AgentID, &row.PRURL, &row.Summary, &row.State, &created, &updated); err != nil {
			return nil, err
		}
		row.CreatedAt = parseTimeOrZero(created)
		row.UpdatedAt = parseTimeOrZero(updated)
		out[row.AgentID] = append(out[row.AgentID], row)
	}
	return out, rows.Err()
}
