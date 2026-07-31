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
// It fires only when the host launch environment actually carried a non-empty
// exemption that the launcher then discarded. An override that changed nothing
// is not a disclosure an operator should have to read past to find the ones
// that did.
//
// The inspected environment is composed by launchModelEnvironment — the SAME
// composition the model-transport gate inspects — so the disclosure describes
// the exact pre-injection value the launcher replaces, including an authored
// EnvironmentEntry override of it. Re-deriving that composition here could
// disclose a host value the launch never saw, or miss an authored one it did.
//
// The engine predicate is ProxyEngineFloorApplies, likewise shared rather than
// re-derived: the override happens on precisely the launches whose floor is the
// proxy's, so a disclosure answering the engine question separately could
// report an override on a packet-gateway launch that never performs one.
func ProxyEngineNoProxyOverrideNotice(
	network sandboxpolicy.NetworkRules,
	environment []sandboxpolicy.EnvironmentEntry,
) *sandboxpolicy.AccessNotice {
	if !ProxyEngineFloorApplies(network) {
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
