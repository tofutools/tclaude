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

var resolveTclaudeLayerAccessVerdict = func(
	harnessName string,
	posture sandboxpolicy.NetworkPosture,
) (harness.LaunchOSSandbox, error) {
	if session.TclaudeLayerUsesServerBoundary(harnessName) {
		_, verdict, err := session.ResolveTclaudeLayerServer(posture)
		return verdict, err
	}
	_, verdict, err := session.ResolveTclaudeLayer(posture)
	return verdict, err
}

var probeFilteredNetworkPrerequisite = session.ProbeFilteredNetworkPrerequisite

// spawnSandboxLineageFailure prevents an agent that can spawn peers from
// minting a child with a looser launch sandbox than the caller currently has.
// Humans bypass this check: they are the trust root everywhere else in agentd.
func spawnSandboxLineageFailure(parentConvID, childHarness, childSandbox string) *spawnFailure {
	if parentConvID == "" {
		return nil
	}
	parent, err := spawnLineageParentSandbox(parentConvID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io",
			"spawn sandbox guard: " + err.Error()}
	}
	child := spawnLineageSandbox{
		Harness: harnessOrDefault(childHarness),
		Mode:    strings.TrimSpace(childSandbox),
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
	if _, err := harness.ResolveOpenCodeSandboxImplementationMode(
		harnessOrDefault(harnessName), sandboxMode, implementation); err != nil {
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
	allowUnenforcedNetworkClosed bool,
) ([]sandboxpolicy.AccessNotice, *spawnFailure) {
	if snapshot == nil ||
		(snapshot.Effective.Network == nil && snapshot.Effective.UnixSockets == nil) {
		return nil, nil
	}
	h, err := harness.Resolve(harnessOrDefault(harnessName))
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"unsupported_sandbox_profile_access", err.Error()}
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return nil, &spawnFailure{http.StatusUnprocessableEntity,
			"invalid_sandbox_implementation", err.Error()}
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
		verdict, err = resolveTclaudeLayerAccessVerdict(h.Name, posture)
		if err != nil {
			return nil, &spawnFailure{http.StatusUnprocessableEntity,
				sandboxImplementationUnavailableKind, err.Error()}
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
			AllowUnenforcedNetworkClosed: allowUnenforcedNetworkClosed,
		},
	)
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
	if implementation.UsesTclaudeLayer() &&
		renderedNetworkPosture == sandboxpolicy.NetworkFiltered {
		plannedEffective := snapshot.Effective
		plannedEffective.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
			plannedEffective.AccessNotices, notices...,
		)
		modelContext.Environment = plannedEffective.Environment
		resolvedModel, resolveModelErr := session.ResolveTclaudeLayerModelTransport(
			h, modelContext)
		if resolveModelErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				resolveModelErr, harness.SandboxCapabilityModelTransport)
		}
		modelNotices, modelErr := session.ValidateTclaudeLayerNetwork(
			h, plannedEffective, resolvedModel,
		)
		if modelErr != nil {
			return nil, sandboxCapabilitySpawnFailure(
				modelErr, harness.SandboxCapabilityModelTransport)
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
// profile that renders filesystem rules. OpenCode off remains profile-aware so
// an incompatible filesystem or network profile fails loudly instead of being
// silently discarded; an empty policy is still a valid off launch.
func sandboxProfilesDisabled(harnessName, sandboxMode string) bool {
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
		return spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit}, nil
	}
	h := harnessOrDefault(row.Harness)
	mode := strings.TrimSpace(row.SandboxMode)
	if h == harness.DefaultName && mode == "" {
		// Old Claude rows and the test simulator used "" for "settings.json
		// decides"; in the lineage matrix that is Claude's inherit sentinel.
		mode = harness.ClaudeSandboxInherit
	}
	return spawnLineageSandbox{Harness: h, Mode: mode}, nil
}

func spawnSandboxLineageAllowed(parent, child spawnLineageSandbox) bool {
	parent, parentOK := normalizeSpawnLineageSandbox(parent)
	child, childOK := normalizeSpawnLineageSandbox(child)
	if !parentOK || !childOK {
		return false
	}

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
