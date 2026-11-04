package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/launchspec"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	current, err = resolveSandboxChainFrom(convID, previous)
	if err != nil {
		return nil, nil, err
	}
	return current, previous, nil
}

// resolveSandboxChainFrom composes the current registry chain for a snapshot's
// recorded provenance: its source group (ResolutionGroupID, or the
// conversation's memberships for a legacy snapshot) and its explicit profile.
// A clone uses it with provenance rebound to the group it joins.
func resolveSandboxChainFrom(convID string, previous *sandboxpolicy.Snapshot) (*sandboxpolicy.Snapshot, error) {
	if previous.ProfilesOmitted {
		return previous, nil
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
	return resolveSandboxChainWith(convID, previous, explicitProfileID, explicitProfileName)
}

// resolveDerivedOmissionChain re-resolves the sandbox-profile tiers of an
// agent whose last launch omitted them only because its mode could not carry
// them (ProfilesOmittedDerived), for a relaunch whose mode can: the source
// group's and the global assignment as they are now, and the profile the
// spawn named explicitly, which the omission did not record.
func resolveDerivedOmissionChain(convID string, previous *sandboxpolicy.Snapshot) (*sandboxpolicy.Snapshot, error) {
	explicitProfileName, err := explicitSandboxProfileAtSpawn(convID)
	if err != nil {
		return nil, err
	}
	resolved, err := resolveSandboxChainWith(convID, previous, 0, explicitProfileName)
	if explicitProfileName != "" && isRelaunchChainUnusable(err) {
		if prof, lookupErr := db.GetSandboxProfile(explicitProfileName); lookupErr != nil || prof != nil {
			return resolved, err
		}
		// The omission meant the name was never resolved, so it may never
		// have existed. A profile the agent never ran under must not make it
		// unresumable: the ambient tiers still apply.
		slog.Warn("relaunch: the sandbox profile named at spawn does not exist; applying the group and global profiles only",
			"conv", convID, "profile", explicitProfileName)
		return resolveSandboxChainWith(convID, previous, 0, "")
	}
	return resolved, err
}

// explicitSandboxProfileAtSpawn is the sandbox profile the agent's spawn
// request named, from its launch layers; "" when none, or when the agent
// predates them.
func explicitSandboxProfileAtSpawn(convID string) (string, error) {
	layers, err := db.AgentLaunchLayersForConv(convID)
	if err != nil || layers == nil {
		return "", err
	}
	raw, ok := layers.Explicit["explicit_sandbox_profile"].Fields["SandboxProfile"]
	if !ok {
		return "", nil
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return "", fmt.Errorf("read the sandbox profile named at spawn: %w", err)
	}
	return strings.TrimSpace(name), nil
}

// resolveSandboxChainWith composes the current registry chain for previous's
// source group and the given explicit profile, by stable ID, else by name.
func resolveSandboxChainWith(convID string, previous *sandboxpolicy.Snapshot, explicitProfileID int64, explicitProfileName string) (*sandboxpolicy.Snapshot, error) {
	var err error
	groupID := previous.ResolutionGroupID
	if groupID == 0 {
		groupID, err = resumeSandboxGroupID(convID)
		if err != nil {
			return nil, err
		}
	}
	var resolved sandboxpolicy.Snapshot
	if explicitProfileID != 0 || explicitProfileName == "" {
		resolved, err = db.ResolveEffectiveSandboxSnapshotByID(groupID, explicitProfileID)
	} else {
		err = db.ErrSandboxProfileNotFound
	}
	if errors.Is(err, db.ErrSandboxProfileNotFound) && explicitProfileName != "" {
		// A deleted explicit profile can be recovered by recreating it under its
		// recorded name. Ordinary renames still follow the stable ID above.
		resolved, err = db.ResolveEffectiveSandboxSnapshot(groupID, explicitProfileName)
	}
	if errors.Is(err, db.ErrSandboxProfileNotFound) {
		return nil, relaunchChainUnusable(fmt.Errorf("the explicit sandbox profile %q used at launch no longer exists; recreate it under that name, then resume again", explicitProfileName))
	}
	if errors.Is(err, db.ErrSandboxProfileUnusable) {
		return nil, relaunchChainUnusable(err)
	}
	if err != nil {
		return nil, err
	}
	// The operator's per-setting group choices (a move that kept a setting on
	// the group it left) outlive every relaunch.
	resolved.GroupBinding = previous.GroupBinding.Clone()
	return &resolved, nil
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
	return resolveResumeSandboxPolicyFrom(convID, sshWorkaround, sshLaunchKey,
		harnessName, harnessBuiltinMode, sandboxImplementation, false)
}

// resolveResumeSandboxPolicyFrom is resolveResumeSandboxPolicy that, with
// useStored, keeps the sandbox-profile contents stored at the last launch
// instead of re-reading the registry. Everything else a relaunch refreshes
// (the group environment, the explicit harness config, the SSH workaround and
// the agent's private directories) is refreshed either way. It is the
// fallback a relaunch takes when the re-read chain cannot be used
// (prepareRelaunchSandbox).
//
// The relaunch policy's group_env reuse row keeps the stored group
// environment instead of reading the group's current one. Its
// sandbox_profile_contents reuse row is decided by the caller
// (prepareRelaunchSandbox), which passes useStored.
func resolveResumeSandboxPolicyFrom(
	convID string,
	sshWorkaround bool,
	sshLaunchKey, harnessName, harnessBuiltinMode, sandboxImplementation string,
	useStored bool,
) (*resumeSandboxPolicy, error) {
	var resolved, previous *sandboxpolicy.Snapshot
	var err error
	relaunchPolicy := currentRelaunchPolicy()
	if useStored {
		previous, err = db.AgentEffectiveSandboxConfigForConv(convID)
		if err == nil && previous != nil {
			stored := unmaterializedAgentDirectories(*previous)
			resolved = &stored
		}
	} else {
		resolved, previous, err = resolveCurrentSandboxChainForConv(convID)
		if err == nil && previous != nil && previous.ProfilesOmittedDerived && previous.ProfilesOmitted &&
			!sandboxProfilesDisabled(harnessName, harnessBuiltinMode, sandboxImplementation) {
			// The omission was the old mode's; this launch's mode can carry
			// the profiles, so they apply again.
			resolved, err = resolveDerivedOmissionChain(convID, previous)
		}
	}
	if err != nil || previous == nil {
		return &resumeSandboxPolicy{Snapshot: previous, Previous: previous}, err
	}
	current, err := composeRelaunchSandbox(convID, *resolved, previous, useStored,
		relaunchReuses(relaunchPolicy, groupEnvField))
	if err != nil {
		return nil, err
	}
	if current.ProfilesOmitted {
		// Either a durable per-agent launch choice, or a mode that still
		// cannot carry profiles; not an empty ambient resolution. Do not let
		// later global/group assignments reappear on resume or reincarnation.
		return &resumeSandboxPolicy{Snapshot: &current, Previous: previous}, nil
	}
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

// composeRelaunchSandbox is what every relaunch (resume, reincarnation and
// clone) lays over the sandbox-profile chain it resolved: the source group's
// current environment, then the explicit --harness-config posture as the
// relaunch policy weighs it (reapplyExplicitHarnessConfig). previous is the
// snapshot the chain was resolved for. stored says resolved is that stored
// snapshot itself rather than a re-read chain: its group environment is kept
// when its group cannot be determined, and its posture is the explicit write,
// so the chain's posture is read back for the policy to weigh
// (withChainHarnessConfig). An omitted-profiles snapshot gets the environment
// only.
//
// With reuseGroupEnv (the relaunch policy's group_env reuse row), it keeps
// the group environment stored at the last launch instead of reading the
// group's current one. Every snapshot stores one.
func composeRelaunchSandbox(convID string, resolved sandboxpolicy.Snapshot, previous *sandboxpolicy.Snapshot, stored, reuseGroupEnv bool) (sandboxpolicy.Snapshot, error) {
	var current sandboxpolicy.Snapshot
	var err error
	if reuseGroupEnv {
		current = reuseStoredGroupEnvironment(resolved, previous)
	} else {
		current, err = refreshResumeGroupEnvironment(convID, resolved, previous)
	}
	if stored && isRelaunchChainUnusable(err) {
		// The stored policy keeps its group environment when its group is
		// the one that could not be determined; any other failure (a read
		// error, a group environment that no longer merges) still fails.
		current, err = resolved, nil
	}
	if err != nil || current.ProfilesOmitted {
		return current, err
	}
	if stored {
		current = withChainHarnessConfig(convID, current, previous)
	}
	applied, err := reapplyExplicitHarnessConfig(convID, current, previous)
	if err != nil {
		// The explicit posture no longer applies to the profiles.
		return sandboxpolicy.Snapshot{}, relaunchChainUnusable(err)
	}
	return applied, nil
}

// withChainHarnessConfig gives a stored snapshot whose harness_config is an
// explicit write the posture the current chain composes, so a relaunch policy
// other than explicit-wins can weigh the two as it does for a re-read chain.
// A chain that cannot be read leaves the stored posture: the stored policy is
// the fallback for exactly that case.
func withChainHarnessConfig(convID string, current sandboxpolicy.Snapshot, previous *sandboxpolicy.Snapshot) sandboxpolicy.Snapshot {
	if explicitHarnessConfig(previous) == "" ||
		currentRelaunchPolicy().Mode(harnessConfigField) == launchspec.ModeExplicitWins {
		return current
	}
	chain, err := resolveSandboxChainFrom(convID, previous)
	if err != nil || chain == nil {
		return current
	}
	current.Effective.HarnessConfig = chain.Effective.HarnessConfig
	current.Effective.Provenance.HarnessConfig = nil
	if source := chain.Effective.Provenance.HarnessConfig; source != nil {
		copied := *source
		current.Effective.Provenance.HarnessConfig = &copied
	}
	return current
}

// refreshResumeGroupEnvironment preserves the birth-time spawn-profile and
// explicit tiers while rebuilding the common environment from the source
// group's current settings. Snapshots written before that split are upgraded
// by treating names currently present in the group as the mutable tier.
func refreshResumeGroupEnvironment(
	convID string,
	current sandboxpolicy.Snapshot,
	previous *sandboxpolicy.Snapshot,
) (sandboxpolicy.Snapshot, error) {
	groupID, explicit := previous.EnvironmentGroupID()
	// No recorded group: recover it from memberships, as the sandbox-profile
	// tier does (resolveSandboxChainFrom), so the two never come from
	// different groups. An omitted-profile agent recorded as ungrouped keeps
	// having no group environment.
	if groupID == 0 && !explicit && (!previous.RefreshGroupEnvironment || !previous.ProfilesOmitted) {
		if previous.ProfilesOmitted {
			// Older omitted-profile snapshots did not retain source-group
			// provenance. Do not guess from later memberships: the agent may now
			// belong to several groups, and omission made their sandbox-profile
			// assignments deliberately irrelevant. New snapshots always record
			// the source group independently of sandbox-profile participation.
			current.LaunchEnvironment = append(
				[]sandboxpolicy.EnvironmentEntry(nil), previous.LaunchEnvironment...)
			return current, nil
		}
		var err error
		groupID, err = resumeSandboxGroupID(convID)
		if err != nil {
			return sandboxpolicy.Snapshot{}, err
		}
	}
	var groupEnvironment []sandboxpolicy.EnvironmentEntry
	if groupID != 0 {
		group, err := db.GetAgentGroupByID(groupID)
		if err != nil {
			return sandboxpolicy.Snapshot{}, fmt.Errorf("load launch group environment: %w", err)
		}
		if group != nil {
			groupEnvironment = group.Environment
		}
	}

	overrides := previous.LaunchEnvironmentOverrides
	if !previous.RefreshGroupEnvironment {
		// Snapshot v13 and older flattened every common environment tier. On the
		// first relaunch after upgrade, current group names become the mutable
		// tier and win that one migration merge; all other frozen names become
		// the higher-precedence overlay. From then on the split is exact.
		groupNames := make(map[string]struct{}, len(groupEnvironment))
		for _, entry := range groupEnvironment {
			groupNames[entry.Name] = struct{}{}
		}
		overrides = make([]sandboxpolicy.EnvironmentEntry, 0, len(previous.LaunchEnvironment))
		for _, entry := range previous.LaunchEnvironment {
			if _, mutable := groupNames[entry.Name]; !mutable {
				overrides = append(overrides, entry)
			}
		}
	}
	environment, err := sandboxpolicy.MergeEnvironment(
		groupEnvironment, overrides)
	if err != nil {
		return sandboxpolicy.Snapshot{}, fmt.Errorf("resolve current group environment: %w", err)
	}
	current.LaunchEnvironment = environment
	current.RefreshGroupEnvironment = true
	current.LaunchEnvironmentOverrides = append(
		[]sandboxpolicy.EnvironmentEntry(nil), overrides...)
	return current, nil
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
			return 0, relaunchChainUnusable(fmt.Errorf("cannot determine the sandbox source group for this multi-group agent after its profile assignments changed; leave it in one launch group or restore an unambiguous assignment, then resume again"))
		}
	}
	if profileID == 0 {
		return 0, nil
	}
	return groups[0].ID, nil
}

