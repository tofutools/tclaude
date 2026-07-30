package sandboxproxy

import (
	"fmt"
	"net/netip"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Verdict is the outcome of one policy question. Refusal verdicts are distinct
// so a carriage can phrase a legible reason rather than failing opaquely — the
// proxy is the one component that can tell the harness itself why.
type Verdict string

const (
	// VerdictAllowed authorizes the requested target.
	VerdictAllowed Verdict = "allowed"
	// VerdictDeniedByRule is an explicit deny row winning an overlap.
	VerdictDeniedByRule Verdict = "denied_by_rule"
	// VerdictNotAuthorized is the absence of any allow row covering the
	// target under a deny baseline.
	VerdictNotAuthorized Verdict = "not_authorized"
	// VerdictPrivateDestination is the host-side resolution blocker: the name
	// was authorized but resolves into address space the policy never asked
	// for. This is what closes the DNS-rebinding route the packet posture
	// documents as an open gap.
	VerdictPrivateDestination Verdict = "private_destination"
	// VerdictUnresolvable reports that an authorized name has no usable
	// address. It is not a policy refusal and is kept separate so a resolution
	// failure is never rendered as a policy one.
	VerdictUnresolvable Verdict = "unresolvable"
)

// Decision is the complete result of evaluating one target. Both carriages
// receive the identical value for the identical target, which is what the
// carriage-equivalence test asserts.
type Decision struct {
	Verdict Verdict
	// Rule is the deny row that refused or the allow row that authorized. It
	// is nil when no row was involved: an open baseline authorizing by
	// default, or a refusal for want of any match.
	Rule *sandboxpolicy.FilteredNetworkRule
	// Detail is capability-phrased text naming the destination and the
	// remedy. It is safe to hand to a sandboxed client.
	Detail string
}

// Allowed reports whether the request may proceed.
func (d Decision) Allowed() bool { return d.Verdict == VerdictAllowed }

// Evaluator answers policy questions for one materialized network policy. It
// is immutable after construction and safe for concurrent use.
type Evaluator struct {
	rules         sandboxpolicy.FilteredNetworkRuleSet
	defaultAccept bool
}

// NewEvaluator compiles materialized launch intent into an evaluator. It
// rejects unmaterialized rules for the same reason the discrimination
// predicate does: a caller that forgot to expand packs must fail closed.
func NewEvaluator(rules sandboxpolicy.NetworkRules) (*Evaluator, error) {
	compiled, err := sandboxpolicy.CompileFilteredNetworkRules(rules)
	if err != nil {
		return nil, err
	}
	return NewEvaluatorFromRuleSet(compiled)
}

// NewEvaluatorFromRuleSet accepts already-compiled gateway IR, which is the
// same IR the packet posture consumes. Sharing it is what keeps the two
// postures answering from one authored list.
func NewEvaluatorFromRuleSet(
	rules sandboxpolicy.FilteredNetworkRuleSet,
) (*Evaluator, error) {
	if rules.ProtocolContract != sandboxpolicy.FilteredNetworkProtocolContract {
		return nil, fmt.Errorf("filtered network protocol contract is invalid")
	}
	verdict := sandboxpolicy.FilteredNetworkDefaultVerdictForRules(rules)
	switch verdict {
	case sandboxpolicy.FilteredNetworkDefaultDrop:
	case sandboxpolicy.FilteredNetworkDefaultAccept:
	default:
		return nil, fmt.Errorf(
			"filtered network default verdict %q is invalid", verdict)
	}
	return &Evaluator{
		rules:         rules,
		defaultAccept: verdict == sandboxpolicy.FilteredNetworkDefaultAccept,
	}, nil
}

// Evaluate answers the policy question on the target as the client stated it,
// before any resolution. Deny is evaluated first and wins every overlap, so
// authoring order cannot change a result.
func (e *Evaluator) Evaluate(target Target) Decision {
	// Host loopback has two spellings — the literal and the name — and they
	// are one identity. Matching both against the same name keeps a deny row
	// from refusing one spelling while admitting the other.
	name := target.Name
	if target.Kind == TargetKindLiteral && target.IsLoopback() {
		name = LoopbackTargetName
	}
	var names sandboxpolicy.FilteredNetworkDNSMatches
	if name != "" {
		matched, err := sandboxpolicy.MatchFilteredNetworkDNSPolicy(e.rules, name)
		if err != nil {
			return Decision{
				Verdict: VerdictNotAuthorized,
				Detail:  refusalDetail(target, VerdictNotAuthorized),
			}
		}
		names = matched
	}
	if rule := e.match(e.rules.DenyRules, names.Deny, target, true); rule != nil {
		return Decision{
			Verdict: VerdictDeniedByRule,
			Rule:    rule,
			Detail:  refusalDetail(target, VerdictDeniedByRule),
		}
	}
	// An open baseline authorizes everything it does not deny — except host
	// loopback, which no baseline has ever reached. Under the packet engine an
	// open posture puts the sandbox on the host network, where its loopback is
	// its own; host loopback is only ever reached through an authored loopback
	// row. Honoring the default here would hand an open policy a destination it
	// never asked for and never previously had.
	if e.defaultAccept && !target.IsLoopback() {
		return Decision{Verdict: VerdictAllowed}
	}
	if rule := e.match(e.rules.Rules, names.Allow, target, false); rule != nil {
		return Decision{Verdict: VerdictAllowed, Rule: rule}
	}
	return Decision{
		Verdict: VerdictNotAuthorized,
		Detail:  refusalDetail(target, VerdictNotAuthorized),
	}
}

// match returns the first row of one polarity covering the target, in the
// canonical rule order the compiler produced. nameMatches carries the rows the
// shared DNS matcher already selected for a name target, so this package never
// implements a second domain matcher.
func (e *Evaluator) match(
	rules []sandboxpolicy.FilteredNetworkRule,
	nameMatches []sandboxpolicy.FilteredNetworkRule,
	target Target,
	deny bool,
) *sandboxpolicy.FilteredNetworkRule {
	if target.IsLoopback() {
		for i := range rules {
			if rules[i].Selector != sandboxpolicy.NetworkSelectorLoopback {
				continue
			}
			if portsCover(rules[i].Ports, target.Port) {
				return &rules[i]
			}
		}
		if !deny {
			// The loopback selector is the sole host-loopback authority: a
			// host or domain row spelled "localhost" grants nothing, so no
			// ordinary name rule can substitute for the authored intent.
			return nil
		}
		// A deny row may never be narrowed by that rule. A row that matches
		// the loopback name still refuses, because refusing more is always
		// within an operator's authored intent.
		return firstCoveringPort(nameMatches, target.Port)
	}
	switch target.Kind {
	case TargetKindName:
		return firstCoveringPort(nameMatches, target.Port)
	case TargetKindLiteral:
		for i := range rules {
			if rules[i].Selector != sandboxpolicy.NetworkSelectorCIDR {
				continue
			}
			prefix, err := netip.ParsePrefix(rules[i].Value)
			if err != nil {
				continue
			}
			if !prefix.Contains(target.Addr) {
				continue
			}
			if portsCover(rules[i].Ports, target.Port) {
				return &rules[i]
			}
		}
		return nil
	default:
		return nil
	}
}

func firstCoveringPort(
	rules []sandboxpolicy.FilteredNetworkRule,
	port int,
) *sandboxpolicy.FilteredNetworkRule {
	for i := range rules {
		if portsCover(rules[i].Ports, port) {
			return &rules[i]
		}
	}
	return nil
}

// portsCover applies the authored meaning of an empty port list: any port.
func portsCover(ports []int, port int) bool {
	if len(ports) == 0 {
		return true
	}
	return slices.Contains(ports, port)
}

// EvaluateResolvedAddress applies the private-destination blocker to one
// address a name resolved to. The resolved address is deliberately not
// re-checked against the authored list — there is no lease model here and name
// identity is the authority — but it may not lead somewhere the operator never
// asked for.
//
// The carve-outs are exactly the two authored ways to ask: a loopback row for
// host loopback, and an explicit CIDR row for other reserved space. Both must
// also cover the requested port, so the blocker never grants more than the row
// that carved it out.
//
// The blocker applies only in allowlist postures. Under an open baseline the
// policy authorizes everything except its authored denies, so refusing private
// space would narrow an otherwise-open posture and would disagree with what
// the packet engine does for the same authored policy. Open means open, minus
// the denies (operator ruling, 2026-07-30; it supersedes the design doc's
// unconditional wording).
func (e *Evaluator) EvaluateResolvedAddress(
	target Target,
	addr netip.Addr,
) Decision {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return Decision{
			Verdict: VerdictPrivateDestination,
			Detail:  refusalDetail(target, VerdictPrivateDestination),
		}
	}
	// The loopback carve-out is not part of the baseline-conditional blocker:
	// host loopback is reachable only through an authored loopback row under
	// every baseline, for the reason Evaluate states.
	if !addr.IsLoopback() && e.defaultAccept {
		return Decision{Verdict: VerdictAllowed}
	}
	if !isReservedDestination(addr) {
		return Decision{Verdict: VerdictAllowed}
	}
	if addr.IsLoopback() {
		for i := range e.rules.Rules {
			if e.rules.Rules[i].Selector != sandboxpolicy.NetworkSelectorLoopback {
				continue
			}
			if portsCover(e.rules.Rules[i].Ports, target.Port) {
				return Decision{Verdict: VerdictAllowed, Rule: &e.rules.Rules[i]}
			}
		}
		return Decision{
			Verdict: VerdictPrivateDestination,
			Detail:  refusalDetail(target, VerdictPrivateDestination),
		}
	}
	for i := range e.rules.Rules {
		if e.rules.Rules[i].Selector != sandboxpolicy.NetworkSelectorCIDR {
			continue
		}
		prefix, err := netip.ParsePrefix(e.rules.Rules[i].Value)
		if err != nil || !prefix.Contains(addr) {
			continue
		}
		if portsCover(e.rules.Rules[i].Ports, target.Port) {
			return Decision{Verdict: VerdictAllowed, Rule: &e.rules.Rules[i]}
		}
	}
	return Decision{
		Verdict: VerdictPrivateDestination,
		Detail:  refusalDetail(target, VerdictPrivateDestination),
	}
}

