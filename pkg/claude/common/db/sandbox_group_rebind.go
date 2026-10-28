package db

import (
	"database/sql"
	"errors"
)

// rebindSandboxResolutionGroup keeps an agent's recorded sandbox source group
// (the snapshot's resolution_group_id) in step with its memberships. Resume and
// reincarnation compose the group sandbox tier and group environment from that
// group, so after a move it must name the group the agent moved to, or the
// relaunch keeps the old group's profile: withholding the new group's grants,
// or keeping the old group's. The running process is unaffected; a sandbox
// only changes on relaunch.
//
// While the agent is still a member of the recorded group, nothing changes
// (joining a second group does not move the launch group). Otherwise the
// record is rebound to the group the agent joined most recently, or cleared
// when it has none, so a deleted group's id is never kept. A
// cleared record on a provenance-tracking snapshot is filled in again by the
// next group the agent joins.
//
// Called after every membership change, inside the same transaction when the
// caller has one. Only the one JSON field is rewritten, so the rest of the
// snapshot stays byte-for-byte as recorded.
func rebindSandboxResolutionGroup(q dbExecQuerier, agentID string) error {
	var recorded int64
	var tracked bool
	err := q.QueryRow(`SELECT COALESCE(json_extract(effective_sandbox_config, '$.resolution_group_id'), 0),
		       COALESCE(json_extract(effective_sandbox_config, '$.refresh_group_environment'), 0)
		FROM agents WHERE agent_id = ? AND json_valid(effective_sandbox_config)`, agentID).Scan(&recorded, &tracked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no snapshot recorded yet
	}
	if err != nil {
		return err
	}
	if recorded == 0 && !tracked {
		// A legacy snapshot without source-group provenance: resume already
		// recovers the group from memberships, and refuses to guess among
		// several (resumeSandboxGroupID). Do not guess here either.
		return nil
	}
	if recorded != 0 {
		var member bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_group_members WHERE agent_id = ? AND group_id = ?)`,
			agentID, recorded).Scan(&member); err != nil {
			return err
		}
		if member {
			return nil
		}
	}
	var next int64
	err = q.QueryRow(`SELECT group_id FROM agent_group_members WHERE agent_id = ?
		ORDER BY joined_at DESC, group_id DESC LIMIT 1`, agentID).Scan(&next)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if next == recorded {
		return nil
	}
	if next == 0 {
		_, err = q.Exec(`UPDATE agents SET effective_sandbox_config =
			json_remove(effective_sandbox_config, '$.resolution_group_id') WHERE agent_id = ?`, agentID)
		return err
	}
	_, err = q.Exec(`UPDATE agents SET effective_sandbox_config =
		json_set(effective_sandbox_config, '$.resolution_group_id', ?) WHERE agent_id = ?`, next, agentID)
	return err
}
