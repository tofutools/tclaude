package session

import (
	"fmt"
	"runtime"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchAccessPlanInput is what planning a launch's access enforcement reads.
type launchAccessPlanInput struct {
	h                           *harness.Harness
	params                      *NewParams
	model, cwd                  string
	extraArgs                   []string
	sandboxImplementation       sandboxpolicy.Implementation
	effectiveHarnessBuiltinMode string
	unconfined, outerLayer      bool
	layerEngine                 sandboxpolicy.NetworkEngine
	filteredNetworkProbe        *FilteredNetworkPrerequisite
	launchSandbox               *sandboxpolicy.Snapshot
	effectiveSandbox            *sandboxpolicy.Snapshot
}

// launchAccessState is the launch's sandbox state that planning may revise: a
// filtered posture the host cannot enforce drops to host-open with its root
// and OS-sandbox verdict re-derived, and the planned posture decides whether
// the harness is told its network is filtered.
type launchAccessState struct {
	posture         sandboxpolicy.NetworkPosture
	root            sandboxpolicy.RootPosture
	osSandbox       harness.LaunchOSSandbox
	networkFiltered bool
}

// planLaunchAccessEnforcement resolves and renders a confining launch's
// network, unix-socket and filesystem-root rules against what the harness and
// host can enforce. It records the resulting notices on both snapshots,
// validates a filtered launch's model transport, and materializes the
// unix-socket set. A launch without such rules, or one that enforces nothing,
// has nothing to plan.
func planLaunchAccessEnforcement(in launchAccessPlanInput, state *launchAccessState) error {
	h, params, model, cwd := in.h, in.params, in.model, in.cwd
	var err error
	if !in.unconfined && in.launchSandbox != nil &&
		(in.launchSandbox.Effective.Network != nil ||
			in.launchSandbox.Effective.UnixSockets != nil ||
			in.launchSandbox.Effective.FilesystemRoot == sandboxpolicy.FilesystemRootSeparate) {
		axes, axesErr := sandboxpolicy.EffectiveAccessAxes(in.launchSandbox.Effective)
		if axesErr != nil {
			return fmt.Errorf("derive sandbox access axes: %w", axesErr)
		}
		caps, capsErr := harness.ResolveAccessEnforcement(
			h, in.sandboxImplementation, axes, state.osSandbox, in.effectiveHarnessBuiltinMode,
		)
		if capsErr != nil {
			return capsErr
		}
		accessOptions := accessEnforcementOptionsFromLaunchNotices(
			in.launchSandbox.Effective.AccessNotices)
		rendered, notices, planErr := harness.PlanAccessEnforcement(
			axes, caps, accessOptions,
		)
		if planErr != nil {
			return planErr
		}
		renderedNetworkPosture, postureErr := sandboxpolicy.NetworkPostureForRules(
			rendered.Network)
		if postureErr != nil {
			return postureErr
		}
		filteredEnforcing := renderedNetworkPosture == sandboxpolicy.NetworkFiltered &&
			state.osSandbox.FilteredNetwork
		if runtime.GOOS == "linux" ||
			(runtime.GOOS == "darwin" &&
				!sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network)) {
			notices = appendFilteredNetworkPrerequisiteNotice(
				notices, in.outerLayer, axes.Network,
				filteredEnforcing,
				func() FilteredNetworkPrerequisite {
					if in.filteredNetworkProbe != nil {
						return *in.filteredNetworkProbe
					}
					return ProbeFilteredNetworkPrerequisite()
				},
			)
		}
		if state.posture == sandboxpolicy.NetworkFiltered && !filteredEnforcing {
			state.posture = sandboxpolicy.NetworkHostOpen
			// The root posture is re-derived rather than carried over: the
			// filtered posture implied a constructed root, and dropping to
			// host-open must not keep claiming one unless the profile's own
			// socket axis still asks for it.
			state.root, err = TclaudeLayerLaunchRootPosture(
				h, in.sandboxImplementation, state.posture,
				in.launchSandbox.Effective)
			if err != nil {
				return fmt.Errorf("derive sandbox root posture: %w", err)
			}
			state.osSandbox = TclaudeLayerLaunchOSSandboxForHarness(
				h.Name, state.posture, state.root, in.layerEngine)
		}
		if in.outerLayer {
			plannedEffective := in.launchSandbox.Effective
			plannedEffective.AccessNotices = replaceAccessDegradationNotices(
				plannedEffective.AccessNotices, notices...,
			)
			resolvedModel := harness.ResolvedModelTransport{Model: model}
			plannedAxes, plannedAxesErr := sandboxpolicy.PlannedEffectiveAccessAxes(
				plannedEffective)
			if plannedAxesErr != nil {
				return plannedAxesErr
			}
			plannedNetworkPosture, plannedPostureErr :=
				sandboxpolicy.NetworkPostureForRules(plannedAxes.Network)
			if plannedPostureErr != nil {
				return plannedPostureErr
			}
			// §7.4, from the same pre-injection environment the model-transport
			// gate below inspects: the launcher discards exactly what the
			// resolver saw, so the disclosure and the gate describe one launch.
			// The planned posture is passed in for the same reason the gate
			// below is conditioned on it: a policy that is not launching
			// filtered runs no proxy and performs no override. The enclosing
			// branch is already the tclaude-layer one, and the implementation
			// is handed over anyway so both launch surfaces ask one predicate
			// the same way rather than relying on where they sit.
			if noProxyNotice := ProxyEngineNoProxyOverrideNotice(
				runtime.GOOS, in.sandboxImplementation, plannedNetworkPosture,
				plannedAxes.Network, plannedEffective.Environment,
			); noProxyNotice != nil {
				notices = append(notices, *noProxyNotice)
			}
			// Private routed open is default-accept: no destination list to
			// resolve a route against, and nothing for pi to stay offline for.
			state.networkFiltered = plannedNetworkPosture == sandboxpolicy.NetworkFiltered &&
				!sandboxpolicy.NetworkRulesArePrivateRoutedOpen(plannedAxes.Network)
			if state.networkFiltered {
				var resolveModelErr error
				resolvedModel, resolveModelErr = ResolveTclaudeLayerModelTransport(
					h,
					ModelTransportLaunchContext{
						Model:       model,
						ModelProxy:  params.ModelProxy,
						Cwd:         cwd,
						Environment: plannedEffective.Environment,
						ExtraArgs:   in.extraArgs,
					},
				)
				if resolveModelErr != nil {
					return AnnotateDenyDrivenFilteredModelTransport(
						plannedAxes.Network, resolveModelErr)
				}
			}
			modelNotices, modelErr := ValidateTclaudeLayerNetwork(
				h,
				plannedEffective,
				resolvedModel,
			)
			if modelErr != nil {
				return AnnotateDenyDrivenFilteredModelTransport(
					plannedAxes.Network, modelErr)
			}
			notices = append(notices, modelNotices...)
		}
		materialization := in.launchSandbox.UnixSocketMaterialization
		if materialization == nil || rendered.UnixSockets.Mode != sandboxpolicy.AccessModeList {
			var materializationErr error
			materialization, materializationErr = sandboxpolicy.PrepareUnixSocketLaunch(
				rendered.UnixSockets)
			if materializationErr != nil {
				return materializationErr
			}
		}
		in.launchSandbox.Effective.AccessNotices = replaceAccessDegradationNotices(
			in.launchSandbox.Effective.AccessNotices, notices...,
		)
		sandboxpolicy.SetUnixSocketLaunchMaterialization(in.launchSandbox, materialization)
		if in.effectiveSandbox != nil {
			in.effectiveSandbox.Effective.AccessNotices = replaceAccessDegradationNotices(
				in.effectiveSandbox.Effective.AccessNotices, notices...,
			)
			sandboxpolicy.SetUnixSocketLaunchMaterialization(
				in.effectiveSandbox, materialization)
		}
	}
	return nil
}
