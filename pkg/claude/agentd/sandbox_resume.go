package agentd

import (
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

type resumeSandboxPolicy struct {
	Snapshot *sandboxpolicy.Snapshot
	Previous *sandboxpolicy.Snapshot
}

// resolveResumeSandboxPolicy reconstructs an offline agent's policy from the
// current global/group/explicit registry state. The previous snapshot supplies
// only stable provenance and private agent-directory bindings; its ordinary
// filesystem/environment values are not launch authority after resume.
func resolveResumeSandboxPolicy(convID string) (*resumeSandboxPolicy, error) {
	previous, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil || previous == nil {
		return &resumeSandboxPolicy{Snapshot: previous, Previous: previous}, err
	}

	var explicitProfileID int64
	var explicitProfileName string
	for _, applied := range previous.Applied {
		switch applied.Scope {
		case sandboxpolicy.ScopeExplicit:
			explicitProfileID = applied.ID
			explicitProfileName = applied.Name
		}
	}
	groupID := previous.ResolutionGroupID
	if groupID == 0 {
		groupID, err = resumeSandboxGroupID(convID)
		if err != nil {
			return nil, err
		}
	}
	current, err := db.ResolveEffectiveSandboxSnapshotByID(groupID, explicitProfileID)
	if errors.Is(err, db.ErrSandboxProfileNotFound) && explicitProfileName != "" {
		// A deleted explicit profile can be recovered by recreating it under its
		// recorded name. Ordinary renames still follow the stable ID above.
		current, err = db.ResolveEffectiveSandboxSnapshot(groupID, explicitProfileName)
	}
	if errors.Is(err, db.ErrSandboxProfileNotFound) {
		return nil, fmt.Errorf("the explicit sandbox profile %q used at launch no longer exists; recreate it under that name, then resume again", explicitProfileName)
	}
	if err != nil {
		return nil, err
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	current, err = reconcileAgentDirectoriesForResume(current, *previous, agentID)
	if err != nil {
		return nil, err
	}
	current = clampResumeProtectedAuthority(current, *previous)
	return &resumeSandboxPolicy{Snapshot: &current, Previous: previous}, nil
}

// clampResumeProtectedAuthority preserves the protected-access decision and the
// read-baseline boundary that were recorded when the agent launched.
//
// Resume deliberately re-resolves the ordinary rules from the current registry
// so an operator can fix a profile and relaunch. That is fine for grants a
// human re-confirms implicitly by editing them — but break-glass and the strict
// read baseline are different: the ticket requires that resume never silently
// gains protected access from a later ambient profile change, and never widens
// minimal back to the broad default. Neither can be re-acknowledged here,
// because resume has no human in the loop.
//
// So this clamps rather than refuses: protected access is intersected with what
// the previous snapshot already held (never added, never widened read→write),
// and the baseline takes the strictest of the two. Both directions are
// fail-safe — a resumed agent can only ever end up with less authority than the
// ambient registry would grant it. An operator who genuinely wants to widen a
// running agent's protected access spawns a fresh one, which goes through the
// acknowledgement gate.
func clampResumeProtectedAuthority(current, previous sandboxpolicy.Snapshot) sandboxpolicy.Snapshot {
	current.Effective.ReadBaseline = sandboxpolicy.StrictestReadBaseline(
		current.Effective.ReadBaseline, previous.Effective.ReadBaseline)

	if len(current.Effective.BreakGlassFilesystem) == 0 {
		// Nothing to clamp, and dropping access the profile no longer grants is
		// always safe.
		return current
	}
	kept := make([]sandboxpolicy.BreakGlassGrant, 0, len(current.Effective.BreakGlassFilesystem))
	for _, grant := range current.Effective.BreakGlassFilesystem {
		if allowed, ok := previousBreakGlassAccess(previous, grant); ok {
			grant.Access = allowed
			kept = append(kept, grant)
		}
	}
	if len(kept) == 0 {
		kept = nil
	}
	current.Effective.BreakGlassFilesystem = kept
	if current.Effective.Provenance.BreakGlassFilesystem != nil {
		retained := map[string][]sandboxpolicy.ProfileSource{}
		for _, grant := range kept {
			if sources, ok := current.Effective.Provenance.BreakGlassFilesystem[grant.Path]; ok {
				retained[grant.Path] = sources
			}
		}
		if len(retained) == 0 {
			retained = nil
		}
		current.Effective.Provenance.BreakGlassFilesystem = retained
	}
	return current
}

// previousBreakGlassAccess reports the strongest access the parent snapshot
// already held for a path, capped at what is being requested now. Coverage is
// segment-aware: an ancestor grant covers its descendants, and a recorded read
// never satisfies a newly-requested write.
func previousBreakGlassAccess(previous sandboxpolicy.Snapshot, want sandboxpolicy.BreakGlassGrant) (sandboxpolicy.Access, bool) {
	best := sandboxpolicy.Access("")
	for _, held := range previous.Effective.BreakGlassFilesystem {
		if !sandboxpolicy.PathContainsOrEqual(held.Path, want.Path) {
			continue
		}
		if held.Access == sandboxpolicy.AccessWrite {
			best = sandboxpolicy.AccessWrite
			break
		}
		best = sandboxpolicy.AccessRead
	}
	if best == "" {
		return "", false
	}
	if want.Access == sandboxpolicy.AccessRead {
		// Requesting less than was held is always fine.
		return sandboxpolicy.AccessRead, true
	}
	if best != sandboxpolicy.AccessWrite {
		// A recorded read must not become a write on resume.
		return sandboxpolicy.AccessRead, true
	}
	return sandboxpolicy.AccessWrite, true
}

// resumeSandboxGroupID recovers the launch group for agents created before a
// dedicated source-group field existed. The ordinary and overwhelmingly common
// one-group case is exact. A legacy multi-group snapshot has no trustworthy
// launch-group provenance: an unchanged profile ID on another membership can
// otherwise be mistaken for the launch group after the real assignment changes.
// Resume therefore succeeds only when every current group tier is equivalent.
func resumeSandboxGroupID(convID string) (int64, error) {
	groups, err := db.ListGroupsForConv(convID)
	if err != nil {
		return 0, err
	}
	switch len(groups) {
	case 0:
		return 0, nil
	case 1:
		return groups[0].ID, nil
	}
	// If every current membership composes the same group tier, selecting any
	// one is value-equivalent (and zero means there is no group tier at all).
	profileID := groups[0].SandboxProfileID
	for _, group := range groups[1:] {
		if group.SandboxProfileID != profileID {
			return 0, fmt.Errorf("cannot determine the sandbox source group for this multi-group agent after its profile assignments changed; leave it in one launch group or restore an unambiguous assignment, then resume again")
		}
	}
	if profileID == 0 {
		return 0, nil
	}
	return groups[0].ID, nil
}
