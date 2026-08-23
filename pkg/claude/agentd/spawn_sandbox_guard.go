package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// resolveTclaudeLayerAccessVerdict probes THIS host for the floor the launch
// will actually build. The engine is part of that question rather than a
// decoration on the answer: it maps a filtered posture onto the proxy floor,
// which needs strictly less than the packet gateway, so resolving without it
// would refuse a proxy-engine spawn for a prerequisite that spawn never uses.
var resolveTclaudeLayerAccessVerdict = func(
	harnessName string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
) (harness.LaunchOSSandbox, error) {
	if session.TclaudeLayerUsesServerBoundary(harnessName) {
		_, verdict, err := session.ResolveTclaudeLayerServerForEngine(
			posture, root, engine)
		return verdict, err
	}
	_, verdict, err := session.ResolveTclaudeLayerForEngine(posture, root, engine)
	return verdict, err
}

var resolveTclaudeLayerAccessVerdictWithIdentity = func(
	harnessName string,
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
	engine sandboxpolicy.NetworkEngine,
	preserveCallerIdentity bool,
) (harness.LaunchOSSandbox, error) {
	if !preserveCallerIdentity {
		return resolveTclaudeLayerAccessVerdict(harnessName, posture, root, engine)
	}
	if session.TclaudeLayerUsesServerBoundary(harnessName) {
		_, verdict, err := session.ResolveTclaudeLayerServerForEngineWithIdentity(
			posture, root, engine, true)
		return verdict, err
	}
	_, verdict, err := session.ResolveTclaudeLayerForEngineWithIdentity(
		posture, root, engine, true)
	return verdict, err
}

var probeFilteredNetworkPrerequisite = session.ProbeFilteredNetworkPrerequisite

// spawnSandboxLineageFailure prevents an agent that can spawn peers from
// minting a child with a looser launch sandbox than the caller currently has.
// Humans bypass this check: they are the trust root everywhere else in agentd.
func spawnSandboxLineageFailure(
	parentConvID, childHarness, childHarnessBuiltinMode, childImplementation string,
) *spawnFailure {
	if parentConvID == "" {
		return nil
	}
	parent, err := spawnLineageParentSandbox(parentConvID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io",
			"spawn sandbox guard: " + err.Error()}
	}
	child := spawnLineageSandbox{
		Harness:            harnessOrDefault(childHarness),
		HarnessBuiltinMode: strings.TrimSpace(childHarnessBuiltinMode),
		Implementation:     normalizeLineageImplementation(childImplementation),
	}
	if !spawnSandboxLineageAllowed(parent, child) {
		// Both postures name their IMPLEMENTATION as well as their mode.
		// Without it the message can restate the same pair on both sides —
		// `copilot sandbox "off"` may not spawn a `copilot` child with sandbox
		// `"off"` — which reads as a bug rather than as the two different
		// postures that string spells for this harness.
		message := fmt.Sprintf(
			"agent %s was launched as %s sandbox %q (%s) and may not spawn a %s child with sandbox %q (%s)",
			short8(parentConvID), parent.Harness, parent.HarnessBuiltinMode, parent.Implementation,
			child.Harness, child.HarnessBuiltinMode, child.Implementation)
		if note := lineageParentAuthorityNote(parent); note != "" {
			message += "; " + note
		}
		if remedy := copilotLineageRemedy(parent, child); remedy != "" {
			message += "; " + remedy
		}
		return &spawnFailure{http.StatusForbidden, "sandbox_restricted", message}
	}
	return nil
}

// lineageParentAuthorityNote names implementation-derived parent authority in a
// refusal, so whoever hits it can tell this apart from an ordinary
// child-too-loose refusal.
//
// Without it the sentence reads `agent X was launched as claude sandbox "off"
// (tclaude-layer) and may not spawn ...`, and `off` is the loosest mode the
// matrix has — nothing in the pair explains why anything was refused, so it
// reads as a guard bug. The reason is that the parent's authority comes from its
// implementation: under tclaude's own wall the harness's inner sandbox is
// deliberately stood down, and the recorded mode names that stand-down rather
// than an unconfined agent.
func lineageParentAuthorityNote(parent spawnLineageSandbox) string {
	normalized, ok := normalizeSpawnLineageSandbox(parent)
	if !ok {
		return ""
	}
	authority := lineageConfinementMode(normalized)
	if authority == normalized.HarnessBuiltinMode {
		return ""
	}
	return fmt.Sprintf(
		"the parent's authority is derived from its sandbox implementation, not its recorded mode: under %s the harness's own sandbox is stood down because tclaude's wall confines the agent, so it delegates as %s sandbox %q rather than as %q",
		normalized.Implementation, normalized.Harness, authority, normalized.HarnessBuiltinMode)
}

