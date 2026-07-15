package db

import (
	"fmt"
	"strings"
	"time"
)

// RetireAgentAuthorizationOutcome reports every durable authority row removed
// by one retirement transaction.
type RetireAgentAuthorizationOutcome struct {
	GroupsLeft    []string
	OwnerGroupIDs []int64
	PermsRevoked  int64
	SudoRevoked   int64
	CronDisabled  int64
	Retired       bool
}

// RetireAgentAuthorizationByConv atomically removes every permanent
// permission override, sudo grant, group membership and ownership, disables
// owned cron jobs, then marks convID's actor retired. Coupling the state
// transition to every SQLite authority row closes post-revoke grant windows:
// conditional writers either commit before this transaction and are removed
// by it, or observe retired_at afterward and refuse the write. Any failure
// rolls the complete authority and enrollment state back for a truthful,
// retry-safe response.
func RetireAgentAuthorizationByConv(convID, by, reason string) (RetireAgentAuthorizationOutcome, error) {
	var out RetireAgentAuthorizationOutcome
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return out, fmt.Errorf("revoke permission grants: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return out, fmt.Errorf("revoke permission grants: %w", err)
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return out, fmt.Errorf("revoke permission grants: resolve agent: %w", err)
	}
	if agentID == "" {
		return out, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return out, fmt.Errorf("revoke permission grants: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT g.name FROM agent_groups g
		JOIN agent_group_members m ON m.group_id = g.id
		WHERE m.agent_id = ? ORDER BY g.name`, agentID)
	if err != nil {
		return out, fmt.Errorf("list groups: %w", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("list groups: %w", err)
		}
		out.GroupsLeft = append(out.GroupsLeft, name)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("list groups: %w", err)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("list groups: %w", err)
	}

	rows, err = tx.Query(`SELECT group_id FROM agent_group_owners WHERE agent_id = ? ORDER BY group_id`, agentID)
	if err != nil {
		return out, fmt.Errorf("list group ownerships: %w", err)
	}
	for rows.Next() {
		var groupID int64
		if err := rows.Scan(&groupID); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("list group ownerships: %w", err)
		}
		out.OwnerGroupIDs = append(out.OwnerGroupIDs, groupID)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("list group ownerships: %w", err)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("list group ownerships: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM agent_group_owners WHERE agent_id = ?`, agentID); err != nil {
		return out, fmt.Errorf("unjoin group ownerships: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM agent_group_members WHERE agent_id = ?`, agentID); err != nil {
		return out, fmt.Errorf("unjoin groups: %w", err)
	}
	res, err := tx.Exec(`UPDATE agent_cron_jobs
		SET enabled = 0, disabled_reason = ?
		WHERE owner_agent = ? AND (enabled <> 0 OR disabled_reason <> ?)`,
		CronDisabledReasonAgentRetired, agentID, CronDisabledReasonAgentRetired)
	if err != nil {
		return RetireAgentAuthorizationOutcome{}, fmt.Errorf("disable cron jobs: %w", err)
	}
	out.CronDisabled, _ = res.RowsAffected()

	res, err = tx.Exec(`DELETE FROM agent_permissions WHERE agent_id = ?`, agentID)
	if err != nil {
		return RetireAgentAuthorizationOutcome{}, fmt.Errorf("revoke permission grants: %w", err)
	}
	out.PermsRevoked, _ = res.RowsAffected()

	res, err = tx.Exec(`UPDATE agent_sudo_grants SET revoked_at = ?
		WHERE agent_id = ? AND revoked_at = ''`, time.Now().Format(time.RFC3339Nano), agentID)
	if err != nil {
		return RetireAgentAuthorizationOutcome{}, fmt.Errorf("revoke sudo grants: %w", err)
	}
	out.SudoRevoked, _ = res.RowsAffected()

	byAgent, _ := agentIDForConvTx(tx, by)
	res, err = tx.Exec(`UPDATE agents
		SET retired_at = ?, retired_by = ?, retire_reason = ?, retired_by_agent = ?
		WHERE agent_id = ? AND retired_at = ''`,
		time.Now().Format(time.RFC3339Nano), by, reason, byAgent, agentID)
	if err != nil {
		return RetireAgentAuthorizationOutcome{}, fmt.Errorf("retire: %w", err)
	}
	retiredRows, _ := res.RowsAffected()
	out.Retired = retiredRows > 0

	if err := tx.Commit(); err != nil {
		return RetireAgentAuthorizationOutcome{}, fmt.Errorf("commit retirement revocation: %w", err)
	}
	return out, nil
}