// explicitHarnessConfig returns the --harness-config posture a launch
// selected explicitly, read back from its recorded snapshot. The selection
// has no tier of its own: applySpawnHarnessConfig overwrites the composed
// value and clears its provenance, and composition always records provenance
// for a value it chose. So a posture with no provenance is the launch
// contract. An omitted-profiles snapshot never carries one.
func explicitHarnessConfig(previous *sandboxpolicy.Snapshot) string {
	if previous == nil || previous.ProfilesOmitted {
		return ""
	}
	if previous.Effective.HarnessConfig == sandboxpolicy.HarnessConfigAccessDefault ||
		previous.Effective.Provenance.HarnessConfig != nil {
		return ""
	}
	return string(previous.Effective.HarnessConfig)
}

// reapplyExplicitHarnessConfig carries an explicit --harness-config posture
// across a relaunch that rebuilt the policy from the profile chain
// (tcl-bmhpir). Without it a read pin over a write chain widens to write, and
// a write selection over the floor reverts to the floor.
//
// The relaunch policy's harness_config row decides how the explicit posture
// weighs against the chain's (relaunchHarnessConfig): explicit-wins, the
// default, always keeps it; tighten-only lets a chain that keeps the floor
// win over an explicit write; chain-wins takes the chain's. conv names the
// agent whose spawn-time explicit posture to fall back to.
func reapplyExplicitHarnessConfig(conv string, current sandboxpolicy.Snapshot, previous *sandboxpolicy.Snapshot) (sandboxpolicy.Snapshot, error) {
	explicit := relaunchHarnessConfig(currentRelaunchPolicy().Mode(harnessConfigField),
		current.Effective.HarnessConfig, explicitHarnessConfigFor(conv, previous))
	if explicit == "" {
		return current, nil
	}
	applied, fail := applySpawnHarnessConfig(current, explicit)
	if fail != nil {
		return current, fmt.Errorf("reapply the launch's harness_config %q: %s", explicit, fail.Msg)
	}
	return applied, nil
}