// copilotLineageRemedy names the way out of a Copilot refusal the caller can
// act on, because the posture pair alone does not say which value to change.
// Copilot enters the matrix in exactly one launch pair while the spawn defaults
// land elsewhere, so a refused Copilot child is usually a defaulted request
// rather than a request for something the matrix will never grant.
//
// It is withheld whenever THIS parent would refuse the admitted pair too —
// a Codex workspace-write parent, an unproven Copilot parent, a row that does
// not normalize — because there the refusal is about the caller's own posture
// and no child-side flag moves it. That is asked of the matrix rather than
// enumerated, so the advice is true by construction and cannot drift out of
// sync with the arms; it also subsumes the child that already spells the pair.
func copilotLineageRemedy(parent, child spawnLineageSandbox) string {
	if child.Harness != harness.CopilotName {
		return ""
	}
	admitted := spawnLineageSandbox{
		Harness:            harness.CopilotName,
		HarnessBuiltinMode: harness.CopilotSandboxOff,
		Implementation:     sandboxpolicy.ImplementationTclaudeLayer,
	}
	if !spawnSandboxLineageAllowed(parent, admitted) {
		return ""
	}
	return fmt.Sprintf(
		"%s agents are admitted in exactly one launch topology — pass sandbox_implementation=%s (`--sandbox-impl %s`; mode resolves to %q)",
		harness.CopilotName, sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ImplementationTclaudeLayer, harness.CopilotSandboxOff)
}

func sandboxProfileCapabilityFailure(
	harnessName, harnessBuiltinMode string,
	snapshot *sandboxpolicy.Snapshot,
	sandboxImplementation ...string,
) *spawnFailure {
	rawImplementation := ""
	if len(sandboxImplementation) > 0 {
		rawImplementation = sandboxImplementation[0]
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "invalid_sandbox_implementation", err.Error()}
	}
	h, err := harness.ResolveSpawnable(harnessOrDefault(harnessName))
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	if _, err := harness.ResolveNativeHarnessBuiltinMode(
		h, harnessBuiltinMode, implementation); err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "invalid_sandbox", err.Error()}
	}
	if snapshot == nil {
		return nil
	}
	filesystem, err := sandboxpolicy.FilesystemForLaunch(snapshot.Effective)
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem", err.Error()}
	}
	// Mount paths need a real mount namespace, which only the Linux tclaude-layer
	// has. The launch boundary refuses too, but doing it here as well means the
	// operator gets the named capability back from the spawn API instead of
	// watching the pane die (TCL-866).
	if err := sandboxpolicy.ValidateMountPathSupport(
		filesystem, implementation, runtime.GOOS,
	); err != nil {
		return &spawnFailure{
			http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_mount_path", err.Error()}
	}
	if implementation.UsesTclaudeLayer() {
		// The outer applier, not the harness-native sandbox catalog, represents
		// filesystem and network policy. The session boundary validates the
		// layer's network transport assertion and renders the mount plan.
		return nil
	}
	if implementation.OmitsOSConfinement() {
		// Every gate below asks whether the harness's own sandbox can REPRESENT
		// a rule, and answers by refusing when it cannot. That question is not
		// posed here: this implementation has already stood every access
		// boundary down on purpose, so no rule is represented and none is
		// claimed to be.
		//
		// Refusing would make the implementation unreachable rather than safe.
		// The resolved snapshot is the whole chain — global, group, then the
		// explicit profile — and it must keep resolving because
		// `resource_limits` travel in it. An operator whose global profile
		// carries any network rule (or, on Codex/OpenCode, any filesystem rule)
		// would otherwise be unable to launch a resource-only agent at all,
		// without authoring a second limits-only chain for the purpose.
		//
		// The rules are inert, not honored: planSandboxProfileAccessForLaunch
		// records an explicit not-enforced notice, and the access-enforcement
		// table reports None on every axis. Mount paths are deliberately still
		// refused above — a remapped grant needs a mount namespace, and leaving
		// the authored sandbox path empty is a different failure from a rule
		// that simply does not apply.
		return nil
	}
	// The capability gate runs FIRST and unconditionally: a reopen-under-deny
	// shape must be refused by a harness that cannot enforce it even when the
	// profile carries no other rules. Approximating it would hand the operator a
	// false guarantee.
	//
	// The shape is read from the LAUNCH filesystem rather than the raw effective
	// set, so a deny/reopen pair that is inactive this launch (missing path) is
	// judged exactly as it will be rendered.
	if err := harness.ValidateSandboxReopenUnderDeny(harnessOrDefault(harnessName), harnessBuiltinMode, filesystem); err != nil {
		return sandboxCapabilitySpawnFailure(err, harness.SandboxCapabilityReopenUnderDeny)
	}
	hasNetworkPolicy := snapshot.Effective.Network == nil &&
		snapshot.Effective.UnixSockets == nil &&
		snapshot.Effective.NetworkAccess != sandboxpolicy.NetworkAccessInherit
	if len(filesystem) == 0 && len(snapshot.Effective.AgentDirectories) == 0 && !hasNetworkPolicy {
		return nil
	}
	switch harnessOrDefault(harnessName) {
	case harness.DefaultName:
		if hasNetworkPolicy {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network",
				"Claude launches cannot represent sandbox profile network access; use the Codex managed sandbox"}
		}
		if strings.TrimSpace(harnessBuiltinMode) != harness.ClaudeSandboxOn && filesystemHasDeny(filesystem) {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
				fmt.Sprintf("Claude filesystem deny rules require sandbox %q; sandbox %q cannot guarantee enforcement", harness.ClaudeSandboxOn, harnessBuiltinMode)}
		}
		return nil
	case harness.CodexName:
		if strings.TrimSpace(harnessBuiltinMode) == harness.SandboxManagedProfile {
			if hasNetworkPolicy {
				if err := harness.ValidateCodexAgentNetworkAccess(snapshot.Effective.NetworkAccess); err != nil {
					return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error()}
				}
			}
			return nil
		}
		if hasNetworkPolicy {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network",
				fmt.Sprintf("Codex network rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, harnessBuiltinMode)}
		}
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("Codex filesystem rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, harnessBuiltinMode)}
	case harness.OpenCodeName:
		if strings.TrimSpace(harnessBuiltinMode) == harness.OpenCodeSandboxAccessControl {
			// OpenCode represents these as ordered, per-session tool rules.
			// NetworkAccess gates webfetch/websearch only; it is intentionally
			// not described as process-level network isolation.
			return nil
		}
		kind := "unsupported_sandbox_profile_filesystem"
		detail := "filesystem"
		if len(filesystem) == 0 && len(snapshot.Effective.AgentDirectories) == 0 && hasNetworkPolicy {
			kind = "unsupported_sandbox_profile_network"
			detail = "tool-level network"
		}
		return &spawnFailure{http.StatusUnprocessableEntity, kind,
			fmt.Sprintf("OpenCode sandbox %q cannot represent sandbox profile %s rules; use %q",
				harnessBuiltinMode, detail, harness.OpenCodeSandboxAccessControl)}
	default:
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("harness %q cannot represent sandbox filesystem rules", harnessName)}
	}
}

