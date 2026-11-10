package session

import (
	"fmt"
	"runtime"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// tclaudeLayerNetwork is how tclaude's own sandbox layer will build this
// launch's network and filesystem root: the network posture, the filtering
// engine it deploys, whether the packet engine must preserve the caller's
// identity, the filtered-network host probe (when one ran), and the root
// posture the agentd socket environment follows.
type tclaudeLayerNetwork struct {
	posture                sandboxpolicy.NetworkPosture
	engine                 sandboxpolicy.NetworkEngine
	preserveCallerIdentity bool
	probe                  *FilteredNetworkPrerequisite
	root                   sandboxpolicy.RootPosture
}

// resolveTclaudeLayerNetwork resolves the launch's tclaude-layer network
// posture, filtering engine and root posture from the requested network
// access and the launch sandbox, refusing a filtered posture the host or
// harness cannot honour.
func resolveTclaudeLayerNetwork(
	h *harness.Harness,
	sandboxImplementation sandboxpolicy.Implementation,
	outerLayer bool,
	networkAccess sandboxpolicy.NetworkAccess,
	launchSandbox *sandboxpolicy.Snapshot,
) (tclaudeLayerNetwork, error) {
	tclaudeLayerPosture, err := sandboxpolicy.NetworkPostureForAccess(networkAccess)
	if err != nil {
		return tclaudeLayerNetwork{}, err
	}
	var filteredNetworkProbe *FilteredNetworkPrerequisite
	// The engine this launch DEPLOYS, resolved once here and carried to every
	// site that must agree about which floor is being built: the posture gate
	// below, the host-capability resolve, and the boundary sentence. Unset means
	// "no engine deployed", which leaves every one of those on the pre-engine
	// path by construction.
	tclaudeLayerEngine := sandboxpolicy.NetworkEngineUnset
	tclaudeLayerPreserveCallerIdentity := false
	if outerLayer &&
		sandboxImplementation == sandboxpolicy.ImplementationTclaudeLayer &&
		launchSandbox != nil && launchSandbox.Effective.Network != nil {
		axes, axesErr := sandboxpolicy.EffectiveAccessAxes(launchSandbox.Effective)
		if axesErr != nil {
			return tclaudeLayerNetwork{}, fmt.Errorf("derive sandbox access axes: %w", axesErr)
		}
		requestedNetworkPosture, postureErr := TclaudeLayerNetworkPosture(
			launchSandbox.Effective)
		if postureErr != nil {
			return tclaudeLayerNetwork{}, fmt.Errorf("derive sandbox network posture: %w", postureErr)
		}
		deployedEngine, engineErr := sandboxpolicy.DeployedNetworkEngineForRules(
			axes.Network)
		if engineErr != nil {
			return tclaudeLayerNetwork{}, fmt.Errorf("resolve network filtering engine: %w", engineErr)
		}
		tclaudeLayerEngine = deployedEngine
		tclaudeLayerPreserveCallerIdentity = requestedNetworkPosture == sandboxpolicy.NetworkFiltered &&
			deployedEngine == sandboxpolicy.NetworkEnginePacket
		if requestedNetworkPosture == sandboxpolicy.NetworkFiltered {
			if runtime.GOOS == "darwin" &&
				sandboxpolicy.NetworkRulesAreLoopbackOnly(axes.Network) {
				tclaudeLayerPosture = sandboxpolicy.NetworkFiltered
			} else if runtime.GOOS == "linux" {
				if ProxyEngineFloorApplies(axes.Network) {
					// TCL-883: the proxy floor gets its own spawn-time gate. The
					// packet gateway's probe is not it — pasta and nft are never
					// called here — and gating on it would refuse the filtered
					// posture on exactly the low-prerequisite hosts this engine
					// exists to serve. The gate that DOES apply is the posture-
					// exact resolve below, which maps the proxy floor onto the
					// isolated posture and probes bubblewrap's namespaces plus
					// pidfd; a host that cannot build the floor is refused there
					// rather than silently widened to open here.
					tclaudeLayerPosture = sandboxpolicy.NetworkFiltered
				} else {
					probe := ProbeFilteredNetworkPrerequisite()
					filteredNetworkProbe = &probe
					if supportErr := ValidateFilteredNetworkHarnessSupport(
						h, sandboxImplementation, axes, probe,
					); supportErr != nil {
						return tclaudeLayerNetwork{}, supportErr
					}
					if probe.Detected {
						tclaudeLayerPosture = sandboxpolicy.NetworkFiltered
					}
				}
			}
		}
	}
	// The constructed root is what makes the canonical agentd socket the only
	// reachable one, so the environment follows the ROOT posture rather than the
	// network posture it used to be inseparable from (TCL-798).
	tclaudeLayerRoot := sandboxpolicy.RootHostInherited
	if outerLayer && launchSandbox != nil {
		// This runs before the capability ladder, so the gated helper is
		// required: on a target where the socket-driven constructed root does
		// not apply the ladder is about to widen the axis away, and claiming a
		// constructed root here would refuse an explicit agentd socket for a
		// launch that never confines sockets.
		tclaudeLayerRoot, err = TclaudeLayerLaunchRootPosture(
			h, sandboxImplementation, tclaudeLayerPosture, launchSandbox.Effective)
		if err != nil {
			return tclaudeLayerNetwork{}, fmt.Errorf("derive sandbox root posture: %w", err)
		}
	} else if tclaudeLayerPosture == sandboxpolicy.NetworkIsolatedWithAgentd ||
		tclaudeLayerPosture == sandboxpolicy.NetworkFiltered {
		tclaudeLayerRoot = sandboxpolicy.RootConstructed
	}
	return tclaudeLayerNetwork{
		posture:                tclaudeLayerPosture,
		engine:                 tclaudeLayerEngine,
		preserveCallerIdentity: tclaudeLayerPreserveCallerIdentity,
		probe:                  filteredNetworkProbe,
		root:                   tclaudeLayerRoot,
	}, nil
}
