package session

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// proxyNetworkRoutingVariables are the proxy-routing variables the launcher
// owns on a proxy-engine launch. Every one of them is REPLACED rather than
// merged: an inherited value would point the harness at a destination tclaude
// does not supervise.
//
// A foreign value in one of these never reaches this replacement, because the
// model-transport gate has already refused the launch over it (§7.3). They are
// listed here as the launcher's own outputs, not as an allowlist.
var proxyNetworkRoutingVariables = []string{
	"HTTP_PROXY", "http_proxy",
	"HTTPS_PROXY", "https_proxy",
	"ALL_PROXY", "all_proxy",
}

// proxyNetworkExemptionVariables are the proxy-EXEMPTION variables, which the
// launcher overrides to empty rather than refusing over (§7.4). An inherited
// NO_PROXY can only carve destinations back out of the only route that exists,
// so the attempts it would authorize hit the empty-netns floor and fail closed;
// refusing a launch over a value that cannot widen anything would be
// disproportionate. The override is disclosed instead, by
// ProxyEngineNoProxyOverrideNotice below.
//
// This list is the single source of both halves of that behavior: the launcher
// overrides exactly these names, and the disclosure reports exactly these names
// as overridden. A disclosure derived from its own second list could name a
// variable the launcher does not own.
var proxyNetworkExemptionVariables = []string{"NO_PROXY", "no_proxy"}

// proxyNetworkProxyVariables are all the variables the launcher owns, in the
// order the sandbox environment is built.
var proxyNetworkProxyVariables = append(
	append([]string(nil), proxyNetworkRoutingVariables...),
	proxyNetworkExemptionVariables...,
)

// ProxyEngineNoProxyOverrideNotice is §7.4's disclosure: the proxy engine owns
// the sandbox's proxy environment, and an inherited NO_PROXY is overridden to
// empty rather than honored or refused over.
//
// It fires only on a launch that ACTUALLY PERFORMS the override, and only when
// the host environment actually carried a non-empty exemption for it to
// discard. Both halves matter, and in opposite directions: an override that
// changed nothing is noise an operator has to read past, while a notice on a
// launch that runs no proxy is worse than noise — it would tell the operator
// that a destination is unreachable when the inherited NO_PROXY is in fact
// still being honored, unfiltered.
//
// Four conditions, none of them re-derived here:
//
//   - goos is Linux. The override is performed by proxyNetworkSandboxEnv, which
//     is the Linux launcher's exec seam; the Darwin floor is M3. This is taken
//     as a parameter rather than read from runtime.GOOS so a caller predicting
//     for another target — and a test — can ask about a platform it is not
//     running on.
//   - the launch uses the tclaude layer. No other implementation builds this
//     floor or injects this environment.
//   - the launch deploys a proxy: TclaudeLayerDeploysProxy, the same predicate
//     the launcher's own plan asks (tclaudeLayerPlanDeploysProxy). It needs
//     BOTH the filtered posture and the proxy engine, which is why the engine
//     question alone is not enough: a policy whose list widened to open keeps
//     its authored engine and would otherwise still be disclosed as filtered.
//   - the composed pre-injection environment carries a non-empty exemption. The
//     composition is launchModelEnvironment, the SAME one the model-transport
//     gate inspects, so the disclosure describes the exact value the launcher
//     replaces, including an authored EnvironmentEntry override of it.
//
// Like that gate, this reads the environment of the process resolving the
// launch. On the daemon path that is agentd's environment rather than the pane
// the harness will run in; the session boundary recomputes the notice at the
// launch itself, and degradation notices are replaced rather than merged, so
// the persisted record ends up describing the real launch.
//
// The notice is classed as a degradation despite the effect saying the opposite
// — nothing is enforced more weakly here — because that class is what makes it
// recomputed and REPLACED at every launch. A stale override notice surviving on
// a later launch that inherited no exemption would be a false statement about
// that launch. The one coupling that buys, stated so it is not rediscovered:
// on resume, mergeResumeAccessNotices takes its replace branch whenever the
// previous snapshot carried ANY degradation, so this notice puts an otherwise
// undegraded launch on that branch. That is the correct branch for it — the
// resolve that produced it regenerates it — but it means adding a degradation
// notice here is a decision about resume behavior as well as about rendering.
func ProxyEngineNoProxyOverrideNotice(
	goos string,
	implementation sandboxpolicy.Implementation,
	posture sandboxpolicy.NetworkPosture,
	network sandboxpolicy.NetworkRules,
	environment []sandboxpolicy.EnvironmentEntry,
) *sandboxpolicy.AccessNotice {
	if goos != "linux" || !implementation.UsesTclaudeLayer() {
		return nil
	}
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(network)
	if err != nil || !TclaudeLayerDeploysProxy(posture, engine) {
		return nil
	}
	inherited := launchModelEnvironment(environment)
	overridden := make([]string, 0, len(proxyNetworkExemptionVariables))
	for _, name := range proxyNetworkExemptionVariables {
		if strings.TrimSpace(inherited[name]) != "" {
			overridden = append(overridden, name)
		}
	}
	if len(overridden) == 0 {
		return nil
	}
	return &sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Reason: sandboxpolicy.AccessNoticeReasonProxyEngineNoProxyOverride,
		Effect: sandboxpolicy.AccessNoticeEffectEnvironmentOverridden,
		Detail: fmt.Sprintf(
			"the filtering proxy owns this sandbox's proxy environment: inherited %s is overridden to empty inside the sandbox, "+
				"so no destination is exempted from the proxy. "+
				"A destination that value would have exempted is not reachable around the proxy — it has no route out of the sandbox floor and fails closed — "+
				"so author it as a network rule if the sandbox needs it.",
			strings.Join(overridden, " and ")),
	}
}
