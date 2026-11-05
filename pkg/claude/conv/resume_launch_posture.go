package conv

import (
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// applyResumeLayerAgentEnv is the resume's agent-environment step behind
// tclaude's sandbox: the hook-broker marker, and the agentd socket selection
// for the root posture the resume builds, also exported to the pane's shell
// environment when the root is constructed. It returns shellEnvironment,
// allocated if it was nil and a socket was selected.
//
// The root posture comes from session.TclaudeLayerRootPosture, the
// post-ladder derivation, not runNew's pre-ladder
// TclaudeLayerLaunchRootPosture. That is deliberate: the persisted snapshot
// already carries the degradations the capability ladder recorded at spawn,
// which PlannedEffectiveAccessAxes applies, and the explicit-root target
// matrix was enforced at spawn for this conversation's fixed harness and
// implementation.
func applyResumeLayerAgentEnv(
	h *harness.Harness,
	effectiveProfile sandboxpolicy.EffectiveProfile,
	resumeEnv, shellEnvironment map[string]string,
) (map[string]string, error) {
	// Mirror the spawn seam: hook callbacks go through agentd for a
	// launch whose namespace hides the database (TCL-754). Applied after
	// the recorded profile environment so a stale or hostile profile
	// cannot unset the marker on resume.
	resumeEnv[session.HookBrokerEnvVar] = session.HookBrokerAgentd
	posture, err := session.TclaudeLayerNetworkPosture(effectiveProfile)
	if err != nil {
		return nil, err
	}
	rootPosture, err := session.TclaudeLayerRootPosture(posture, effectiveProfile)
	if err != nil {
		return nil, err
	}
	if err := session.ApplyAgentSocketEnv(
		h.Name,
		"",
		"",
		rootPosture == sandboxpolicy.RootConstructed,
		resumeEnv,
	); err != nil {
		return nil, err
	}
	if selected := resumeEnv[agentipc.SocketEnv]; selected != "" &&
		rootPosture == sandboxpolicy.RootConstructed {
		if shellEnvironment == nil {
			shellEnvironment = map[string]string{}
		}
		shellEnvironment[agentipc.SocketEnv] = selected
	}
	return shellEnvironment, nil
}

// gateResumeLayerNetwork is the resume's network gate behind tclaude's
// sandbox: a filtered posture resolves the harness's model route (pi from the
// model its session records), and the planned network is validated against
// that route. It reports whether the resume's network is filtered.
func gateResumeLayerNetwork(
	h *harness.Harness,
	effectiveProfile sandboxpolicy.EffectiveProfile,
	convID, resumeCwd string,
	extraArgs []string,
) (bool, error) {
	var err error
	networkFiltered := false
	resolvedModel := harness.ResolvedModelTransport{}
	plannedAxes, axesErr := sandboxpolicy.PlannedEffectiveAccessAxes(
		effectiveProfile)
	if axesErr != nil {
		return false, axesErr
	}
	plannedNetworkPosture, postureErr := sandboxpolicy.NetworkPostureForRules(
		plannedAxes.Network)
	if postureErr != nil {
		return false, postureErr
	}
	// As in `session new`: private routed open is default-accept, so it
	// has no route to resolve; resolving it here refused resumes of
	// launches the spawn admitted.
	if plannedNetworkPosture == sandboxpolicy.NetworkFiltered &&
		!sandboxpolicy.NetworkRulesArePrivateRoutedOpen(plannedAxes.Network) {
		networkFiltered = true
		// pi resumes with the model its session records, so that is
		// the route to resolve; the others resolve from config alone.
		resumeModel := ""
		if h.Name == harness.PiName {
			resumeModel = harness.PiSessionModel(convID, resumeCwd)
		}
		resolvedModel, err = session.ResolveTclaudeLayerModelTransport(
			h,
			session.ModelTransportLaunchContext{
				Model:       resumeModel,
				Cwd:         resumeCwd,
				Environment: effectiveProfile.Environment,
				ExtraArgs:   extraArgs,
			},
		)
		if err != nil {
			return false,
				session.AnnotateDenyDrivenFilteredModelTransport(
					plannedAxes.Network, err)
		}
	}
	if _, err := session.ValidateTclaudeLayerNetwork(
		h, effectiveProfile, resolvedModel,
	); err != nil {
		return false,
			session.AnnotateDenyDrivenFilteredModelTransport(
				plannedAxes.Network, err)
	}
	return networkFiltered, nil
}
