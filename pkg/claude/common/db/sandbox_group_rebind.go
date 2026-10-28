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
	// KeepPermissions keeps the source group's group permissions, still
	// bound to that group and following its later edits
	// (agent_retained_group_permissions). Unlike the sandbox, this applies
	// immediately.
	KeepPermissions bool
	// By names the operator for the retained-permissions record.
	By string
}

// MoveAgentGroupMember moves an agent from one group to another in one
// transaction, carrying its role and description. A setting the agent took
// from the source group then follows the target group, unless opts keeps it
// on the source group: it stays there, following that group's later edits,
// until the group is deleted. The running process is unaffected; a sandbox
// only changes on relaunch. An agent already in the target group keeps that
// membership as it is.
func MoveAgentGroupMember(convID string, fromGroupID, toGroupID int64, opts MoveOptions) (MoveResult, error) {
	if fromGroupID == toGroupID {
		return MoveResult{}, fmt.Errorf("move: source and target group are the same")
	}
	d, err := Open()
	if err != nil {
		return MoveResult{}, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return MoveResult{}, err
	}
	if agentID == "" {
		return MoveResult{}, ErrNotGroupMember
	}
	tx, err := d.Begin()
	if err != nil {
		return MoveResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := moveAgentGroupMemberTx(tx, agentID, fromGroupID, toGroupID, opts)
	if err != nil {
		return MoveResult{}, err
	}
	return result, tx.Commit()
}

// MoveResult reports where an agent's group-sourced settings come from after
// a move. Tracked is false for an agent with no recorded sandbox, or a legacy
// one without source-group provenance: its settings follow its memberships
// when it next launches.
type MoveResult struct {
	Tracked            bool
	SandboxGroupID     int64
	EnvironmentGroupID int64
}

func moveAgentGroupMemberTx(tx *sql.Tx, agentID string, fromGroupID, toGroupID int64, opts MoveOptions) (MoveResult, error) {
	var role, descr string
	if err := tx.QueryRow(`SELECT role, descr FROM agent_group_members WHERE group_id = ? AND agent_id = ?`,
		fromGroupID, agentID).Scan(&role, &descr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MoveResult{}, ErrNotGroupMember
		}
		return MoveResult{}, err
	}
	res, err := tx.Exec(`INSERT INTO agent_group_members (group_id, agent_id, role, descr, joined_at)
		SELECT ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL
		ON CONFLICT(group_id, agent_id) DO NOTHING`,
		toGroupID, agentID, role, descr, dbTime(time.Now()), agentID)
	if err != nil {
		return MoveResult{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var member bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_group_members WHERE group_id = ? AND agent_id = ?)`,
			toGroupID, agentID).Scan(&member); err != nil {
			return MoveResult{}, err
		}
		if !member {
			return MoveResult{}, fmt.Errorf("move: agent %s is retired", agentID)
		}
	}
	if _, err := tx.Exec(`DELETE FROM agent_group_members WHERE group_id = ? AND agent_id = ?`, fromGroupID, agentID); err != nil {
		return MoveResult{}, err
	}
	if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id IN (?, ?)`,
		fromGroupID, toGroupID); err != nil {
		return MoveResult{}, err
	}
	if err := applyMoveOptions(tx, agentID, fromGroupID, opts); err != nil {
		return MoveResult{}, err
	}
	if opts.KeepPermissions {
		if _, err := tx.Exec(`INSERT INTO agent_retained_group_permissions (agent_id, group_id, retained_at, retained_by)
			VALUES (?, ?, ?, ?) ON CONFLICT(agent_id, group_id) DO NOTHING`,
			agentID, fromGroupID, dbTime(time.Now()), opts.By); err != nil {
			return MoveResult{}, err
		}
	}
	if err := syncGroupSourcedSettings(tx, agentID, toGroupID); err != nil {
		return MoveResult{}, err
	}
	snapshot, ok, err := loadAgentSandboxForRebind(tx, agentID)
	if err != nil || !ok || !tracksGroupProvenance(snapshot) {
		return MoveResult{}, err
	}
	envGroup, _ := snapshot.EnvironmentGroupID()
	return MoveResult{Tracked: true, SandboxGroupID: snapshot.ResolutionGroupID, EnvironmentGroupID: envGroup}, nil
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
	// A setting the operator did not keep this time moves, even if an
	// earlier move had kept it on another group.
	if !opts.KeepSandbox {
		binding.ProfileGroupPinned = false
	}
	if !opts.KeepEnvironment {
		binding.EnvironmentGroupPinned = false
	}
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

// syncGroupSourcedSettings brings everything an agent takes from its groups
// in step with its memberships after a membership change: a group it belongs
// to again no longer needs a kept-permissions record (so leaving it later
// really drops its permissions), and its sandbox source groups are rebound.
func syncGroupSourcedSettings(q dbExecQuerier, agentID string, preferred int64) error {
	if _, err := q.Exec(`DELETE FROM agent_retained_group_permissions WHERE agent_id = ?
		AND group_id IN (SELECT group_id FROM agent_group_members WHERE agent_id = ?)`, agentID, agentID); err != nil {
		return err
	}
	return rebindSandboxResolutionGroup(q, agentID, preferred)
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
//
// preferred names the group a setting moves to when it must move: a move
// targets that group even when the agent was already a member of it and joined
// another later. Zero means the most recently joined group.
func rebindSandboxResolutionGroup(q dbExecQuerier, agentID string, preferred int64) error {
	snapshot, ok, err := loadAgentSandboxForRebind(q, agentID)
	if err != nil || !ok || !tracksGroupProvenance(snapshot) {
		return err
	}
	var latest int64
	err = q.QueryRow(`SELECT group_id FROM agent_group_members WHERE agent_id = ?
		ORDER BY group_id = ? DESC, joined_at DESC, group_id DESC LIMIT 1`, agentID, preferred).Scan(&latest)
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

// ListRetainedPermissionGroups returns the ids of the active groups whose
// group permissions agentID kept when it was moved out of them, oldest first.
func ListRetainedPermissionGroups(agentID string) ([]int64, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT r.group_id FROM agent_retained_group_permissions r
		JOIN agent_groups g ON g.id = r.group_id AND g.archived_at IS NULL
		WHERE r.agent_id = ? AND NOT EXISTS (
			SELECT 1 FROM agent_group_members m WHERE m.group_id = r.group_id AND m.agent_id = r.agent_id)
		ORDER BY r.retained_at, r.group_id`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListRetainedPermissionHolders returns the current conversations of the
// active agents that kept groupID's permissions without being members of it.
func ListRetainedPermissionHolders(groupID int64) ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT a.current_conv_id FROM agent_retained_group_permissions r
		JOIN agents a ON a.agent_id = r.agent_id AND a.retired_at IS NULL
		WHERE r.group_id = ? AND NOT EXISTS (
			SELECT 1 FROM agent_group_members m WHERE m.group_id = r.group_id AND m.agent_id = r.agent_id)
		ORDER BY r.retained_at, a.current_conv_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var conv string
		if err := rows.Scan(&conv); err != nil {
			return nil, err
		}
		out = append(out, conv)
	}
	return out, rows.Err()
}

// DropRetainedGroupPermissions ends an agent's kept permissions from groupID,
// reporting whether it had any.
func DropRetainedGroupPermissions(agentID string, groupID int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM agent_retained_group_permissions WHERE agent_id = ? AND group_id = ?`, agentID, groupID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