func planSandboxProfileAccessForLaunch(
	harnessName, harnessBuiltinMode string,
	snapshot *sandboxpolicy.Snapshot,
	rawImplementation string,
	modelContext session.ModelTransportLaunchContext,
	allowUnenforcedSandbox bool,
) ([]sandboxpolicy.AccessNotice, *spawnFailure) {
	if snapshot == nil {
		return nil, nil
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_implementation", err.Error()}
	}
	resourceNotices := []sandboxpolicy.AccessNotice{}
	if err := sandboxpolicy.ValidateResourceLimitTarget(
		snapshot.Effective.ResourceLimits, implementation, runtime.GOOS,
	); err != nil {
		if !allowUnenforcedSandbox {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"unsupported_sandbox_profile_resource_limits", err.Error()}
		}
		resourceNotices = append(resourceNotices, sandboxpolicy.AccessNotice{
			Class:  sandboxpolicy.AccessNoticeClassDegradation,
			Axis:   "resource_limits",
			Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
			Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
			Detail: "the human operator used the dashboard launch override; configured CPU and memory limits are not enforced: " + err.Error(),
		})
		snapshot.Effective.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
			snapshot.Effective.AccessNotices, resourceNotices...)
	}
	if snapshot.Effective.FilesystemRoot == sandboxpolicy.FilesystemRootSeparate {
		h, resolveErr := harness.Resolve(harnessOrDefault(harnessName))
		if resolveErr != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"unsupported_sandbox_profile_access", resolveErr.Error()}
		}
		if rootErr := harness.ValidateExplicitFilesystemRoot(
			h, implementation, snapshot.Effective.FilesystemRoot, runtime.GOOS,
		); rootErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				rootErr, harness.SandboxCapabilityFilesystemRoot)
		}
	}
	if notice, ok := sandboxpolicy.UnconfinedAccessRulesNotice(
		implementation, snapshot.Effective,
	); ok {
		resourceNotices = append(resourceNotices, notice)
		snapshot.Effective.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
			snapshot.Effective.AccessNotices, resourceNotices...)
		return resourceNotices, nil
	}
	if snapshot.Effective.Network == nil && snapshot.Effective.UnixSockets == nil &&
		snapshot.Effective.FilesystemRoot != sandboxpolicy.FilesystemRootSeparate {
		return resourceNotices, nil
	}
	h, err := harness.Resolve(harnessOrDefault(harnessName))
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_access", err.Error()}
	}
	axes, err := sandboxpolicy.EffectiveAccessAxes(snapshot.Effective)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_profile", err.Error()}
	}
	requestedNetworkPosture, err := sandboxpolicy.NetworkPostureForRules(axes.Network)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_profile", err.Error()}
	}
	var verdict harness.LaunchOSSandbox
	var filteredProbe *session.FilteredNetworkPrerequisite
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin {
		// Claude's durable "inherit" mode deliberately does not say whether the
		// native sandbox is enabled: the current managed/project/user settings do.
		// Resolve that launch-time verdict here just as session new does. Without
		// it, daemon preflight mistakes every inherited builtin sandbox for
		// "unconfigured" and blocks spawn/resume before the real launch can pick
		// up an operator's settings change.
		verdict = harness.ResolveLaunchOSSandbox(
			h, harnessBuiltinMode, "", modelContext.Cwd)
	}
	if implementation.UsesTclaudeLayer() {
		posture := sandboxpolicy.NetworkHostOpen
		if requestedNetworkPosture == sandboxpolicy.NetworkIsolatedWithAgentd {
			posture = sandboxpolicy.NetworkIsolatedWithAgentd
		} else if requestedNetworkPosture == sandboxpolicy.NetworkFiltered &&
			implementation == sandboxpolicy.ImplementationTclaudeLayer {
			if runtime.GOOS == "darwin" &&
				sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network) {
				posture = sandboxpolicy.NetworkFiltered
			} else if runtime.GOOS == "linux" {
				if session.ProxyEngineFloorApplies(axes.Network) {
					// TCL-883: the proxy floor's own spawn-time gate. The probe
					// in the other branch is the packet gateway's — pasta, nft,
					// and the namespace privileges they need — none of which a
					// proxy-engine launch ever calls. Gating on it would widen a
					// proxy-engine profile to open on precisely the hosts §2.5
					// says this engine is for. The gate that does apply is the
					// posture-exact verdict below, which maps the proxy floor
					// onto the isolated posture and probes bubblewrap plus
					// pidfd; a host that cannot build that floor is refused
					// there, not silently unfiltered here.
					posture = sandboxpolicy.NetworkFiltered
				} else {
					probe := probeFilteredNetworkPrerequisite()
					filteredProbe = &probe
					if supportErr := session.ValidateFilteredNetworkHarnessSupport(
						h, implementation, axes, probe,
					); supportErr != nil {
						return nil, sandboxCapabilitySpawnFailure(
							supportErr, "unsupported_sandbox_profile_access")
					}
					if probe.Detected {
						posture = sandboxpolicy.NetworkFiltered
					}
				}
			}
		}
		// This verdict is one of the ladder's own inputs, so the socket axis is
		// still the authored one. Ask the same gate the ladder will: on a
		// target where the socket-driven constructed root does not apply, the
		// axis is about to be widened away and the verdict must not describe a
		// root the launch will never build.
		sockets := axes.UnixSockets.Mode
		if !harness.SupportsHostOpenConstructedRoot(
			h, implementation, axes, runtime.GOOS) {
			sockets = sandboxpolicy.AccessModeUnset
		}
		root := sandboxpolicy.RootPostureForMode(
			posture, sockets, axes.FilesystemRoot)
		deployedEngine, engineErr :=
			sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
		if engineErr != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"invalid_sandbox_profile", engineErr.Error()}
		}
		verdict, err = resolveTclaudeLayerAccessVerdictWithIdentity(
			h.Name, posture, root, deployedEngine,
			posture == sandboxpolicy.NetworkFiltered &&
				deployedEngine == sandboxpolicy.NetworkEnginePacket)
		if err != nil {
			// Reached from spawn, clone, reincarnate and relaunch — all of
			// which refuse here on a LIVE host-capability failure, so all of
			// them must resume the disclosure's presence checking.
			return nil, sandboxImplementationUnavailable(err.Error())
		}
	}
	caps, err := harness.ResolveAccessEnforcement(
		h, implementation, axes, verdict, harnessBuiltinMode,
	)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_access", err.Error()}
	}
	rendered, notices, err := harness.PlanAccessEnforcement(
		axes, caps, harness.AccessEnforcementOptions{
			AllowUnenforcedNetworkClosed: allowUnenforcedSandbox,
			AllowReducedNetworkDeny:      allowUnenforcedSandbox,
		},
	)
	notices = append(resourceNotices, notices...)
	if err != nil {
		return nil, sandboxCapabilitySpawnFailure(
			err, "unsupported_sandbox_profile_access")
	}
	renderedNetworkPosture, err := sandboxpolicy.NetworkPostureForRules(rendered.Network)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_profile", err.Error()}
	}
	if implementation.UsesTclaudeLayer() &&
		requestedNetworkPosture == sandboxpolicy.NetworkFiltered &&
		session.FilteredNetworkPrerequisiteNoticeApplies(axes.Network) &&
		(runtime.GOOS == "linux" ||
			(runtime.GOOS == "darwin" &&
				!sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network))) {
		if filteredProbe == nil {
			probe := probeFilteredNetworkPrerequisite()
			filteredProbe = &probe
		}
		notices = append(notices, session.FilteredNetworkPrerequisiteNotice(
			*filteredProbe,
			renderedNetworkPosture == sandboxpolicy.NetworkFiltered &&
				verdict.FilteredNetwork,
		))
	}
	if implementation.UsesTclaudeLayer() {
		// Runs for EVERY tclaude-layer spawn, not only the filtered ones below:
		// a harness whose own sandbox tclaude cannot switch off has to have its
		// posture verified whatever the network policy is, or a host-open
		// tclaude-layer spawn would silently stack two filesystem boundaries.
		if err := session.ValidateTclaudeLayerHarnessPosture(
			h, snapshot.Effective.Environment, modelContext.ExtraArgs,
		); err != nil {
			return nil, sandboxCapabilitySpawnFailure(
				err, harness.SandboxCapabilityCopilotInnerSandbox)
		}
	}
	if implementation.UsesTclaudeLayer() &&
		renderedNetworkPosture == sandboxpolicy.NetworkFiltered {
		plannedEffective := snapshot.Effective
		plannedEffective.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
			plannedEffective.AccessNotices, notices...,
		)
		modelContext.Environment = plannedEffective.Environment
		// §7.4, disclosed from the same pre-injection environment the gate
		// below inspects: what the launcher discards is exactly what the
		// resolver saw, so the two cannot describe different launches. The
		// posture handed in is the RENDERED one, so a policy that widened away
		// from filtered is not disclosed as if a proxy still ran for it.
		if noProxyNotice := session.ProxyEngineNoProxyOverrideNotice(
			runtime.GOOS, implementation, renderedNetworkPosture,
			rendered.Network, plannedEffective.Environment,
		); noProxyNotice != nil {
			notices = append(notices, *noProxyNotice)
		}
		resolvedModel := harness.ResolvedModelTransport{}
		if !sandboxpolicy.NetworkRulesArePrivateRoutedOpen(rendered.Network) {
			var resolveModelErr error
			resolvedModel, resolveModelErr = session.ResolveTclaudeLayerModelTransport(
				h, modelContext)
			if resolveModelErr != nil {
				return nil, sandboxCapabilitySpawnFailure(
					session.AnnotateDenyDrivenFilteredModelTransport(
						rendered.Network, resolveModelErr),
					harness.SandboxCapabilityModelTransport)
			}
		}
		modelNotices, modelErr := session.ValidateTclaudeLayerNetwork(
			h, plannedEffective, resolvedModel,
		)
		if modelErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				session.AnnotateDenyDrivenFilteredModelTransport(
					rendered.Network, modelErr),
				harness.SandboxCapabilityModelTransport)
		}
		notices = append(notices, modelNotices...)
	}
	materialization, err := sandboxpolicy.PrepareUnixSocketLaunch(rendered.UnixSockets)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_profile", err.Error()}
	}
	launchNotices := []sandboxpolicy.AccessNotice{}
	if launchNotice := sandboxpolicy.UnixSocketLaunchNotice(materialization); launchNotice != nil {
		launchNotices = append(launchNotices, *launchNotice)
	}
	snapshot.Effective.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
		snapshot.Effective.AccessNotices, notices...,
	)
	sandboxpolicy.SetUnixSocketLaunchMaterialization(snapshot, materialization)
	return append(notices, launchNotices...), nil
}