// relaunchSandboxRequest is what a resume or reincarnation prepares its
// sandbox for.
type relaunchSandboxRequest struct {
	convID, sshLaunchKey                 string
	sshWorkaround, copilotAPI            bool
	harness, builtinMode, implementation string
	temporaryBuiltinMode                 bool
	transport                            session.ModelTransportLaunchContext
	// revalidate re-checks the composed snapshot against the current host.
	revalidate bool
}

// relaunchSandbox is a prepared relaunch sandbox: the policy to commit, the
// snapshot to launch under, and the warnings to show.
type relaunchSandbox struct {
	policy    *resumeSandboxPolicy
	effective *sandboxpolicy.Snapshot
	warnings  []string
	// changes is the per-field echo of what differs from the policy stored
	// at the last launch (relaunchChanges).
	changes []string
}

// relaunchSandboxRequestFor is the sandbox request for relaunching convID
// with its durable launch config from cwd.
func relaunchSandboxRequestFor(convID, cwd, sshLaunchKey string, cfg *durableRelaunchConfig) relaunchSandboxRequest {
	return relaunchSandboxRequest{
		convID: convID, sshLaunchKey: sshLaunchKey,
		sshWorkaround: cfg.SSHWorkaround, copilotAPI: cfg.CopilotAPI,
		harness: cfg.Harness, builtinMode: cfg.Sandbox, implementation: cfg.activeSandboxImplementation(),
		temporaryBuiltinMode: cfg.TemporaryHarnessBuiltinMode,
		transport: session.ModelTransportLaunchContext{
			Model: cfg.Model, ModelProxy: cfg.ModelProxy, Cwd: cwd},
		revalidate: true,
	}
}

