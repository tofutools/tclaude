package agentd

import (
	"errors"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

type resumeSandboxPolicy struct {
	Snapshot      *sandboxpolicy.Snapshot
	Previous      *sandboxpolicy.Snapshot
	SSHWorkaround bool
}

// cleanupUncommittedResumeSandboxPolicy removes generation-specific roots
// materialized while preparing a relaunch that will not be attempted.
func cleanupUncommittedResumeSandboxPolicy(policy *resumeSandboxPolicy) error {
	if policy == nil || policy.Snapshot == nil || policy.Previous == nil {
		return nil
	}
	_, err := removeSupersededMaterializedAgentDirectories(*policy.Snapshot, *policy.Previous)
	return err
}

// resolveCurrentSandboxChainForConv answers what a relaunch of this
// conversation would compose from the CURRENT registry — global, then group,
// then the explicit profile recorded at launch — and returns it alongside the
// recorded snapshot it was derived from.
//
// It is the side-effect-free half of resolveResumeSandboxPolicy below, split out
// because the sandbox-implementation assignment has to judge the same chain the
// relaunch will judge, and must not materialize agent directories or rotate SSH
// bindings to find out. The recorded snapshot alone is not that answer: it is
// last-launch state, so a ceiling added to the global or group profile since then
// is absent from it, and gates run against it would clear a posture the relaunch
// then refuses.
//
// A conversation with no recorded snapshot, or one whose durable choice was to
// omit profiles entirely, has nothing to re-resolve; both cases return the
// recorded value as the current one so callers see a single answer.
func resolveCurrentSandboxChainForConv(
	convID string,
) (current, previous *sandboxpolicy.Snapshot, err error) {
	previous, err = db.AgentEffectiveSandboxConfigForConv(convID)
	if err != nil || previous == nil {
		return previous, previous, err
	}
	if previous.ProfilesOmitted {
		return previous, previous, nil
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
			return nil, nil, err
		}
	}
	resolved, err := db.ResolveEffectiveSandboxSnapshotByID(groupID, explicitProfileID)
	if errors.Is(err, db.ErrSandboxProfileNotFound) && explicitProfileName != "" {
		// A deleted explicit profile can be recovered by recreating it under its
		// recorded name. Ordinary renames still follow the stable ID above.
		resolved, err = db.ResolveEffectiveSandboxSnapshot(groupID, explicitProfileName)
	}
	if errors.Is(err, db.ErrSandboxProfileNotFound) {
		return nil, nil, fmt.Errorf("the explicit sandbox profile %q used at launch no longer exists; recreate it under that name, then resume again", explicitProfileName)
	}
	if err != nil {
		return nil, nil, err
	}
	return &resolved, previous, nil
}

// resolveResumeSandboxPolicy reconstructs an offline agent's policy from the
// current global/group/explicit registry state. The previous snapshot supplies
// only stable provenance and private agent-directory bindings; its ordinary
// filesystem/environment values are not launch authority after resume.
func resolveResumeSandboxPolicy(
	convID string,
	sshWorkaround bool,
	sshLaunchKey, harnessName, harnessBuiltinMode, sandboxImplementation string,
) (*resumeSandboxPolicy, error) {
	resolved, previous, err := resolveCurrentSandboxChainForConv(convID)
	if err != nil || previous == nil {
		return &resumeSandboxPolicy{Snapshot: previous, Previous: previous}, err
	}
	if previous.ProfilesOmitted {
		// This is a durable per-agent launch choice, not an empty ambient
		// resolution. Do not let later global/group assignments reappear on
		// resume or reincarnation.
		return &resumeSandboxPolicy{Snapshot: previous, Previous: previous}, nil
	}
	current := *resolved
	sshWorkaround = sshWorkaround && codexSSHWorkaroundApplies(
		harnessName, harnessBuiltinMode, sandboxImplementation, &current)
	current, err = configureCodexSSHWorkaroundDeclaration(current, sshWorkaround)
	if err != nil {
		return nil, err
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	// The SSH config is regenerated for every process generation. Its fresh,
	// generation-keyed root is not mounted into the predecessor, which is
	// essential during reincarnation: that pane remains live until its
	// successor has started and must not be able to race daemon-side writes.
	previousForReconcile, err := configureCodexSSHWorkaroundDeclaration(*previous, false)
	if err != nil {
		return nil, err
	}
	freshBindingKeys := map[string]string{}
	if sshWorkaround {
		if sshLaunchKey == "" {
			return nil, fmt.Errorf("codex SSH workaround launch key is missing")
		}
		freshBindingKeys[codexSSHAgentDirectory] = sshLaunchKey
	}
	current, err = reconcileAgentDirectoriesForResume(current, previousForReconcile, agentID, freshBindingKeys)
	if err != nil {
		return nil, err
	}
	current.Effective.AccessNotices = mergeResumeAccessNotices(
		current.Effective.AccessNotices,
		previous.Effective.AccessNotices,
	)
	return &resumeSandboxPolicy{
		Snapshot: &current, Previous: previous, SSHWorkaround: sshWorkaround,
	}, nil
}

func mergeResumeAccessNotices(
	current, previous []sandboxpolicy.AccessNotice,
) []sandboxpolicy.AccessNotice {
	hasPreviousDegradation := false
	for _, notice := range previous {
		if notice.Class == sandboxpolicy.AccessNoticeClassDegradation {
			hasPreviousDegradation = true
			break
		}
	}
	if hasPreviousDegradation {
		return sandboxpolicy.ReplaceAccessDegradationNotices(
			previous, current...,
		)
	}
	return sandboxpolicy.MergeAccessNotices(current, previous...)
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