// sandboxProfilesDisabled reports launch modes whose explicit contract omits
// every tclaude sandbox-profile tier. Codex danger-full-access uses the raw
// --sandbox opt-out, which cannot be combined with the managed permission
// profile that renders filesystem rules. An explicit implementation=off is a
// harness-neutral opt-out and therefore omits every sandbox-profile tier,
// including for OpenCode.
func sandboxProfilesDisabled(
	harnessName, harnessBuiltinMode string,
	sandboxImplementation ...string,
) bool {
	if len(sandboxImplementation) > 0 {
		implementation, err := sandboxpolicy.NormalizeImplementation(sandboxImplementation[0])
		if err == nil && implementation == sandboxpolicy.ImplementationResourceOnly {
			// resource-only resolves the harness's no-confinement mode, which
			// for Codex is danger-full-access — the very mode the switch below
			// treats as a profile opt-out. Omitting the tiers here would
			// discard the resolved resource_limits along with them and leave
			// the implementation enforcing nothing at all, so it must answer
			// before the mode is consulted. Its access rules are still not
			// enforced; the access-enforcement table discloses that as None.
			return false
		}
		if err == nil && implementation == sandboxpolicy.ImplementationOff {
			return true
		}
	}
	switch harnessOrDefault(harnessName) {
	case harness.CodexName:
		return strings.TrimSpace(harnessBuiltinMode) == harness.SandboxDangerFull
	default:
		return false
	}
}