// prepareRelaunchSandbox resolves, validates and checks the sandbox a resume
// or reincarnation launches under. It re-reads the agent's sandbox profiles
// first. When that chain cannot be used (an explicit profile was deleted,
// the agent's groups now assign different profiles, the registry no longer
// composes or validates, or its rules are ones this launch cannot enforce),
// it prepares the same relaunch under the profile contents stored at the
// last launch and says so in a warning: a resume always works. It fails only
// when the stored policy cannot be launched either, and then reports the
// re-read chain's failure.
//
// The relaunch policy's sandbox_profile_contents reuse row swaps the two:
// the stored contents come first, and when they cannot be used the current
// profiles are tried, so a resume still works (reuseStoredProfileContents
// decides, and says when the agent's profiles are now different ones).
func prepareRelaunchSandbox(req relaunchSandboxRequest) (*relaunchSandbox, *spawnFailure) {
	if relaunchReuses(currentRelaunchPolicy(), sandboxProfileContentsField) {
		stored, err := db.AgentEffectiveSandboxConfigForConv(req.convID)
		if err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "io", "read the stored sandbox policy: " + err.Error()}
		}
		reuse, note := reuseStoredProfileContents(req.convID, stored)
		if reuse {
			return prepareReusedRelaunchSandbox(req)
		}
		prepared, fail := prepareRereadRelaunchSandbox(req)
		if prepared != nil && note != "" {
			prepared.warnings = append([]string{note}, prepared.warnings...)
		}
		return prepared, fail
	}
	return prepareRereadRelaunchSandbox(req)
}

