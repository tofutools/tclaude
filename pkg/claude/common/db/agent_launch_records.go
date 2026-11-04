package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// AgentLaunchRecord pairs an agent's verbatim first spawn request with the
// launch snapshot its relaunches read, so a caller can compare what was asked
// for with what is recorded. EffectiveSandboxRaw is the stored text, the
// compare-and-swap key for ReplaceAgentEffectiveSandboxConfig.
type AgentLaunchRecord struct {
	AgentID             string
	ConvID              string
	InitialSpawnConfig  string
	EffectiveSandbox    *sandboxpolicy.Snapshot
	EffectiveSandboxRaw string
	Retired             bool
}

const agentLaunchRecordColumns = `agent_id, current_conv_id, initial_spawn_config, effective_sandbox_config, retired_at IS NOT NULL`

// ListActiveAgentLaunchRecords returns every active agent that has a recorded
// first spawn request. An agent with none (promotion, legacy backfill, the
// pending-spawn sweeper, the group-template instantiator) is left out, as is
// one whose snapshot no longer decodes.
//
// Retired agents are left out in the query, which idx_agents_active serves,
// so the scan stays proportional to the active roster however many retired
// agents an install holds. includeRetired adds them back for a read-only
// listing; that scan reads every agents row.
func ListActiveAgentLaunchRecords() ([]AgentLaunchRecord, error) {
	return ListAgentLaunchRecords(false)
}

// ListAgentLaunchRecords is ListActiveAgentLaunchRecords, with retired agents
// too when includeRetired.
func ListAgentLaunchRecords(includeRetired bool) ([]AgentLaunchRecord, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(agentLaunchRecordsQuery(includeRetired))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentLaunchRecord
	for rows.Next() {
		rec, ok, err := scanAgentLaunchRecord(rows)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rec)
		}
	}
	return out, rows.Err()
}

func agentLaunchRecordsQuery(includeRetired bool) string {
	where := `retired_at IS NULL AND initial_spawn_config <> ''`
	if includeRetired {
		where = `initial_spawn_config <> ''`
	}
	return `SELECT ` + agentLaunchRecordColumns + ` FROM agents WHERE ` + where + ` ORDER BY agent_id`
}

// ActiveAgentLaunchRecord is one active agent's launch record, nil when the
// agent is retired or unknown, has no recorded spawn request, or its snapshot
// no longer decodes.
func ActiveAgentLaunchRecord(agentID string) (*AgentLaunchRecord, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+agentLaunchRecordColumns+` FROM agents
		WHERE agent_id = ? AND retired_at IS NULL AND initial_spawn_config <> ''`, strings.TrimSpace(agentID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return nil, rows.Err()
	}
	rec, ok, err := scanAgentLaunchRecord(rows)
	if err != nil || !ok {
		return nil, err
	}
	return &rec, nil
}

func scanAgentLaunchRecord(rows *sql.Rows) (AgentLaunchRecord, bool, error) {
	var rec AgentLaunchRecord
	if err := rows.Scan(&rec.AgentID, &rec.ConvID, &rec.InitialSpawnConfig, &rec.EffectiveSandboxRaw, &rec.Retired); err != nil {
		return rec, false, err
	}
	snapshot, err := unmarshalEffectiveSandboxSnapshot(rec.EffectiveSandboxRaw)
	if err != nil {
		return rec, false, nil
	}
	rec.EffectiveSandbox = snapshot
	return rec, true, nil
}

// ErrAgentLaunchRecordChanged is returned when an agent's stored launch
// snapshot changed after it was read, so the replacement was not written.
var ErrAgentLaunchRecordChanged = errors.New("the agent's launch snapshot changed since it was read")

// ReplaceAgentEffectiveSandboxConfig writes snapshot as agentID's launch
// snapshot only if the stored text is still previousRaw, so a relaunch that
// recorded a new snapshot meanwhile is never overwritten.
func ReplaceAgentEffectiveSandboxConfig(agentID, previousRaw string, snapshot *sandboxpolicy.Snapshot) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("ReplaceAgentEffectiveSandboxConfig: agent_id required")
	}
	raw, err := marshalEffectiveSandboxSnapshot(snapshot)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agents SET effective_sandbox_config = ?
		WHERE agent_id = ? AND effective_sandbox_config = ? AND retired_at IS NULL`,
		raw, agentID, previousRaw)
	if err != nil {
		return fmt.Errorf("replace agent effective sandbox: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrAgentLaunchRecordChanged
	}
	return nil
}