func filesystemHasDeny(filesystem []sandboxpolicy.FilesystemGrant) bool {
	for _, grant := range filesystem {
		if grant.Access == sandboxpolicy.AccessDeny {
			return true
		}
	}
	return false
}

// sandboxCapabilitySpawnFailure converts an adapter capability refusal into
// the daemon's typed HTTP failure, preserving the adapter's stable error kind
// so the CLI and dashboard can render the specific remedy.
func sandboxCapabilitySpawnFailure(err error, fallbackKind string) *spawnFailure {
	var capErr *harness.SandboxCapabilityError
	if errors.As(err, &capErr) {
		return &spawnFailure{http.StatusUnprocessableEntity, capErr.Kind, capErr.Message}
	}
	return &spawnFailure{http.StatusUnprocessableEntity, fallbackKind, err.Error()}
}

type spawnLineageSandbox struct {
	Harness string
	// HarnessBuiltinMode is what the HARNESS ITSELF does about sandboxing —
	// the value behind the `--sandbox` flag and the `sandbox_mode` column.
	// It is deliberately not called `Mode`: `off` reads as "no sandbox" to any
	// reasonable reader, when all it ever meant is "the harness's own wall is
	// down". Reading it as the former is exactly the misclassification TCL-991
	// fixed. Nothing here is a complete posture without Implementation.
	HarnessBuiltinMode string
	// Implementation is WHO owns the OS wall for this launch. It is carried
	// because the harness-builtin mode alone stopped being a complete posture
	// once tclaude-layer started forcing one: a Claude child launched under
	// tclaude's own wall records mode `off`, which names the harness's inner
	// wall being deliberately stood down, not an unconfined agent.
	Implementation sandboxpolicy.Implementation
}