// prepareReusedRelaunchSandbox prepares the relaunch under the sandbox
// profile contents stored at the last launch (the reuse row), falling back
// to the current profiles when those cannot be used.
func prepareReusedRelaunchSandbox(req relaunchSandboxRequest) (*relaunchSandbox, *spawnFailure) {
	prepared, fail, unusable := prepareRelaunchSandboxFrom(req, true)
	if fail == nil || !unusable {
		return prepared, fail
	}
	fallback, chainFail, _ := prepareRelaunchSandboxFrom(req, false)
	if chainFail != nil {
		return nil, fail
	}
	fallback.warnings = append([]string{fmt.Sprintf(
		"the sandbox profile contents stored at the last launch could not be applied (%s); "+
			"relaunched under the agent's current sandbox profiles, although the relaunch policy reuses stored contents",
		fail.Msg)}, fallback.warnings...)
	return fallback, nil
}

// prepareRereadRelaunchSandbox is the default: the current profiles, falling
// back to the stored policy.
func prepareRereadRelaunchSandbox(req relaunchSandboxRequest) (*relaunchSandbox, *spawnFailure) {
	prepared, fail, unusable := prepareRelaunchSandboxFrom(req, false)
	if fail == nil {
		return prepared, nil
	}
	if !unusable {
		// A read, io or preparation failure says nothing about whether the
		// re-read profiles are valid, so it must not send the agent back to
		// an older, possibly wider policy.
		return nil, fail
	}
	stored, err := db.AgentEffectiveSandboxConfigForConv(req.convID)
	if err != nil || stored == nil || stored.ProfilesOmitted {
		// Nothing was re-read, so there is nothing to fall back from.
		return nil, fail
	}
	fallback, storedFail, _ := prepareRelaunchSandboxFrom(req, true)
	if storedFail != nil {
		return nil, fail
	}
	fallback.warnings = append([]string{fmt.Sprintf(
		"the agent's sandbox profiles could not be applied (%s); "+relaunchFallbackNote,
		fail.Msg)}, fallback.warnings...)
	return fallback, nil
}

