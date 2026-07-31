package agentd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// describePredictedSandboxProfile adds the launch-policy fields that are
// independent of the network/socket capability table. Environment injection
// and agent-owned directory materialization are agentd launch contracts;
// filesystem enforcement depends on the selected wall and harness.
func describePredictedSandboxProfile(
	profile sandboxpolicy.Profile,
	target parsedSandboxProfileEnforcementTarget,
	validatedBuiltinMode string,
	described harness.PredictedAccessAxes,
) harness.PredictedAccessAxes {
	described.Filesystem = predictSandboxFilesystem(profile, target, validatedBuiltinMode)
	described.Environment = predictSandboxEnvironment(profile, target)
	described.AgentDirectories = predictSandboxAgentDirectories(
		profile, described.Filesystem,
	)
	return described
}

// describePredictedDraftSandboxProfile evaluates filesystem/environment/private
// directory capability against the effective assignment contexts, not just the
// draft in isolation. This catches a carve-out formed across scopes, such as a
// global denied parent plus a narrower group-profile write grant.
// A capability conflict formed in ONE context refuses that context alone. The
// returned refusal slice is index-aligned with the axes slice, and a refused
// index carries the zero PredictedAccessAxes: a refusal is decided before any
// rule is judged, so there is no verdict to place there, and every consumer must
// read the refusal slice before the axes one. The aggregate axes summarize only
// the contexts that survived, so an unrelated context's refusal cannot silently
// darken a clean context's verdict.
func describePredictedDraftSandboxProfile(
	flattened sandboxpolicy.Profile,
	contexts []sandboxProfileEffectiveContext,
	target parsedSandboxProfileEnforcementTarget,
	validatedBuiltinMode string,
	described harness.PredictedAccessAxes,
) (
	harness.PredictedAccessAxes,
	[]harness.PredictedAccessAxes,
	[]*sandboxProfileEnforcementRefusal,
	[][]harness.PredictedNetworkEntry,
	error,
) {
	if len(contexts) == 0 {
		return describePredictedSandboxProfile(
			flattened, target, validatedBuiltinMode, described,
		), nil, nil, nil, nil
	}
	predictions := make([]harness.PredictedAccessAxes, 0, len(contexts))
	refusals := make([]*sandboxProfileEnforcementRefusal, 0, len(contexts))
	networkPredictions := make(
		[][]harness.PredictedNetworkEntry, 0, len(contexts))
	// survivors feeds the aggregate; predictions stays index-aligned with the
	// contexts so a refused context still occupies its own slot.
	survivors := make([]harness.PredictedAccessAxes, 0, len(contexts))
	for _, context := range contexts {
		axes, err := sandboxpolicy.DeriveAccessAxes(context.policy)
		if err != nil {
			return harness.PredictedAccessAxes{}, nil, nil, nil, err
		}
		predicted, err := harness.PredictAccessEnforcement(
			target.harness, target.implementation, axes,
			validatedBuiltinMode, target.platform,
		)
		if err != nil {
			refusal := sandboxProfilePredictionRefusal(err)
			if refusal == nil {
				return harness.PredictedAccessAxes{}, nil, nil, nil, err
			}
			refusals = append(refusals, refusal)
			predictions = append(predictions, harness.PredictedAccessAxes{})
			networkPredictions = append(networkPredictions, nil)
			continue
		}
		networkRows := append([]harness.PredictedNetworkEntry{},
			harness.DescribePredictedNetworkEntries(
				axes.Network, predicted)...)
		networkRows = append(networkRows,
			harness.DescribePredictedNetworkDenyEntries(
				axes.Network.Deny, predicted)...)
		networkPredictions = append(networkPredictions, networkRows)
		contextAxes := describePredictedSandboxProfile(
			context.policy, target, validatedBuiltinMode,
			harness.DescribePredictedAccess(axes, predicted),
		)
		predictions = append(predictions, contextAxes)
		survivors = append(survivors, contextAxes)
		refusals = append(refusals, nil)
	}
	if len(survivors) == 0 {
		// Nothing survived to aggregate. The caller turns an all-refused target
		// into a target-level refusal, so the axes returned here are never read.
		return harness.PredictedAccessAxes{}, predictions, refusals, networkPredictions, nil
	}
	described.Filesystem = aggregateSandboxFeature(
		survivors, func(value harness.PredictedAccessAxes) harness.PredictedAccessAxis {
			return value.Filesystem
		},
	)
	described.Environment = aggregateSandboxFeature(
		survivors, func(value harness.PredictedAccessAxes) harness.PredictedAccessAxis {
			return value.Environment
		},
	)
	described.AgentDirectories = aggregateSandboxFeature(
		survivors, func(value harness.PredictedAccessAxes) harness.PredictedAccessAxis {
			return value.AgentDirectories
		},
	)
	described.Network = aggregateSandboxFeature(
		survivors, func(value harness.PredictedAccessAxes) harness.PredictedAccessAxis {
			return value.Network
		},
	)
	described.UnixSockets = aggregateSandboxFeature(
		survivors, func(value harness.PredictedAccessAxes) harness.PredictedAccessAxis {
			return value.UnixSockets
		},
	)
	return described, predictions, refusals, networkPredictions, nil
}