// normalizeLineageImplementation maps a persisted or requested implementation
// onto the lineage vocabulary. A blank value is the legacy/default record —
// every row written before the axis existed, and every launch that never chose
// — and means the harness owns its own sandbox, which is exactly
// harness-builtin. An unparseable value degrades the same way rather than
// failing the guard open: the launch boundaries validate this field, so a value
// that reaches here unrecognized is a stale row, not a live escalation.
func normalizeLineageImplementation(raw string) sandboxpolicy.Implementation {
	implementation, err := sandboxpolicy.NormalizeImplementation(raw)
	if err != nil {
		return sandboxpolicy.ImplementationHarnessBuiltin
	}
	return implementation
}

func spawnLineageParentSandbox(convID string) (spawnLineageSandbox, error) {
	row, err := db.FindSessionByConvID(convID)
	if err != nil {
		return spawnLineageSandbox{}, err
	}
	if row == nil {
		// A real daemon caller should have a live session row. Tests and very old
		// rows can lack one, so degrade to the default Claude/inherit posture
		// instead of treating "unknown" as full access.
		return spawnLineageSandbox{
			Harness:            harness.DefaultName,
			HarnessBuiltinMode: harness.ClaudeSandboxInherit,
			Implementation:     sandboxpolicy.ImplementationHarnessBuiltin,
		}, nil
	}
	h := harnessOrDefault(row.Harness)
	// row.HarnessBuiltinMode already carries the harness-builtin vocabulary; the
	// column behind it is still spelled `sandbox_mode`, and that mapping is
	// confined to pkg/claude/common/db (TCL-1023).
	builtinMode := strings.TrimSpace(row.HarnessBuiltinMode)
	if h == harness.DefaultName && builtinMode == "" {
		// Old Claude rows and the test simulator used "" for "settings.json
		// decides"; in the lineage matrix that is Claude's inherit sentinel.
		builtinMode = harness.ClaudeSandboxInherit
	}
	return spawnLineageSandbox{
		Harness:            h,
		HarnessBuiltinMode: builtinMode,
		Implementation:     normalizeLineageImplementation(row.SandboxImplementation),
	}, nil
}

