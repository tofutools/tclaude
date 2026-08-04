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

var probeFilteredNetworkPrerequisite = session.ProbeFilteredNetworkPrerequisite

// spawnSandboxLineageFailure prevents an agent that can spawn peers from
// minting a child with a looser launch sandbox than the caller currently has.
// Humans bypass this check: they are the trust root everywhere else in agentd.
func spawnSandboxLineageFailure(
	parentConvID, childHarness, childSandbox, childImplementation string,
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
		Harness:        harnessOrDefault(childHarness),
		Mode:           strings.TrimSpace(childSandbox),
		Implementation: normalizeLineageImplementation(childImplementation),
	}
	if !spawnSandboxLineageAllowed(parent, child) {
		return &spawnFailure{http.StatusForbidden, "sandbox_restricted",
			fmt.Sprintf("agent %s was launched as %s sandbox %q and may not spawn a %s child with sandbox %q",
				short8(parentConvID), parent.Harness, parent.Mode, child.Harness, child.Mode)}
	}
	return nil
}

func sandboxProfileCapabilityFailure(
	harnessName, sandboxMode string,
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
	if _, err := harness.ResolveHarnessNativeSandboxMode(
		h, sandboxMode, implementation); err != nil {
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
	// The capability gate runs FIRST and unconditionally: a reopen-under-deny
	// shape must be refused by a harness that cannot enforce it even when the
	// profile carries no other rules. Approximating it would hand the operator a
	// false guarantee.
	//
	// The shape is read from the LAUNCH filesystem rather than the raw effective
	// set, so a deny/reopen pair that is inactive this launch (missing path) is
	// judged exactly as it will be rendered.
	if err := harness.ValidateSandboxReopenUnderDeny(harnessOrDefault(harnessName), sandboxMode, filesystem); err != nil {
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
		if strings.TrimSpace(sandboxMode) != harness.ClaudeSandboxOn && filesystemHasDeny(filesystem) {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
				fmt.Sprintf("Claude filesystem deny rules require sandbox %q; sandbox %q cannot guarantee enforcement", harness.ClaudeSandboxOn, sandboxMode)}
		}
		return nil
	case harness.CodexName:
		if strings.TrimSpace(sandboxMode) == harness.SandboxManagedProfile {
			if hasNetworkPolicy {
				if err := harness.ValidateCodexAgentNetworkAccess(snapshot.Effective.NetworkAccess); err != nil {
					return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network", err.Error()}
				}
			}
			return nil
		}
		if hasNetworkPolicy {
			return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_network",
				fmt.Sprintf("Codex network rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, sandboxMode)}
		}
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("Codex filesystem rules require sandbox %q; sandbox %q cannot represent them", harness.SandboxManagedProfile, sandboxMode)}
	case harness.OpenCodeName:
		if strings.TrimSpace(sandboxMode) == harness.OpenCodeSandboxAccessControl {
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
				sandboxMode, detail, harness.OpenCodeSandboxAccessControl)}
	default:
		return &spawnFailure{http.StatusUnprocessableEntity, "unsupported_sandbox_profile_filesystem",
			fmt.Sprintf("harness %q cannot represent sandbox filesystem rules", harnessName)}
	}
}

