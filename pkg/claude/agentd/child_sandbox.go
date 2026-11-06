package agentd

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// childSandboxInput is what a spawned child's sandbox is decided from.
type childSandboxInput struct {
	group               *db.AgentGroup
	harnessName         string
	harnessBuiltinMode  string
	implementation      string
	sandboxProfile      string // the explicit --sandbox-profile, or ""
	omitSandboxProfiles bool
	harnessConfig       string
	spawnerConvID       string // the spawning agent, or "" for a human
	externalSpawn       bool   // the spawner is an external steward (a principal)
	// sandboxScopePinned says the grant that admitted the spawn is scoped to
	// a sandbox profile. It was judged against the target group's assignment
	// (resolvedSandboxProfileNameForScope), so such a child gets that
	// assignment rather than inheriting a profile the grant never named.
	sandboxScopePinned bool
}

// resolveChildSandbox is the rule a spawned child's sandbox comes from
// (tcl-pi13z9, tcl-23i0pp). It is the one place that rule lives, so changing
// how a child inherits its parent's sandbox changes this function only.
//
//   - An agent's child naming no profile, spawned into the group its parent
//     resolves its own sandbox from, inherits the parent's selected profiles,
//     resolved the way the parent's own next relaunch would resolve them
//     (its sandbox_profile_contents row): the same configuration.
//   - Any other child resolves fresh: the explicit profile, otherwise the
//     target group's or the global assignment, with their current contents.
//
// Then the requested harness-config posture is applied, and an agent
// spawner's child must fit inside the spawner:
//
//   - with no explicit profile, inside the parent's configuration resolved
//     now, so an operator's edit to the parent's profiles does not refuse its
//     children (the edit is the operator's, not an escalation by the parent);
//   - with an explicit profile, inside the parent's stored launch snapshot,
//     with the parent's agent-owned directories re-probed first (step 4
//     moves this onto the resolved-now configuration too).
//
// A spawner from before snapshots may not hand down custom capabilities. A
// human, a peer and an external steward have no parent sandbox here.
func resolveChildSandbox(in childSandboxInput) (sandboxpolicy.Snapshot, *spawnFailure) {
	var parent *childSandboxParent
	// A principal has no sandbox to contain the child. Its pinned profile is the
	// bound, and checkSandboxLineage (sandboxScopePinned) refused a launch that
	// would not enforce it.
	if in.spawnerConvID != "" && !in.externalSpawn {
		stored, err := db.AgentEffectiveSandboxConfigForConv(in.spawnerConvID)
		if err != nil {
			return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusInternalServerError, "io", "load parent sandbox snapshot: " + err.Error()}
		}
		parent = &childSandboxParent{conv: in.spawnerConvID, stored: stored}
	}
	// An omission the caller asked for is kept by every relaunch; one the
	// launch mode forces is re-judged when the mode changes (derived).
	effectiveSandbox := sandboxpolicy.OmittedProfilesSnapshot()
	if !in.omitSandboxProfiles {
		effectiveSandbox = sandboxpolicy.DerivedOmittedProfilesSnapshot()
	}
	if !sandboxProfilesDisabled(in.harnessName, in.harnessBuiltinMode, in.implementation) &&
		!in.omitSandboxProfiles {
		inherited, ok := sandboxpolicy.Snapshot{}, false
		if parent != nil && in.sandboxProfile == "" && !in.sandboxScopePinned {
			inherited, ok = parent.inheritedProfiles(in.group.ID)
		}
		if ok {
			effectiveSandbox = inherited
		} else {
			var policyErr error
			effectiveSandbox, policyErr = db.ResolveEffectiveSandboxSnapshot(in.group.ID, in.sandboxProfile)
			if errors.Is(policyErr, db.ErrSandboxProfileNotFound) {
				return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile",
					fmt.Sprintf("sandbox profile %q does not exist", in.sandboxProfile)}
			}
			if policyErr != nil {
				return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile", policyErr.Error()}
			}
		}
	}
	if applied, fail := applySpawnHarnessConfig(effectiveSandbox, in.harnessConfig); fail != nil {
		return sandboxpolicy.Snapshot{}, fail
	} else {
		effectiveSandbox = applied
	}
	if parent == nil {
		return effectiveSandbox, nil
	}
	if parent.stored == nil {
		if sandboxpolicy.HasCapabilities(effectiveSandbox) {
			return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted",
				"this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to spawn the child"}
		}
		return effectiveSandbox, nil
	}
	bound, fail := parent.bound(in.sandboxProfile != "")
	if fail != nil {
		return sandboxpolicy.Snapshot{}, fail
	}
	if err := sandboxpolicy.RequireContained(bound, effectiveSandbox); err != nil {
		return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted", err.Error()}
	}
	return effectiveSandbox, nil
}

