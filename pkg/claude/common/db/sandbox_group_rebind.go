package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// ErrNotGroupMember is returned by MoveAgentGroupMember when the agent is not
// a member of the source group.
var ErrNotGroupMember = errors.New("not a member of the source group")

// MoveOptions are the operator's per-setting choices for a group move. The
// zero value moves every setting to the target group's defaults.
type MoveOptions struct {
	// KeepSandbox keeps composing the source group's sandbox profile.
	KeepSandbox bool
	// KeepEnvironment keeps composing the source group's environment.
	KeepEnvironment bool
}

// MoveAgentGroupMember moves an agent from one group to another in one
// transaction, carrying its role and description. A setting the agent took
// from the source group then follows the target group, unless opts keeps it
// on the source group: it stays there, following that group's later edits,
// until the group is deleted. The running process is unaffected; a sandbox
// only changes on relaunch. An agent already in the target group keeps that
// membership as it is.
func MoveAgentGroupMember(convID string, fromGroupID, toGroupID int64, opts MoveOptions) error {
	if fromGroupID == toGroupID {
		return fmt.Errorf("move: source and target group are the same")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return ErrNotGroupMember
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var role, descr string
	if err := tx.QueryRow(`SELECT role, descr FROM agent_group_members WHERE group_id = ? AND agent_id = ?`,
		fromGroupID, agentID).Scan(&role, &descr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotGroupMember
		}
		return err
	}
	res, err := tx.Exec(`INSERT INTO agent_group_members (group_id, agent_id, role, descr, joined_at)
		SELECT ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL
		ON CONFLICT(group_id, agent_id) DO NOTHING`,
		toGroupID, agentID, role, descr, dbTime(time.Now()), agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var member bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_group_members WHERE group_id = ? AND agent_id = ?)`,
			toGroupID, agentID).Scan(&member); err != nil {
			return err
		}
		if !member {
			return fmt.Errorf("move: agent %s is retired", agentID)
		}
	}
	if _, err := tx.Exec(`DELETE FROM agent_group_members WHERE group_id = ? AND agent_id = ?`, fromGroupID, agentID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id IN (?, ?)`,
		fromGroupID, toGroupID); err != nil {
		return err
	}
	if err := applyMoveOptions(tx, agentID, fromGroupID, opts); err != nil {
		return err
	}
	if err := rebindSandboxResolutionGroup(tx, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

// applyMoveOptions pins the settings the operator kept on the source group,
// before the rebind moves the rest. A setting that did not come from the
// source group is left alone. When the sandbox stays but the environment
// moves, the environment gets its own (unpinned) source, so the rebind moves
// it independently of the pinned sandbox group.
func applyMoveOptions(q dbExecQuerier, agentID string, fromGroupID int64, opts MoveOptions) error {
	snapshot, ok, err := loadAgentSandboxForRebind(q, agentID)
	if err != nil || !ok || !tracksGroupProvenance(snapshot) {
		return err
	}
	binding := snapshot.GroupBinding.Clone()
	if binding == nil {
		binding = &sandboxpolicy.GroupBinding{}
	}
	envGroup, envExplicit := snapshot.EnvironmentGroupID()
	if opts.KeepSandbox && snapshot.ResolutionGroupID == fromGroupID {
		binding.ProfileGroupPinned = true
		if !opts.KeepEnvironment && !envExplicit {
			binding.EnvironmentGroupSet = true
			binding.EnvironmentGroupID = fromGroupID
		}
	}
	if opts.KeepEnvironment && envGroup == fromGroupID {
		binding.EnvironmentGroupSet = true
		binding.EnvironmentGroupID = fromGroupID
		binding.EnvironmentGroupPinned = true
	}
	if *binding == derefBinding(snapshot.GroupBinding) {
		return nil
	}
	snapshot.GroupBinding = binding
	return storeAgentSandboxForRebind(q, agentID, snapshot)
}

// rebindSandboxResolutionGroup keeps an agent's recorded sandbox source groups
// in step with its memberships. Resume and reincarnation compose the group
// sandbox tier from ResolutionGroupID and the group environment from the
// environment source (the same group unless a move split them), so after a
// move they must name the group the agent moved to, or the relaunch keeps the
// old group's settings: withholding the new group's grants, or keeping the
// old group's. The running process is unaffected; a sandbox only changes on
// relaunch.
//
// Per setting: while the agent is still a member of the source group, nothing
// changes (joining a second group does not move it), and a pin made moot by
// rejoining is dropped. A pinned source is kept while its group exists.
// Otherwise the source becomes the group the agent joined most recently, or
// none, so a deleted group's id is never kept. Per-agent overrides (the
// explicit profile, launch environment overrides) are separate fields and
// are never touched.
//
// Legacy snapshots without source-group provenance are left alone: resume
// recovers their group from memberships and refuses to guess among several
// (resumeSandboxGroupID). Called after every membership change, inside the
// caller's transaction when it has one.
func rebindSandboxResolutionGroup(q dbExecQuerier, agentID string) error {
	snapshot, ok, err := loadAgentSandboxForRebind(q, agentID)
	if err != nil || !ok || !tracksGroupProvenance(snapshot) {
		return err
	}
	var latest int64
	err = q.QueryRow(`SELECT group_id FROM agent_group_members WHERE agent_id = ?
		ORDER BY joined_at DESC, group_id DESC LIMIT 1`, agentID).Scan(&latest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	resolve := func(groupID int64, pinned bool) (int64, bool, error) {
		if groupID != 0 {
			var member, exists bool
			if err := q.QueryRow(`SELECT
					EXISTS(SELECT 1 FROM agent_group_members WHERE agent_id = ? AND group_id = ?),
					EXISTS(SELECT 1 FROM agent_groups WHERE id = ?)`,
				agentID, groupID, groupID).Scan(&member, &exists); err != nil {
				return 0, false, err
			}
			if member {
				return groupID, false, nil
			}
			if pinned && exists {
				return groupID, true, nil
			}
		}
		return latest, false, nil
	}

	binding := derefBinding(snapshot.GroupBinding)
	profileGroup, profilePinned, err := resolve(snapshot.ResolutionGroupID, binding.ProfileGroupPinned)
	if err != nil {
		return err
	}
	binding.ProfileGroupPinned = profilePinned
	if binding.EnvironmentGroupSet {
		envGroup, envPinned, err := resolve(binding.EnvironmentGroupID, binding.EnvironmentGroupPinned)
		if err != nil {
			return err
		}
		binding.EnvironmentGroupID, binding.EnvironmentGroupPinned = envGroup, envPinned
		if !envPinned && envGroup == profileGroup {
			// Back in step with the sandbox group: follow it again.
			binding.EnvironmentGroupSet, binding.EnvironmentGroupID = false, 0
		}
	}
	var next *sandboxpolicy.GroupBinding
	if binding != (sandboxpolicy.GroupBinding{}) {
		next = &binding
	}
	if profileGroup == snapshot.ResolutionGroupID && derefBinding(next) == derefBinding(snapshot.GroupBinding) {
		return nil
	}
	snapshot.ResolutionGroupID = profileGroup
	snapshot.GroupBinding = next
	return storeAgentSandboxForRebind(q, agentID, snapshot)
}

// tracksGroupProvenance reports whether a snapshot records its source group.
// A legacy snapshot (no recorded group, no split launch environment, no
// binding) does not, and is left to resume's membership lookup.
func tracksGroupProvenance(snapshot *sandboxpolicy.Snapshot) bool {
	return snapshot.ResolutionGroupID != 0 || snapshot.RefreshGroupEnvironment || snapshot.GroupBinding != nil
}

func derefBinding(b *sandboxpolicy.GroupBinding) sandboxpolicy.GroupBinding {
	if b == nil {
		return sandboxpolicy.GroupBinding{}
	}
	return *b
}

// loadAgentSandboxForRebind reads an agent's recorded snapshot. A missing or
// undecodable snapshot reports ok=false: membership changes must not fail
// over sandbox bookkeeping they cannot interpret.
func loadAgentSandboxForRebind(q dbExecQuerier, agentID string) (*sandboxpolicy.Snapshot, bool, error) {
	var raw string
	err := q.QueryRow(`SELECT effective_sandbox_config FROM agents WHERE agent_id = ?`, agentID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	snapshot, err := unmarshalEffectiveSandboxSnapshot(raw)
	if err != nil || snapshot == nil {
		return nil, false, nil
	}
	return snapshot, true, nil
}

func storeAgentSandboxForRebind(q dbExecQuerier, agentID string, snapshot *sandboxpolicy.Snapshot) error {
	raw, err := marshalEffectiveSandboxSnapshot(snapshot)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE agents SET effective_sandbox_config = ? WHERE agent_id = ?`, raw, agentID)
	return err
}