func predictSandboxFilesystem(
	profile sandboxpolicy.Profile,
	target parsedSandboxProfileEnforcementTarget,
	validatedBuiltinMode string,
) harness.PredictedAccessAxis {
	tier := sandboxFilesystemTier(profile.Filesystem, len(profile.AgentDirectories))
	grants, err := filesystemGrantsWithPredictedAgentDirectories(profile)
	if err != nil {
		return predictedSandboxFeature(
			tier, harness.AccessPredictionRefused,
			"cannot resolve the generated agent-owned directory policy: "+err.Error(),
		)
	}
	reopens := sandboxpolicy.ReopensUnderDeny(grants)
	if len(profile.Filesystem) == 0 && len(profile.AgentDirectories) == 0 {
		return predictedSandboxFeature(
			tier, harness.AccessPredictionEnforced,
			"no directory-access rules are configured",
		)
	}
	// A mount_path rule needs a mount namespace. Report Refused everywhere else
	// BEFORE the per-harness branches, so the preview never shows a cell that
	// reads as enforced for a rule the surface would silently mount at the host
	// path instead.
	remappedCount, remappedDetail := remappedGrantSummary(profile.Filesystem)
	if remappedCount > 0 &&
		!sandboxpolicy.SupportsMountPaths(target.implementation, target.platform) {
		return predictedSandboxFeature(
			tier, harness.AccessPredictionRefused,
			fmt.Sprintf(
				"%d directory rule%s mount%s a host directory at a different sandbox path (%s); "+
					"that needs a mount namespace, which only the Linux tclaude-layer provides, so launch is refused rather than mounting at the host path",
				remappedCount, pluralSuffix(remappedCount),
				singularVerbSuffix(remappedCount), remappedDetail,
			),
		)
	}
	if target.implementation.UsesTclaudeLayer() {
		mechanism := "tclaude-layer Seatbelt"
		if target.platform == "linux" {
			mechanism = "tclaude-layer bubblewrap"
		}
		detail := fmt.Sprintf(
			"%s enforces the directory policy at process scope", mechanism,
		)
		if remappedCount > 0 {
			detail += fmt.Sprintf(
				" and mounts %d host director%s at a different sandbox path (%s)",
				remappedCount, pluralY(remappedCount), remappedDetail,
			)
		}
		if len(reopens) > 0 {
			detail += fmt.Sprintf(
				" and supports %d narrower read/write carve-out%s beneath denied parent directories (%s)",
				len(reopens), pluralSuffix(len(reopens)), describePredictedReopens(reopens),
			)
		}
		return predictedSandboxFeature(tier, harness.AccessPredictionEnforced, detail)
	}

	switch target.harness.Name {
	case harness.DefaultName:
		mode := strings.TrimSpace(validatedBuiltinMode)
		if mode != harness.ClaudeSandboxOn && filesystemHasDeny(grants) {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionRefused,
				fmt.Sprintf(
					"Claude directory rules require sandbox %q; sandbox %q cannot guarantee enforcement",
					harness.ClaudeSandboxOn, validatedBuiltinMode,
				),
			)
		}
		if mode != harness.ClaudeSandboxOn {
			if mode == harness.ClaudeSandboxOff {
				return predictedSandboxFeature(
					tier, harness.AccessPredictionNotEnforced,
					"Claude Code's OS sandbox is off, so tclaude does not emit additive directory grants; the launch still has unrestricted directory access",
				)
			}
			return predictedSandboxFeature(
				tier, harness.AccessPredictionEnforcedPartial,
				"tclaude passes these additive directory grants to Claude Code, but sandbox \"inherit\" lets Claude settings decide whether its OS sandbox applies them; the launch remains allowed because the rules only widen access",
			)
		}
		if len(reopens) > 0 {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionEnforcedPartial,
				fmt.Sprintf(
					"Claude Code's OS sandbox supports %d narrower carve-out%s (%s), "+
						"but a denied parent with a reopen cannot be mirrored to built-in file-tool permissions; "+
						"Read/Write/Edit remain outside that parent deny",
					len(reopens), pluralSuffix(len(reopens)), describePredictedReopens(reopens),
				),
			)
		}
		return predictedSandboxFeature(
			tier, harness.AccessPredictionEnforced,
			"Claude Code's OS sandbox enforces the directory rules; leaf denies are also mirrored to built-in Read/Edit permissions",
		)
	case harness.CodexName:
		if strings.TrimSpace(validatedBuiltinMode) != harness.SandboxManagedProfile {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionRefused,
				fmt.Sprintf(
					"Codex directory rules require sandbox %q; sandbox %q cannot render the managed path policy",
					harness.SandboxManagedProfile, validatedBuiltinMode,
				),
			)
		}
		if len(reopens) > 0 && target.platform == "darwin" {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionRefused,
				fmt.Sprintf(
					"Codex on macOS cannot enforce %d narrower carve-out%s (%s): "+
						"a denied parent mask dominates narrower read/write grants, so launch is refused",
					len(reopens), pluralSuffix(len(reopens)), describePredictedReopens(reopens),
				),
			)
		}
		if len(reopens) > 0 {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionEnforced,
				fmt.Sprintf(
					"Codex's managed Linux profile supports %d narrower carve-out%s (%s) "+
						"only after the launch-time isolated split-policy probe verifies bubblewrap behavior",
					len(reopens), pluralSuffix(len(reopens)), describePredictedReopens(reopens),
				),
			)
		}
		return predictedSandboxFeature(
			tier, harness.AccessPredictionEnforced,
			"Codex's managed permission profile enforces the directory rules",
		)
	case harness.OpenCodeName:
		if strings.TrimSpace(validatedBuiltinMode) == harness.OpenCodeSandboxAccessControl {
			return predictedSandboxFeature(
				tier, harness.AccessPredictionEnforcedPartial,
				"OpenCode represents directory rules as soft, ordered tool access control; it is not OS containment",
			)
		}
	}
	return predictedSandboxFeature(
		tier, harness.AccessPredictionRefused,
		fmt.Sprintf("%s cannot represent sandbox-profile directory rules", target.harness.Name),
	)
}