func spawnSandboxLineageAllowed(parent, child spawnLineageSandbox) bool {
	parent, parentOK := normalizeSpawnLineageSandbox(parent)
	child, childOK := normalizeSpawnLineageSandbox(child)
	if !parentOK || !childOK {
		return false
	}
	// Both sides are classified by the confinement their launch actually has.
	// The parent side is the authority question — how much may this agent
	// delegate — and reading its mode alone credited a tclaude-walled parent
	// with the authority of a genuinely unconfined one (TCL-991).
	parent.HarnessBuiltinMode = lineageConfinementMode(parent)
	child.HarnessBuiltinMode = lineageConfinementMode(child)

	if parent.Harness == harness.DefaultName {
		switch parent.HarnessBuiltinMode {
		case harness.ClaudeSandboxOff:
			return true
		case harness.ClaudeSandboxInherit:
			return childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				copilotProvenLineageLaunch(child)
		case harness.ClaudeSandboxOn:
			return childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				copilotProvenLineageLaunch(child)
		}
	}

	if parent.Harness == harness.CodexName {
		switch parent.HarnessBuiltinMode {
		case harness.SandboxDangerFull:
			return true
		case harness.SandboxManagedProfile:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn) ||
				copilotProvenLineageLaunch(child)
		case harness.SandboxWorkspaceWrite:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite)
		case harness.SandboxReadOnly:
			return childIsCodex(child, harness.SandboxReadOnly)
		}
	}
	if parent.Harness == harness.OpenCodeName {
		switch parent.HarnessBuiltinMode {
		case harness.OpenCodeSandboxOff:
			return true
		case harness.OpenCodeSandboxTclaudeLayer:
			return child.Harness == harness.OpenCodeName && child.HarnessBuiltinMode == harness.OpenCodeSandboxTclaudeLayer ||
				childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				copilotProvenLineageLaunch(child)
		case harness.OpenCodeSandboxAccessControl:
			return child.Harness == harness.OpenCodeName &&
				harnessBuiltinModeIn(child.HarnessBuiltinMode, harness.OpenCodeSandboxAccessControl, harness.OpenCodeSandboxTclaudeLayer) ||
				childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				copilotProvenLineageLaunch(child)
		}
	}

	// A Copilot PARENT is classified by its persisted pair, not by its mode
	// alone. Every parent arm now is — lineageConfinementMode runs over the
	// parent as well (TCL-991) — but Copilot's rule stays HERE rather than
	// joining that remap, for the structural reason spelled out in the
	// normalization arm below: Copilot has no second confinement class to map
	// onto, so its pair has to gate admission to the matrix outright.
	//
	// It has to be read here because Copilot's `off` mode is ambiguous on its
	// own: as a harness-builtin row it means "Copilot's own experimental wall is
	// asserted down and NOTHING replaced it", which is an unconfined agent; as a
	// tclaude-layer row it means the outer wall is enforcing. Only the second may
	// delegate anything, so an unproven Copilot row falls through to the closing
	// refusal below.
	//
	// The outbound set is the confined children whose containment the outer wall
	// already covers. It excludes every fully-open child (Claude `off`/`inherit`,
	// Codex danger-full-access) and every OpenCode child: OpenCode is the one
	// harness tclaude's layer confines through a server boundary rather than by
	// wrapping the pane, and no equivalence between that topology and this one
	// has been proven. Inventing one here would be a containment claim backed by
	// nothing.
	if copilotProvenLineageLaunch(parent) {
		return copilotProvenLineageLaunch(child) ||
			childIsClaude(child, harness.ClaudeSandboxOn) ||
			childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
	}
	return false
}

// copilotProvenLineageLaunch reports the single Copilot launch pair this
// project has reviewed for detached agent-to-agent spawning: tclaude's own OS
// wall (`tclaude-layer`) with Copilot's command sandbox asserted off.
//
// The implementation comparison is EXACT rather than UsesTclaudeLayer(), which
// would also accept `stacked`. Stacked runs Copilot's own experimental MXC
// policy nested inside tclaude's, so the effective confinement is the
// unreviewed intersection of two policies while the row names one — the very
// shape the launch boundary refuses for this harness.
func copilotProvenLineageLaunch(s spawnLineageSandbox) bool {
	return s.Harness == harness.CopilotName &&
		s.HarnessBuiltinMode == harness.CopilotSandboxOff &&
		s.Implementation == sandboxpolicy.ImplementationTclaudeLayer
}

