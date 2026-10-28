package agentd

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// This file is the one place that decides and describes which group an
// agent's group-sourced settings (the sandbox-profile tier and the group
// environment) come from when an operation changes its groups: a move
// (db.MoveAgentGroupMember), a clone that joins a group, a groups clone, and
// an arrival from another node. Operators choose per setting; agents are told
// the outcome.

// groupSourceLabel names a group for an operator or agent: its quoted name, or
// a fallback when it has none.
func groupSourceLabel(groupID int64) string {
	if groupID == 0 {
		return "no group"
	}
	if g, err := db.GetAgentGroupByID(groupID); err == nil && g != nil {
		return fmt.Sprintf("group %q", g.Name)
	}
	return fmt.Sprintf("group #%d", groupID)
}

// sandboxGroupSourcesLine is the sentence an arriving or cloned agent reads
// about its sandbox: which group supplies the profile tier and the group
// environment, and that its own explicit profile and overrides apply on top.
// snapshot is the launch snapshot; nil records no tclaude sandbox profile.
func sandboxGroupSourcesLine(snapshot *sandboxpolicy.Snapshot) string {
	if snapshot == nil {
		return "Sandbox: no tclaude sandbox profile recorded for this launch."
	}
	envGroup, explicit := snapshot.EnvironmentGroupID()
	if snapshot.ResolutionGroupID == 0 && !explicit && !snapshot.RefreshGroupEnvironment {
		return "Sandbox: group profile and environment follow your group memberships."
	}
	profile := "sandbox profile from " + groupSourceLabel(snapshot.ResolutionGroupID)
	if snapshot.ProfilesOmitted {
		profile = "sandbox profiles omitted by launch choice"
	}
	return fmt.Sprintf("Sandbox: %s; group environment from %s; your own explicit profile and environment overrides apply on top. "+
		"Group permissions follow your current memberships.", profile, groupSourceLabel(envGroup))
}

// cloneSandboxForGroup gives a clone that joins groupID its launch sandbox. By
// default both group-sourced settings come from groupID, so the clone runs
// with that group's sandbox from its first launch rather than switching at its
// first resume. keepSandbox / keepEnvironment keep the source's setting
// instead (and any pin the source held on it). fresh reports whether the
// result is a freshly composed chain, whose private agent directories are
// materialized later rather than validated as predecessor bindings.
func cloneSandboxForGroup(sourceConv string, launch *sandboxpolicy.Snapshot, groupID int64,
	keepSandbox, keepEnvironment, fresh bool) (*sandboxpolicy.Snapshot, bool, error) {
	if launch == nil || groupID == 0 || (keepSandbox && keepEnvironment) {
		return launch, fresh, nil
	}
	var source sandboxpolicy.GroupBinding
	if launch.GroupBinding != nil {
		source = *launch.GroupBinding
	}
	sourceEnv, _ := launch.EnvironmentGroupID()
	rebound := *launch
	var binding sandboxpolicy.GroupBinding
	if keepSandbox {
		binding.ProfileGroupPinned = source.ProfileGroupPinned
	} else {
		rebound.ResolutionGroupID = groupID
	}
	envGroup, envPinned := groupID, false
	if keepEnvironment {
		envGroup, envPinned = sourceEnv, source.EnvironmentGroupSet && source.EnvironmentGroupPinned
	}
	if envGroup != rebound.ResolutionGroupID || envPinned {
		binding.EnvironmentGroupSet, binding.EnvironmentGroupID, binding.EnvironmentGroupPinned = true, envGroup, envPinned
	}
	rebound.GroupBinding = nil
	if !binding.IsZero() {
		rebound.GroupBinding = &binding
	}
	current := &rebound
	if rebound.ResolutionGroupID != launch.ResolutionGroupID && !rebound.ProfilesOmitted {
		resolved, err := resolveSandboxChainFrom(sourceConv, &rebound)
		if err != nil {
			return nil, fresh, err
		}
		current, fresh = resolved, true
	}
	refreshed, err := refreshCloneLaunchEnvironment(sourceConv, current, &rebound)
	return refreshed, fresh, err
}