var reservedDestinationPrefixes = []netip.Prefix{
	// "This network". Linux routes 0.0.0.0/8 to the local host, so it is a
	// second spelling of loopback rather than a routable destination.
	netip.MustParsePrefix("0.0.0.0/8"),
	// Carrier-grade NAT.
	netip.MustParsePrefix("100.64.0.0/10"),
}

// isReservedDestination classifies the address space a resolved name may not
// silently lead into: loopback, link-local, RFC1918 private space, CGNAT, IPv6
// ULA, and the unspecified and multicast ranges.
func isReservedDestination(addr netip.Addr) bool {
	if addr.IsLoopback() || addr.IsUnspecified() ||
		addr.IsMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		// netip.Addr.IsPrivate covers RFC1918 for IPv4 and the fc00::/7 ULA
		// range for IPv6.
		addr.IsPrivate() {
		return true
	}
	for _, prefix := range reservedDestinationPrefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// refusalDetail phrases a refusal in capability terms with a named remedy. It
// echoes only the normalized destination the evaluator itself derived, never
// raw client bytes.
func refusalDetail(target Target, verdict Verdict) string {
	destination := target.String()
	switch verdict {
	case VerdictDeniedByRule:
		return "tclaude filtering proxy refused " + destination +
			": this sandbox's network profile denies that destination. " +
			"Remedy: remove the matching network.deny entry from the profile, " +
			"or ask for a destination the profile allows."
	case VerdictNotAuthorized:
		return "tclaude filtering proxy refused " + destination +
			": this sandbox's network profile does not authorize that destination. " +
			"Remedy: add it to the profile's network allow list, or select a " +
			"network posture that allows it."
	case VerdictPrivateDestination:
		return "tclaude filtering proxy refused " + destination +
			": the destination resolves into loopback, link-local, or private " +
			"address space that this sandbox's network profile never authorized. " +
			`Remedy: author {"loopback": true} for host loopback, or an explicit ` +
			"network.allow cidr entry covering the address range."
	case VerdictUnresolvable:
		return "tclaude filtering proxy could not resolve " + destination +
			": the destination is authorized but has no usable address."
	default:
		return ""
	}
}
