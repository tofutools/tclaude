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
}

// resolveChildSandbox is the rule a spawned child's sandbox comes from
// (tcl-pi13z9). The child's profiles are resolved fresh: the explicit profile,
// otherwise the target group's or the global assignment, with their current
// contents, then the requested harness-config posture. An agent spawner's
// child must then fit inside the spawner's own stored launch snapshot, with
// the spawner's agent-owned directories re-probed first; a spawner from before
// snapshots may not hand down custom capabilities.
//
// It is the one place that rule lives, so changing how a child inherits its
// parent's sandbox changes this function only.
func resolveChildSandbox(in childSandboxInput) (sandboxpolicy.Snapshot, *spawnFailure) {
	// An omission the caller asked for is kept by every relaunch; one the
	// launch mode forces is re-judged when the mode changes (derived).
	effectiveSandbox := sandboxpolicy.OmittedProfilesSnapshot()
	if !in.omitSandboxProfiles {
		effectiveSandbox = sandboxpolicy.DerivedOmittedProfilesSnapshot()
	}
	var policyErr error
	if !sandboxProfilesDisabled(in.harnessName, in.harnessBuiltinMode, in.implementation) &&
		!in.omitSandboxProfiles {
		effectiveSandbox, policyErr = db.ResolveEffectiveSandboxSnapshot(in.group.ID, in.sandboxProfile)
	}
	if errors.Is(policyErr, db.ErrSandboxProfileNotFound) {
		return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile",
			fmt.Sprintf("sandbox profile %q does not exist", in.sandboxProfile)}
	}
	if policyErr != nil {
		return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusBadRequest, "invalid_sandbox_profile", policyErr.Error()}
	}
	if applied, fail := applySpawnHarnessConfig(effectiveSandbox, in.harnessConfig); fail != nil {
		return sandboxpolicy.Snapshot{}, fail
	} else {
		effectiveSandbox = applied
	}
	// A principal has no sandbox to contain the child. Its pinned profile is the
	// bound, and checkSandboxLineage (sandboxScopePinned) refused a launch that
	// would not enforce it.
	if in.spawnerConvID != "" && !in.externalSpawn {
		parentSnapshot, err := db.AgentEffectiveSandboxConfigForConv(in.spawnerConvID)
		if err != nil {
			return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusInternalServerError, "io", "load parent sandbox snapshot: " + err.Error()}
		}
		if parentSnapshot == nil {
			if sandboxpolicy.HasCapabilities(effectiveSandbox) {
				return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted",
					"this parent predates effective sandbox snapshots and may not inherit custom capabilities; relaunch it under current policy or ask the human to spawn the child"}
			}
		} else {
			validatedParent, err := ensureAgentDirectoriesForRelaunch(*parentSnapshot)
			if err != nil {
				return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()}
			}
			if err := sandboxpolicy.RequireContained(validatedParent, effectiveSandbox); err != nil {
				return sandboxpolicy.Snapshot{}, &spawnFailure{http.StatusForbidden, "sandbox_profile_restricted", err.Error()}
			}
		}
	}
	return effectiveSandbox, nil
}
