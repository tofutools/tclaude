package db

import (
	"database/sql"
	"errors"
)

// SetAgentPAFExtensions stores agentID's carried extension document.
func SetAgentPAFExtensions(agentID string, data []byte) error {
	if agentID == "" {
		return errors.New("agent paf extensions: empty agent id")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO agent_paf_extensions (agent_id, data) VALUES (?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET data = excluded.data`, agentID, string(data))
	return err
}

// AgentPAFExtensions returns what SetAgentPAFExtensions stored, or nil when the
// agent has none.
func AgentPAFExtensions(agentID string) ([]byte, error) {
	if agentID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var doc string
	err = d.QueryRow(`SELECT data FROM agent_paf_extensions WHERE agent_id = ?`, agentID).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []byte(doc), nil
}
