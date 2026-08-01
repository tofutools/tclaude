package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const (
	OpenCodeStatePrivate      = "private"
	OpenCodeStateLegacyShared = "legacy-shared"
)

type OpenCodeAgentStateAllocation struct {
	AgentID   string
	Mode      string
	StateRoot string
	CreatedAt time.Time
}

func InsertOpenCodeAgentStateAllocation(allocation OpenCodeAgentStateAllocation) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	if allocation.CreatedAt.IsZero() {
		allocation.CreatedAt = time.Now().UTC()
	}
	result, err := d.Exec(`
		INSERT OR IGNORE INTO opencode_agent_state_allocations
			(agent_id, mode, state_root, created_at)
		VALUES (?, ?, ?, ?)
	`, strings.TrimSpace(allocation.AgentID), allocation.Mode, allocation.StateRoot,
		dbTime(allocation.CreatedAt))
	if err != nil {
		return false, fmt.Errorf("insert OpenCode agent state allocation: %w", err)
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

func GetOpenCodeAgentStateAllocation(agentID string) (*OpenCodeAgentStateAllocation, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var allocation OpenCodeAgentStateAllocation
	var created dbTimestamp
	err = d.QueryRow(`
		SELECT agent_id, mode, state_root, created_at
		FROM opencode_agent_state_allocations
		WHERE agent_id = ?
	`, strings.TrimSpace(agentID)).Scan(
		&allocation.AgentID, &allocation.Mode, &allocation.StateRoot, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	allocation.CreatedAt = created.Time()
	return &allocation, nil
}
