package agentd

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
)

// How a resume or reincarnation reads the relaunch policy's re-resolve |
// reuse rows (launch slice 9). Re-resolve, the default, is what a relaunch
// did before the table existed; reuse keeps the value stored at the agent's
// last launch.

const (
	groupEnvField               = "group_env"
	groupRemoteControlField     = "group_remote_control"
	sandboxProfileContentsField = "sandbox_profile_contents"
)

// relaunchReuses reports whether policy says to keep field's stored value.
func relaunchReuses(policy launchspec.RelaunchPolicy, field string) bool {
	return policy.Mode(field) == launchspec.ModeReuse
}

// sameAppliedProfiles reports whether two snapshots name the same sandbox
// profiles, by scope and stable ID. Edits (UpdatedAt) are ignored: that is
// what reusing the contents keeps.
func sameAppliedProfiles(a, b []sandboxpolicy.AppliedProfile) bool {
	key := func(list []sandboxpolicy.AppliedProfile) []string {
		out := make([]string, len(list))
		for i, p := range list {
			out[i] = fmt.Sprintf("%s:%d", p.Scope, p.ID)
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(key(a), key(b))
}

// reuseStoredProfileContents decides, under the sandbox_profile_contents
// reuse row, whether this relaunch keeps the profile contents stored at the
// last launch. It does unless the agent's profiles are now different ones:
// which profile a group or the workspace assigns is a setting of its own
// (group_sandbox_assignment) and is always re-read, so a changed assignment
// re-reads the new profiles and says so. A chain that cannot be composed at
// all (a deleted profile) keeps the stored contents: that is what reuse asks.
func reuseStoredProfileContents(convID string, previous *sandboxpolicy.Snapshot) (bool, string) {
	if previous == nil || previous.ProfilesOmitted {
		return false, ""
	}
	current, err := resolveSandboxChainFrom(convID, previous)
	if err != nil {
		return true, ""
	}
	if !sameAppliedProfiles(current.Applied, previous.Applied) {
		return false, fmt.Sprintf("relaunch policy reuses sandbox profile contents, but the agent's sandbox profiles are now %s (were %s), so their current contents apply",
			orNone(appliedProfileNames(current.Applied)), orNone(appliedProfileNames(previous.Applied)))
	}
	return true, ""
}

// reuseStoredGroupEnvironment keeps the group environment stored at the last
// launch, with its explicit and spawn-profile overrides, on current.
func reuseStoredGroupEnvironment(current sandboxpolicy.Snapshot, previous *sandboxpolicy.Snapshot) sandboxpolicy.Snapshot {
	current.LaunchEnvironment = append([]sandboxpolicy.EnvironmentEntry(nil), previous.LaunchEnvironment...)
	current.LaunchEnvironmentOverrides = append([]sandboxpolicy.EnvironmentEntry(nil), previous.LaunchEnvironmentOverrides...)
	current.RefreshGroupEnvironment = previous.RefreshGroupEnvironment
	return current
}

// relaunchRemoteControl decides remote control for a resume or reincarnation.
// carried is the value the relaunch carries today: the agent's state at its
// last launch, including an operator's live toggle. That is what reuse keeps.
//
// Re-resolve reads the group's current remote-control policy. A set policy
// applies (clamped to the harness), as it does at spawn, unless the spawn
// gave --remote-control explicitly: an explicit value stays. An unset policy
// leaves the carried value, so a live toggle is not undone. An agent spawned
// before launch layers were recorded has no record of an explicit value, so a
// policy that would change it is not applied, and the note says so.
//
// It returns the value, a "changed since last launch" line when it differs
// from carried, and a note when the group policy was not applied.
func relaunchRemoteControl(convID, harnessName string, carried bool) (bool, string, string) {
	policy := currentRelaunchPolicy()
	if relaunchReuses(policy, groupRemoteControlField) {
		return carried, "", ""
	}
	groupPolicy, ok := relaunchGroupRemoteControlPolicy(convID)
	if !ok || groupPolicy == nil {
		return carried, "", ""
	}
	want := *groupPolicy
	if want {
		if h, err := harness.Resolve(harnessName); err != nil || !h.CanRemoteControl() {
			want = false
		}
	}
	if want == carried {
		return carried, "", ""
	}
	layers, err := db.AgentLaunchLayersForConv(convID)
	if err != nil {
		return carried, "", ""
	}
	if layers == nil {
		return carried, "", fmt.Sprintf("remote control: the group policy now says %s, but this agent predates launch records, "+
			"so whether its %s was explicit is unknown; kept %s", onOff(want), onOff(carried), onOff(carried))
	}
	if explicitRemoteControl(layers) != nil {
		return carried, "", ""
	}
	return want, fmt.Sprintf("remote control: %s → %s (group policy)", onOff(carried), onOff(want)), ""
}

// relaunchGroupRemoteControlPolicy is the current remote-control policy of
// the agent's launch group: the group its stored sandbox snapshot names for
// the group environment, else its only group. ok is false when the group
// cannot be told (several groups with different policies).
func relaunchGroupRemoteControlPolicy(convID string) (*bool, bool) {
	if previous, err := db.AgentEffectiveSandboxConfigForConv(convID); err == nil && previous != nil {
		if groupID, _ := previous.EnvironmentGroupID(); groupID != 0 {
			g, err := db.GetAgentGroupByID(groupID)
			if err != nil || g == nil {
				return nil, false
			}
			return g.RemoteControl, true
		}
	}
	groups, err := db.ListGroupsForConv(convID)
	if err != nil || len(groups) == 0 {
		return nil, err == nil
	}
	policy := groups[0].RemoteControl
	for _, g := range groups[1:] {
		if !sameBoolPtr(g.RemoteControl, policy) {
			return nil, false
		}
	}
	return policy, true
}

// explicitRemoteControl is the --remote-control value the agent's spawn (or a
// later resume) gave explicitly, or nil.
func explicitRemoteControl(layers *db.AgentLaunchLayers) *bool {
	raw, ok := layers.Explicit["remote_control"].Fields["RemoteControl"]
	if !ok {
		return nil
	}
	var v *bool
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func sameBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
