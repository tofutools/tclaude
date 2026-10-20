package db

import (
	"database/sql"
	"errors"
	"time"
)

// AgentClaudeSubscription marks an agent that runs on a peer operator's own
// Claude subscription (claude setup-token). It never holds the token.
type AgentClaudeSubscription struct {
	AgentID   string    `json:"agent_id"`
	Peer      string    `json:"peer"`
	CreatedAt time.Time `json:"created_at"`
}

func SetAgentClaudeSubscription(agentID, peer string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO agent_claude_subscriptions (agent_id, peer, created_at) VALUES (?, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET peer = excluded.peer, created_at = excluded.created_at`,
		agentID, peer, dbTime(time.Now()))
	return err
}

// DeleteAgentClaudeSubscription moves an agent back to this node's own login.
func DeleteAgentClaudeSubscription(agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_claude_subscriptions WHERE agent_id = ?`, agentID)
	return err
}

// GetAgentClaudeSubscription returns nil for an agent on this node's own login.
func GetAgentClaudeSubscription(agentID string) (*AgentClaudeSubscription, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var out AgentClaudeSubscription
	var created dbTimestamp
	err = d.QueryRow(`SELECT agent_id, peer, created_at FROM agent_claude_subscriptions WHERE agent_id = ?`, agentID).Scan(&out.AgentID, &out.Peer, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out.CreatedAt = created.Time()
	return &out, nil
}

// ListAgentClaudeSubscriptions maps agent IDs to the peer whose subscription
// each runs on.
func ListAgentClaudeSubscriptions() (map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, peer FROM agent_claude_subscriptions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var agent, peer string
		if err := rows.Scan(&agent, &peer); err != nil {
			return nil, err
		}
		out[agent] = peer
	}
	return out, rows.Err()
}