// lineageConfinementMode maps a launch — either side of the relation — onto the
// confinement class the lineage matrix below reasons in.
//
// It exists because ResolveSandboxImplementationMode now forces the
// harness-native mode for a `tclaude-layer` launch, and that forced mode is the
// harness's own no-confinement spelling — Claude `off`, Codex
// `danger-full-access`. Read as a bare mode, those name the loosest posture in
// the matrix. Read with the implementation beside them, they name the opposite:
// the harness's inner wall is deliberately stood down BECAUSE tclaude's own
// wall is the one enforcing, so the launch is confined at least as tightly as
// the harness-builtin sandboxed class it maps to.
//
// Claude tclaude-layer therefore classifies as Claude `on`, and Codex
// tclaude-layer as the Codex managed profile. OpenCode needs no arm — its
// native `tclaude-layer` mode is already the matrix's own name for this
// topology.
//
// On the CHILD side this does NOT leave every verdict where it was, and the
// difference is measured rather than asserted: 19 request shapes move, 3
// tightened and 16 loosened, enumerated as an exact list by
// TestSandboxLineageTclaudeLayerVerdictDelta. The tightened three refuse a
// Codex read-only / workspace-write parent a tclaude-walled child, because that
// child launches danger-full-access inside a wall that writes its cwd subtree.
// The loosened sixteen accept request shapes whose inner mode is inert under
// that wall, so they mint a session row the parent could already mint by
// spelling the request differently — a property asserted over every one of them
// by TestSandboxLineageTclaudeLayerLooseningGrantsNoNewLaunch.
//
// It applies to the PARENT side too, and there it only ever TIGHTENS: a
// tclaude-layer parent used to be read as the fully-open class its forced mode
// spells, which credited a confined agent with the authority to mint children
// it has no containment claim over (TCL-991). The moved verdicts are enumerated
// exactly by TestSandboxLineageParentAuthorityVerdictDelta, over every
// (harness, mode, implementation) triple a session row can actually hold.
//
// Only the exact `tclaude-layer` implementation enters here. `stacked` runs the
// harness's own sandbox nested inside tclaude's, so its mode still means what
// the matrix thinks it means and must not be reinterpreted — and in practice a
// stacked launch already records the confined mode (stackedSandboxLaunchMode
// forces Claude `on` / the Codex managed profile), so there is nothing here to
// remap for it. An implementation of `off` records the same no-confinement mode
// a tclaude-layer launch does, and there it is the literal truth: nothing is
// enforcing, and the mode must keep its face value.
func lineageConfinementMode(s spawnLineageSandbox) string {
	if s.Implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return s.HarnessBuiltinMode
	}
	switch s.Harness {
	case harness.DefaultName:
		if s.HarnessBuiltinMode == harness.ClaudeSandboxOff {
			return harness.ClaudeSandboxOn
		}
	case harness.CodexName:
		if s.HarnessBuiltinMode == harness.SandboxDangerFull {
			return harness.SandboxManagedProfile
		}
	}
	return s.HarnessBuiltinMode
}

func normalizeSpawnLineageSandbox(s spawnLineageSandbox) (spawnLineageSandbox, bool) {
	s.Harness = harnessOrDefault(s.Harness)
	s.HarnessBuiltinMode = strings.TrimSpace(s.HarnessBuiltinMode)
	switch s.Harness {
	case harness.DefaultName:
		if s.HarnessBuiltinMode == "" {
			s.HarnessBuiltinMode = harness.ClaudeSandboxInherit
		}
		switch s.HarnessBuiltinMode {
		case harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn, harness.ClaudeSandboxOff:
			return s, true
		}
	case harness.CodexName:
		switch s.HarnessBuiltinMode {
		case harness.SandboxManagedProfile, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxDangerFull:
			return s, true
		}
	case harness.OpenCodeName:
		switch s.HarnessBuiltinMode {
		case harness.OpenCodeSandboxAccessControl, harness.OpenCodeSandboxTclaudeLayer, harness.OpenCodeSandboxOff:
			return s, true
		}
	case harness.CopilotName:
		// Copilot enters the matrix in exactly ONE launch pair, and the rule
		// lives HERE rather than in a child arm for a structural reason: three
		// parent arms below return true for every child, so a rule expressed as
		// an arm could never constrain them. Normalization runs ahead of all of
		// them and is the only place that governs the parent and child sides
		// with one statement — which is what this policy is, since the pair a
		// Copilot agent may BE spawned as and the pair it may spawn ANOTHER
		// Copilot as are the same cell.
		//
		// The mode alone can never carry it. Copilot's tclaude-layer mode and
		// its no-wall mode are the same string, `off`: as a `tclaude-layer` row
		// it means tclaude's own wall is enforcing, and as a harness-builtin or
		// `off`-implementation row it means Copilot's experimental MXC sandbox
		// is asserted down and NOTHING replaced it. Claude and Codex have the
		// same collision but resolve it by remapping onto a second confinement
		// class they already have (`on`, the managed profile);
		// lineageConfinementMode is that remap, and Copilot has no second
		// class to remap onto. Only the implementation separates the two.
		//
		// Everything else fails closed: a blank/legacy row (which predates this
		// harness carrying a posture at all and asserts nothing), an explicit
		// harness-builtin, `stacked` (Copilot's own policy nested inside
		// tclaude's, an intersection nobody reviewed), implementation `off`, and
		// the `inherit` mode, whose posture is decided by a settings file
		// tclaude neither controls nor is notified about. The launch boundary
		// re-verifies the assert-off claim separately on every path that starts
		// a pane (TCL-989).
		if copilotProvenLineageLaunch(s) {
			return s, true
		}
	}
	return spawnLineageSandbox{}, false
}

func childIsClaude(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.DefaultName &&
		harnessBuiltinModeIn(child.HarnessBuiltinMode, modes...)
}

func childIsCodex(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.CodexName &&
		harnessBuiltinModeIn(child.HarnessBuiltinMode, modes...)
}

func harnessBuiltinModeIn(builtinMode string, allowed ...string) bool {
	for _, a := range allowed {
		if builtinMode == a {
			return true
		}
	}
	return false
}