func predictSandboxEnvironment(
	profile sandboxpolicy.Profile,
	target parsedSandboxProfileEnforcementTarget,
) harness.PredictedAccessAxis {
	count := len(profile.Environment)
	if count == 0 {
		return predictedSandboxFeature(
			"unset", harness.AccessPredictionEnforced,
			"no literal environment variables are configured",
		)
	}
	detail := fmt.Sprintf(
		"tclaude injects %d literal environment variable%s into the harness launch environment inherited by tools; "+
			"these are mutable process configuration, not an access-control boundary",
		count, pluralSuffix(count),
	)
	if target.harness.Name == harness.CodexName {
		detail = fmt.Sprintf(
			"tclaude injects %d literal environment variable%s at launch and pins their initial tool values in Codex "+
				"shell_environment_policy; these are mutable process configuration, not an access-control boundary",
			count, pluralSuffix(count),
		)
	}
	return predictedSandboxFeature(
		fmt.Sprintf("%d variable%s", count, pluralSuffix(count)),
		harness.AccessPredictionEnforced,
		detail,
	)
}

func predictSandboxAgentDirectories(
	profile sandboxpolicy.Profile,
	filesystem harness.PredictedAccessAxis,
) harness.PredictedAccessAxis {
	count := len(profile.AgentDirectories)
	if count == 0 {
		return predictedSandboxFeature(
			"unset", harness.AccessPredictionEnforced,
			"no agent-owned directories are configured",
		)
	}
	tier := fmt.Sprintf("%d director%s", count, pluralY(count))
	if filesystem.Outcome == harness.AccessPredictionRefused {
		return predictedSandboxFeature(
			tier, harness.AccessPredictionRefused,
			"agentd can create the private per-agent directories, but launch is refused because their effective directory policy cannot be enforced: "+
				filesystem.Detail,
		)
	}
	return predictedSandboxFeature(
		tier, harness.AccessPredictionEnforced,
		fmt.Sprintf(
			"agentd creates %d private 0700 writable director%s per agent, injects the environment binding%s, and applies the directory policy above",
			count, pluralY(count), pluralSuffix(count),
		),
	)
}

func filesystemGrantsWithPredictedAgentDirectories(
	profile sandboxpolicy.Profile,
) ([]sandboxpolicy.FilesystemGrant, error) {
	grants := append([]sandboxpolicy.FilesystemGrant(nil), profile.Filesystem...)
	if len(profile.AgentDirectories) == 0 {
		return grants, nil
	}
	cacheDir, err := canonicalizeForSecureMkdir(tclcommon.CacheDir())
	if err != nil {
		return nil, fmt.Errorf("resolve tclaude cache directory: %w", err)
	}
	base := filepath.Join(cacheDir, "agent-dirs", "predicted-agent")
	for _, name := range profile.AgentDirectories {
		grants = append(grants, sandboxpolicy.FilesystemGrant{
			Path: filepath.Join(base, name), Access: sandboxpolicy.AccessWrite,
		})
	}
	return grants, nil
}