func planSandboxProfileAccessForLaunch(
	harnessName, sandboxMode string,
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
	if snapshot.Effective.Network == nil && snapshot.Effective.UnixSockets == nil {
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
	if implementation == sandboxpolicy.ImplementationTclaudeLayer &&
		h.Name == harness.OpenCodeName &&
		(harness.IsLocalAccessNetworkPreset(axes.Network) ||
			harness.IsLocalModelAPIsNetworkPreset(axes.Network)) {
		modelContext.Environment = snapshot.Effective.Environment
		if modelErr := session.ValidateTclaudeLayerOpenCodeLocalModelTransport(
			h, snapshot.Effective, modelContext,
		); modelErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				modelErr, harness.SandboxCapabilityModelTransport)
		}
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
			h, sandboxMode, "", modelContext.Cwd)
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
		root := sandboxpolicy.RootPostureFor(posture, sockets)
		deployedEngine, engineErr :=
			sandboxpolicy.DeployedNetworkEngineForRules(axes.Network)
		if engineErr != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				"invalid_sandbox_profile", engineErr.Error()}
		}
		verdict, err = resolveTclaudeLayerAccessVerdict(
			h.Name, posture, root, deployedEngine)
		if err != nil {
			// Reached from spawn, clone, reincarnate and relaunch — all of
			// which refuse here on a LIVE host-capability failure, so all of
			// them must resume the disclosure's presence checking.
			return nil, sandboxImplementationUnavailable(err.Error())
		}
	}
	caps, err := harness.ResolveAccessEnforcement(
		h, implementation, axes, verdict, sandboxMode,
	)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_access", err.Error()}
	}
	rendered, notices, err := harness.PlanAccessEnforcement(
		axes, caps, harness.AccessEnforcementOptions{
			AllowUnenforcedNetworkClosed: allowUnenforcedSandbox,
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
		resolvedModel, resolveModelErr := session.ResolveTclaudeLayerModelTransport(
			h, modelContext)
		if resolveModelErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				session.AnnotateDenyDrivenFilteredModelTransport(
					rendered.Network, resolveModelErr),
				harness.SandboxCapabilityModelTransport)
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
	harnessName, sandboxMode string,
	sandboxImplementation ...string,
) bool {
	if len(sandboxImplementation) > 0 {
		implementation, err := sandboxpolicy.NormalizeImplementation(sandboxImplementation[0])
		if err == nil && implementation == sandboxpolicy.ImplementationOff {
			return true
		}
	}
	switch harnessOrDefault(harnessName) {
	case harness.CodexName:
		return strings.TrimSpace(sandboxMode) == harness.SandboxDangerFull
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
	Mode    string
	// Implementation is WHO owns the OS wall for this launch. It is carried
	// because the harness-native mode alone stopped being a complete posture
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
			Harness:        harness.DefaultName,
			Mode:           harness.ClaudeSandboxInherit,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin,
		}, nil
	}
	h := harnessOrDefault(row.Harness)
	mode := strings.TrimSpace(row.SandboxMode)
	if h == harness.DefaultName && mode == "" {
		// Old Claude rows and the test simulator used "" for "settings.json
		// decides"; in the lineage matrix that is Claude's inherit sentinel.
		mode = harness.ClaudeSandboxInherit
	}
	return spawnLineageSandbox{
		Harness:        h,
		Mode:           mode,
		Implementation: normalizeLineageImplementation(row.SandboxImplementation),
	}, nil
}

func spawnSandboxLineageAllowed(parent, child spawnLineageSandbox) bool {
	parent, parentOK := normalizeSpawnLineageSandbox(parent)
	child, childOK := normalizeSpawnLineageSandbox(child)
	if !parentOK || !childOK {
		return false
	}
	child.Mode = childLineageConfinementMode(child)

	if parent.Harness == harness.DefaultName {
		switch parent.Mode {
		case harness.ClaudeSandboxOff:
			return true
		case harness.ClaudeSandboxInherit:
			return childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		case harness.ClaudeSandboxOn:
			return childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		}
	}

	if parent.Harness == harness.CodexName {
		switch parent.Mode {
		case harness.SandboxDangerFull:
			return true
		case harness.SandboxManagedProfile:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile) ||
				childIsClaude(child, harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn)
		case harness.SandboxWorkspaceWrite:
			return childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite)
		case harness.SandboxReadOnly:
			return childIsCodex(child, harness.SandboxReadOnly)
		}
	}
	if parent.Harness == harness.OpenCodeName {
		switch parent.Mode {
		case harness.OpenCodeSandboxOff:
			return true
		case harness.OpenCodeSandboxTclaudeLayer:
			return child.Harness == harness.OpenCodeName && child.Mode == harness.OpenCodeSandboxTclaudeLayer ||
				childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		case harness.OpenCodeSandboxAccessControl:
			return child.Harness == harness.OpenCodeName &&
				modeIn(child.Mode, harness.OpenCodeSandboxAccessControl, harness.OpenCodeSandboxTclaudeLayer) ||
				childIsClaude(child, harness.ClaudeSandboxOn) ||
				childIsCodex(child, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxManagedProfile)
		}
	}
	return false
}

