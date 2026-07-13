package agentd

import (
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

type resumeSandboxPolicy struct {
	Snapshot *sandboxpolicy.Snapshot
}

// resolveResumeSandboxPolicy reconstructs an offline agent's policy from the
// current global/group/explicit registry state. The previous snapshot supplies
// only stable provenance and private agent-directory bindings; its ordinary
// filesystem/environment values are not launch authority after resume.
func resolveResumeSandboxPolicy(convID string) (*resumeSandboxPolicy, error) {
	previous, err := db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil || previous == nil {
		return &resumeSandboxPolicy{Snapshot: previous}, err
	}

	var explicitProfileID, previousGroupProfileID int64
	var explicitProfileName string
	for _, applied := range previous.Applied {
		switch applied.Scope {
		case sandboxpolicy.ScopeExplicit:
			explicitProfileID = applied.ID
			explicitProfileName = applied.Name
		case sandboxpolicy.ScopeGroup:
			previousGroupProfileID = applied.ID
		}
	}
	groupID := previous.ResolutionGroupID
	if groupID == 0 {
		groupID, err = resumeSandboxGroupID(convID, previousGroupProfileID)
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
	return &resumeSandboxPolicy{Snapshot: &current}, nil
}

// resumeSandboxGroupID recovers the launch group for agents created before a
// dedicated source-group field existed. The ordinary and overwhelmingly common
// one-group case is exact. For multi-group actors, the previous snapshot's
// stable group-profile ID disambiguates profile edits; an ambiguous assignment
// change fails before launch instead of choosing a group that could widen the
// sandbox unexpectedly.
func resumeSandboxGroupID(convID string, previousGroupProfileID int64) (int64, error) {
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
	if previousGroupProfileID > 0 {
		var match int64
		for _, group := range groups {
			if group.SandboxProfileID != previousGroupProfileID {
				continue
			}
			if match == 0 {
				match = group.ID
			}
		}
		if match != 0 {
			return match, nil
		}
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
