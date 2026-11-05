package session

import (
	"os"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchOSSandboxResolution is what confines this launch at the OS level, as
// resolved before any access rules are planned: who chose the harness's
// builtin mode, the OS-sandbox verdict the session row records, and, for a
// tclaude-layer launch, the bubblewrap binary plus the capability error the
// launch reports once its row exists.
type launchOSSandboxResolution struct {
	chosenBy           string
	launch             harness.LaunchOSSandbox
	bwrapBinary        string
	bwrapCapabilityErr error
}

// resolveLaunchOSSandbox resolves whether the OS sandbox will confine this
// launch and what decided that: the harness's builtin sandbox, or, for an
// outer-layer launch, tclaude's own layer built from the resolved network
// and root posture.
func resolveLaunchOSSandbox(
	h *harness.Harness,
	params *NewParams,
	harnessBuiltinMode, cwd string,
	outerLayer bool,
	layer tclaudeLayerNetwork,
) launchOSSandboxResolution {
	// Record what the warning check just resolved: whether the OS sandbox will
	// actually confine this agent, and what decided that. The recorded
	// sandbox mode cannot say — Claude Code's `inherit` default means "whatever
	// settings.json says" — and the answer is only knowable HERE, against the
	// settings files as they are at launch (TCL-729). Same inputs as the
	// messages above, so the badge an operator sees later cannot contradict the
	// disclosure or warning they saw at spawn.
	// Who chose the mode. The daemon resolves it through its profile tier stack
	// and passes it in; a direct `session new --sandbox …` is the human's own
	// choice. A managed launch with nothing passed stays unattributed rather
	// than claiming "explicit" on the daemon's behalf — an older agentd that
	// does not send the tier is unknown, not explicit.
	// Bounded and scrubbed HERE, at the point of record: this value is written
	// to sessions.sandbox_mode_source, projected into the durable relaunch
	// profile, and replayed into a child argv on every later relaunch. Cleaning
	// only the derived os_sandbox_source would leave the raw label in all three.
	sandboxChosenBy := harness.SanitizeSandboxChosenBy(params.SandboxChosenBy)
	if sandboxChosenBy == "" && harnessBuiltinMode != "" && !params.ManagedLaunch {
		sandboxChosenBy = harness.SandboxChosenExplicitly
	}
	launchOSSandbox := harness.ResolveLaunchOSSandbox(h, harnessBuiltinMode, sandboxChosenBy, cwd)
	bwrapBinary := ""
	var bwrapCapabilityErr error
	if outerLayer {
		if tclaudeLayerWrapsPane(h.Name) {
			bwrapBinary, launchOSSandbox, bwrapCapabilityErr = ResolveTclaudeLayerForEngineWithIdentity(
				layer.posture, layer.root, layer.engine,
				layer.preserveCallerIdentity)
		} else {
			bwrapBinary, launchOSSandbox, bwrapCapabilityErr = ResolveTclaudeLayerServerForEngineWithIdentity(
				layer.posture, layer.root, layer.engine,
				layer.preserveCallerIdentity)
		}
		if bwrapCapabilityErr == nil {
			launchOSSandbox = TclaudeLayerLaunchOSSandboxForHarness(
				h.Name, layer.posture, layer.root, layer.engine,
				layer.preserveCallerIdentity)
		}
	}
	if outerLayer {
		if h.Name == harness.OpenCodeName {
			switch os.Getenv(clcommon.OpenCodeStateIsolationEnv) {
			case "private":
				launchOSSandbox.Source += "; " + clcommon.OpenCodeStatePrivateNote
			case "legacy-shared":
				launchOSSandbox.Source += "; " + clcommon.OpenCodeStateLegacyNote
			}
		}
	}
	return launchOSSandboxResolution{
		chosenBy:           sandboxChosenBy,
		launch:             launchOSSandbox,
		bwrapBinary:        bwrapBinary,
		bwrapCapabilityErr: bwrapCapabilityErr,
	}
}

// gateTclaudeLayerHarnessPosture verifies, for every tclaude-layer launch,
// the posture of a harness whose own sandbox the launch cannot turn off, and
// refuses pass-through arguments that would contradict it.
func gateTclaudeLayerHarnessPosture(h *harness.Harness, outerLayer bool, launchSandbox *sandboxpolicy.Snapshot, extraArgs []string) error {
	// A harness whose own sandbox cannot be turned off by the launch has its
	// posture verified for EVERY tclaude-layer launch, not only for one that
	// authored a policy.
	//
	// This used to sit inside the access-axes block below, which runs only when
	// a profile declares network or socket rules. A direct
	// `session new --sandbox-impl tclaude-layer --harness copilot` with no
	// profile — the simplest way to reach this code — therefore launched with
	// neither the assert-off gate nor the pass-through-argument refusal ever
	// running, while the recorded posture claimed a single boundary. The gate
	// belongs to the IMPLEMENTATION choice, which is what makes that claim, not
	// to the access axes.
	//
	// The environment is the launch's own when a profile supplied one, because
	// a profile that relocates COPILOT_HOME moves which file governs the launch
	// and a gate reading the ambient one would be inspecting a file the agent
	// never opens. With no profile there is nothing to compose, and nil means
	// the ambient environment — which is then also what the launch itself uses.
	if outerLayer {
		var launchEnvironment []sandboxpolicy.EnvironmentEntry
		if launchSandbox != nil {
			launchEnvironment = sandboxpolicy.EnvironmentForLaunch(launchSandbox)
		}
		if err := ValidateTclaudeLayerHarnessPosture(h, launchEnvironment, extraArgs); err != nil {
			return err
		}
	}
	return nil
}

// discloseUnconfinedAccessRules records, on a launch whose implementation
// enforces nothing, the notice that names the access rules it will not
// enforce.
func discloseUnconfinedAccessRules(
	sandboxImplementation sandboxpolicy.Implementation,
	unconfined bool,
	launchSandbox, effectiveSandbox *sandboxpolicy.Snapshot,
) {
	if unconfined && launchSandbox != nil {
		// Everything the block below does — resolve capabilities, render the
		// planned rules, probe the filtered-network prerequisite, materialize a
		// unix socket set — describes enforcement. This implementation has
		// none, so there is nothing to render and, in particular, nothing to
		// materialize: a socket set prepared for a launch that enforces no
		// socket policy is state created for no reader.
		//
		// The rules still have to be DISCLOSED, and by the same words the
		// daemon uses, which is why the notice is built in sandboxpolicy rather
		// than at either seam.
		if notice, ok := sandboxpolicy.UnconfinedAccessRulesNotice(
			sandboxImplementation, launchSandbox.Effective,
		); ok {
			launchSandbox.Effective.AccessNotices = replaceAccessDegradationNotices(
				launchSandbox.Effective.AccessNotices, notice,
			)
			if effectiveSandbox != nil {
				effectiveSandbox.Effective.AccessNotices = replaceAccessDegradationNotices(
					effectiveSandbox.Effective.AccessNotices, notice,
				)
			}
		}
	}
}