func sandboxFilesystemTier(
	grants []sandboxpolicy.FilesystemGrant,
	agentDirectoryCount int,
) string {
	counts := map[sandboxpolicy.Access]int{}
	for _, grant := range grants {
		counts[grant.Access]++
	}
	parts := make([]string, 0, 4)
	for _, access := range []sandboxpolicy.Access{
		sandboxpolicy.AccessRead, sandboxpolicy.AccessWrite, sandboxpolicy.AccessDeny,
	} {
		if counts[access] > 0 {
			parts = append(parts, fmt.Sprintf(
				"%d %s rule%s", counts[access], access, pluralSuffix(counts[access]),
			))
		}
	}
	if agentDirectoryCount > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d generated write root%s", agentDirectoryCount, pluralSuffix(agentDirectoryCount),
		))
	}
	if len(parts) == 0 {
		return "unset"
	}
	return strings.Join(parts, " · ")
}

// remappedGrantSummary describes each remapped rule as "host → sandbox", capped
// so a large profile does not turn one capability cell into a wall of paths.
func remappedGrantSummary(grants []sandboxpolicy.FilesystemGrant) (int, string) {
	const maxShown = 2
	var shown []string
	total := 0
	for _, grant := range grants {
		if !grant.IsRemapped() {
			continue
		}
		total++
		if len(shown) < maxShown {
			shown = append(shown, fmt.Sprintf("%q → %q", grant.Path, grant.GuestPath()))
		}
	}
	if total > maxShown {
		shown = append(shown, fmt.Sprintf("and %d more", total-maxShown))
	}
	return total, strings.Join(shown, ", ")
}

func describePredictedReopens(reopens []sandboxpolicy.ReopenUnderDeny) string {
	const maxShown = 2
	parts := make([]string, 0, maxShown+1)
	for i, reopen := range reopens {
		if i == maxShown {
			parts = append(parts, fmt.Sprintf("and %d more", len(reopens)-maxShown))
			break
		}
		parts = append(parts, fmt.Sprintf(
			"%s %q beneath deny %q",
			reopen.Reopen.Access, reopen.Reopen.Path, reopen.Deny,
		))
	}
	return strings.Join(parts, ", ")
}

func aggregateSandboxFeature(
	predictions []harness.PredictedAccessAxes,
	selectAxis func(harness.PredictedAccessAxes) harness.PredictedAccessAxis,
) harness.PredictedAccessAxis {
	if len(predictions) == 1 {
		return selectAxis(predictions[0])
	}
	worstRank := -1
	worstOutcome := ""
	countAtWorst := 0
	details := map[string]struct{}{}
	for _, prediction := range predictions {
		axis := selectAxis(prediction)
		rank := sandboxPredictionOutcomeRank(axis.Outcome)
		switch {
		case rank > worstRank:
			worstRank = rank
			worstOutcome = axis.Outcome
			countAtWorst = 1
			details = map[string]struct{}{axis.Detail: {}}
		case rank == worstRank:
			countAtWorst++
			details[axis.Detail] = struct{}{}
		}
	}
	orderedDetails := make([]string, 0, len(details))
	for detail := range details {
		orderedDetails = append(orderedDetails, detail)
	}
	sort.Strings(orderedDetails)
	if len(orderedDetails) > 2 {
		orderedDetails = append(orderedDetails[:2], "additional context details omitted")
	}
	return predictedSandboxFeature(
		fmt.Sprintf("%d effective contexts", len(predictions)),
		worstOutcome,
		fmt.Sprintf(
			"%d of %d effective contexts have this worst outcome: %s",
			countAtWorst, len(predictions), strings.Join(orderedDetails, "; "),
		),
	)
}

func sandboxPredictionOutcomeRank(outcome string) int {
	switch outcome {
	case harness.AccessPredictionRefused:
		return 3
	case harness.AccessPredictionNotEnforced:
		return 2
	case harness.AccessPredictionEnforcedPartial:
		return 1
	default:
		return 0
	}
}

func predictedSandboxFeature(tier, outcome, detail string) harness.PredictedAccessAxis {
	return harness.PredictedAccessAxis{Tier: tier, Outcome: outcome, Detail: detail}
}

// singularVerbSuffix is pluralSuffix's mirror: a verb agreeing with a count
// takes its "s" in exactly the case the noun does not.
func singularVerbSuffix(count int) string {
	if count == 1 {
		return "s"
	}
	return ""
}

func pluralSuffix(count int) string {
	if count == 1 {
		return ""
	}
	return "s"
}

func pluralY(count int) string {
	if count == 1 {
		return "y"
	}
	return "ies"
}