// relaunchSandboxGateFailure is the check every relaunch (resume,
// reincarnation, clone, an applied spawn copy) runs on the sandbox it is
// about to launch under: that the mode and implementation can carry its
// sandbox profiles, the Copilot API loopback rule, and that its access rules
// can be enforced. The loopback rule is re-asked on every relaunch rather
// than trusted from the spawn that admitted it: an operator can close network
// access between two launches, and an API-driven agent relaunched into a
// private network namespace would come back with a channel that can never
// connect. A relaunch has no allow-unenforced override.
func relaunchSandboxGateFailure(harnessName, mode, implementation string, snapshot *sandboxpolicy.Snapshot,
	copilotAPI bool, transport session.ModelTransportLaunchContext) *spawnFailure {
	if fail := sandboxProfileCapabilityFailure(harnessName, mode, snapshot, implementation); fail != nil {
		return fail
	}
	if fail := copilotAPILoopbackFailure(copilotAPI, snapshot, implementation); fail != nil {
		return fail
	}
	_, fail := planSandboxProfileAccessForLaunch(harnessName, mode, snapshot, implementation, transport, false)
	return fail
}

// relaunchFallbackNote ends the warning of a relaunch that fell back to its
// stored sandbox policy. The dashboard's wake toast matches on it to show
// that warning, and only it, as an error.
const relaunchFallbackNote = "relaunched under the sandbox policy stored at its last launch"

// prepareRelaunchSandboxFrom reports, with a failure, whether it means the
// sandbox profiles cannot be used for this launch (unusable), which is what
// a fallback to the stored policy answers.
func prepareRelaunchSandboxFrom(req relaunchSandboxRequest, useStored bool) (*relaunchSandbox, *spawnFailure, bool) {
	policy, err := resolveResumeSandboxPolicyFrom(req.convID, req.sshWorkaround, req.sshLaunchKey,
		req.harness, req.builtinMode, req.implementation, useStored)
	if err != nil {
		return nil, &spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()},
			isRelaunchChainUnusable(err)
	}
	cleanup := func(fail *spawnFailure) *spawnFailure {
		if cleanupErr := cleanupUncommittedResumeSandboxPolicy(policy); cleanupErr != nil {
			fail.Msg += "; remove unused agent-owned directories: " + cleanupErr.Error()
		}
		return fail
	}
	if policy != nil && policy.Snapshot != nil {
		refreshed, refreshErr := finalizeCodexSSHWorkaroundForRelaunch(*policy.Snapshot, policy.SSHWorkaround)
		if refreshErr != nil {
			return nil, cleanup(&spawnFailure{http.StatusInternalServerError, "spawn",
				"prepare SSH workaround: " + refreshErr.Error()}), false
		}
		policy.Snapshot = &refreshed
	}
	var effective *sandboxpolicy.Snapshot
	if policy != nil {
		effective = policy.Snapshot
	}
	if effective != nil && req.revalidate {
		validated, err := sandboxpolicy.RevalidateSnapshot(*effective)
		if err != nil {
			return nil, cleanup(&spawnFailure{http.StatusConflict, "sandbox_profile_changed", err.Error()}), true
		}
		effective = &validated
		policy.Snapshot = effective
	}
	if req.temporaryBuiltinMode {
		effective = temporarySandboxLaunchSnapshot(req.harness, effective)
	}
	if fail := relaunchSandboxGateFailure(req.harness, req.builtinMode, req.implementation,
		effective, req.copilotAPI, req.transport); fail != nil {
		return nil, cleanup(fail), true
	}
	out := &relaunchSandbox{policy: policy, effective: effective}
	if policy != nil {
		out.changes = relaunchChanges(policy.Previous, policy.Snapshot)
	}
	if effective != nil {
		for _, notice := range effective.Effective.AccessNotices {
			out.warnings = append(out.warnings, notice.Detail)
		}
	}
	return out, nil, false
}

// relaunchChainUnusableError marks a re-read sandbox chain that cannot be
// used for this launch (a missing or uncomposable profile, an ambiguous
// group, an explicit posture that no longer applies), keeping its message.
type relaunchChainUnusableError struct{ err error }

func (e relaunchChainUnusableError) Error() string { return e.err.Error() }
func (e relaunchChainUnusableError) Unwrap() error { return e.err }

func relaunchChainUnusable(err error) error { return relaunchChainUnusableError{err} }

func isRelaunchChainUnusable(err error) bool {
	var unusable relaunchChainUnusableError
	return errors.As(err, &unusable)
}
