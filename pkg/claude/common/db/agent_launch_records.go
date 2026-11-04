package db

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// AgentLaunchRecord pairs an active agent's verbatim first spawn request with
// the launch snapshot its relaunches read, so a caller can compare what was
// asked for with what is recorded. EffectiveSandboxRaw is the stored text, the
// compare-and-swap key for ReplaceAgentEffectiveSandboxConfig.
type AgentLaunchRecord struct {
	AgentID             string
	ConvID              string
	InitialSpawnConfig  string
	EffectiveSandbox    *sandboxpolicy.Snapshot
	EffectiveSandboxRaw string
}

// ListActiveAgentLaunchRecords returns every active agent that has a recorded
// first spawn request. An agent with none (promotion, legacy backfill, the
// pending-spawn sweeper, the group-template instantiator) is left out, as is
// one whose snapshot no longer decodes.
func ListActiveAgentLaunchRecords() ([]AgentLaunchRecord, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT agent_id, current_conv_id, initial_spawn_config, effective_sandbox_config
		FROM agents
		WHERE retired_at IS NULL AND initial_spawn_config <> ''
		ORDER BY agent_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentLaunchRecord
	for rows.Next() {
		var rec AgentLaunchRecord
		if err := rows.Scan(&rec.AgentID, &rec.ConvID, &rec.InitialSpawnConfig, &rec.EffectiveSandboxRaw); err != nil {
			return nil, err
		}
		snapshot, err := unmarshalEffectiveSandboxSnapshot(rec.EffectiveSandboxRaw)
		if err != nil {
			continue
		}
		rec.EffectiveSandbox = snapshot
		out = append(out, rec)
	}
	return out, rows.Err()
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