// childLineageConfinementMode maps a CHILD's launch onto the confinement class
// the lineage matrix below reasons in.
//
// It exists because ResolveSandboxImplementationMode now forces the
// harness-native mode for a `tclaude-layer` launch, and that forced mode is the
// harness's own no-confinement spelling — Claude `off`, Codex
// `danger-full-access`. Read as a bare mode, those name the loosest posture in
// the matrix. Read with the implementation beside them, they name the opposite:
// the harness's inner wall is deliberately stood down BECAUSE tclaude's own
// wall is the one enforcing, so the child is confined at least as tightly as
// the harness-builtin sandboxed class it maps to.
//
// The mapping deliberately preserves the exact admission decisions these
// launches already got, when the guard saw their pre-forcing requested mode:
// Claude tclaude-layer classifies as Claude `on`, and Codex tclaude-layer as
// the Codex managed profile. OpenCode needs no arm — its native `tclaude-layer`
// mode is already the matrix's own name for this topology.
//
// The asymmetry with the PARENT side is deliberate, not an oversight. A parent
// is still classified by its persisted mode alone, so a tclaude-layer parent
// keeps being read as the fully-open class it reads as today. Tightening the
// parent side changes what existing agents are permitted to spawn, which is a
// separate, behaviour-changing decision (TCL-991); this function only keeps the
// child side from being *loosened* by the forcing above.
//
// Only the exact `tclaude-layer` implementation enters here. `stacked` runs the
// harness's own sandbox nested inside tclaude's, so its mode still means what
// the matrix thinks it means and must not be reinterpreted.
func childLineageConfinementMode(child spawnLineageSandbox) string {
	if child.Implementation != sandboxpolicy.ImplementationTclaudeLayer {
		return child.Mode
	}
	switch child.Harness {
	case harness.DefaultName:
		if child.Mode == harness.ClaudeSandboxOff {
			return harness.ClaudeSandboxOn
		}
	case harness.CodexName:
		if child.Mode == harness.SandboxDangerFull {
			return harness.SandboxManagedProfile
		}
	}
	return child.Mode
}

func normalizeSpawnLineageSandbox(s spawnLineageSandbox) (spawnLineageSandbox, bool) {
	s.Harness = harnessOrDefault(s.Harness)
	s.Mode = strings.TrimSpace(s.Mode)
	switch s.Harness {
	case harness.DefaultName:
		if s.Mode == "" {
			s.Mode = harness.ClaudeSandboxInherit
		}
		switch s.Mode {
		case harness.ClaudeSandboxInherit, harness.ClaudeSandboxOn, harness.ClaudeSandboxOff:
			return s, true
		}
	case harness.CodexName:
		switch s.Mode {
		case harness.SandboxManagedProfile, harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxDangerFull:
			return s, true
		}
	case harness.OpenCodeName:
		switch s.Mode {
		case harness.OpenCodeSandboxAccessControl, harness.OpenCodeSandboxTclaudeLayer, harness.OpenCodeSandboxOff:
			return s, true
		}
	}
	return spawnLineageSandbox{}, false
}

func childIsClaude(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.DefaultName && modeIn(child.Mode, modes...)
}

func childIsCodex(child spawnLineageSandbox, modes ...string) bool {
	return child.Harness == harness.CodexName && modeIn(child.Mode, modes...)
}

func modeIn(mode string, allowed ...string) bool {
	for _, a := range allowed {
		if mode == a {
			return true
		}
	}
	return false
}