// childSandboxParent is the spawning agent's sandbox as its child is decided
// from: its stored launch snapshot (nil for a parent from before snapshots).
type childSandboxParent struct {
	conv   string
	stored *sandboxpolicy.Snapshot
}

// inheritedProfiles is the parent's own profile chain for a child spawned
// into groupID, and ok false when the child does not inherit it: the parent
// resolves its sandbox from another group, keeps a setting on a group it left
// (GroupBinding), omits its profiles, or its chain cannot be read now. Such a
// child resolves the target group's assignment instead, capped by the parent.
//
// The chain is what the parent's next relaunch would resolve, by its
// sandbox_profile_contents row: the current contents of its selected profiles
// (re-resolve, the default), or the contents stored at its last launch
// (reuse). It carries the profiles only: none of the parent's launch-time
// access notices (an operator's override for that one launch must not become
// the child's authority), and not the parent's own explicit
// harness-config posture (the child's request decides its own). The launch
// environment and group are set for the child by the spawn.
func (p *childSandboxParent) inheritedProfiles(groupID int64) (sandboxpolicy.Snapshot, bool) {
	s := p.stored
	if s == nil || s.ProfilesOmitted || s.GroupBinding != nil || s.ResolutionGroupID == 0 || s.ResolutionGroupID != groupID {
		return sandboxpolicy.Snapshot{}, false
	}
	chain, err := resolveSandboxChainFrom(p.conv, s)
	if err != nil || chain == nil {
		return sandboxpolicy.Snapshot{}, false
	}
	inherited := *chain
	policy, _ := relaunchPolicyFor(p.conv)
	if relaunchReuses(policy, sandboxProfileContentsField) {
		if reuse, _ := reuseStoredProfileContents(p.conv, s); reuse {
			// The stored contents, with the posture the chain composes in
			// place of one the parent's own launch asked for explicitly.
			inherited = unmaterializedAgentDirectories(*s)
			if explicitHarnessConfig(s) != "" {
				inherited.Effective.HarnessConfig = chain.Effective.HarnessConfig
				inherited.Effective.Provenance.HarnessConfig = nil
				if source := chain.Effective.Provenance.HarnessConfig; source != nil {
					copied := *source
					inherited.Effective.Provenance.HarnessConfig = &copied
				}
			}
		}
	}
	inherited.Effective.AccessNotices = sandboxpolicy.ReplaceAccessLaunchNotices(
		sandboxpolicy.ReplaceAccessDegradationNotices(inherited.Effective.AccessNotices))
	inherited.GroupBinding = nil
	inherited.LaunchEnvironment = nil
	inherited.LaunchEnvironmentOverrides = nil
	inherited.UnixSocketMaterialization = nil
	return inherited, true
}

// bound is the configuration the child must fit inside. With an explicit
// profile it is the parent's stored launch snapshot with its agent-owned
// directories re-probed. Otherwise it is the parent's configuration resolved
// now (parentSandboxNow), falling back to that stored snapshot when the
// parent's chain cannot be used now, as the parent's own relaunch would. Any
// other failure refuses: it says nothing about which policy is the parent's.
func (p *childSandboxParent) bound(explicitProfile bool) (sandboxpolicy.Snapshot, *spawnFailure) {
	if !explicitProfile {
		now, err := parentSandboxNow(p.conv, p.stored)
		if err == nil {
			return now, nil
		}
		if !isRelaunchChainUnusable(err) {
			return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusInternalServerError, "io", "resolve the parent's sandbox: " + err.Error()}
		}
	}
	validated, err := ensureAgentDirectoriesForRelaunch(*p.stored)
	if err != nil {
		return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()}
	}
	return validated, nil
}

// parentSandboxNow is the configuration conv's own next relaunch would resolve
// from its stored snapshot, without creating anything: its profile chain under
// the sandbox_profile_contents row, composed with the group environment and
// its explicit harness-config posture as a relaunch composes them. Agent-owned
// directories stay declarations: they are not inherited authority, and
// containment does not read them.
func parentSandboxNow(conv string, stored *sandboxpolicy.Snapshot) (sandboxpolicy.Snapshot, error) {
	policy, _ := relaunchPolicyFor(conv)
	useStored := false
	if relaunchReuses(policy, sandboxProfileContentsField) {
		useStored, _ = reuseStoredProfileContents(conv, stored)
	}
	var chain sandboxpolicy.Snapshot
	if useStored {
		chain = unmaterializedAgentDirectories(*stored)
	} else {
		resolved, err := resolveSandboxChainFrom(conv, stored)
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
		chain = *resolved
	}
	return composeRelaunchSandbox(conv, chain, stored, useStored, relaunchReuses(policy, groupEnvField))
}
