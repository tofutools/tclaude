package harness

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	FilteredNetworkDNSIdentityCaveat = "Host and domain rules allow IP addresses returned by DNS. The sandbox can also reach other sites hosted on that same IP until the DNS answer expires."
	FilteredNetworkDNSLeaseCaveat    = "Only a new DNS lookup refreshes the allowed IP. Existing connections may continue after the DNS answer expires; new connections need another lookup."
	FilteredNetworkLoopbackCaveat    = "Local-machine rules use host.tclaude.internal. Inside the sandbox, 127.0.0.1 and ::1 refer to the sandbox itself."
	FilteredNetworkPortDetail        = "TCP and UDP destination ports are enforced; QUIC is covered as UDP."
)

// The Darwin native host-loopback filter's disclosure. It is separate from the
// FilteredNetwork* strings above because the mechanism is different: those
// describe the Linux packet gateway's synthetic host.tclaude.internal mapping,
// while Darwin's Local access reaches the REAL host loopback through a Seatbelt
// rule.
//
// WHY THE SELECTOR IS PARTIAL AND NOT FULL. It shipped at Full from TCL-833
// (#1688) until TCL-927. TCL-917 measured on this exact path
// (appendSeatbeltLoopbackNetworkRules; run 30691418550, job 91346704723) that a
// service listening on an AUTHORED PORT at a NON-LOOPBACK address of the same
// machine is reachable from inside the sandbox. The rule cannot be written any
// other way: SBPL's grammar accepts only `*` or `localhost` as the host term,
// `localhost` means every address assigned to the host, and a literal IP is
// rejected when the profile is parsed ("host must be * or localhost in network
// address"). So the mechanism cannot express "this port, loopback only" at all.
//
// NOT NONE EITHER. Real enforcement happens and is measured: a port outside the
// authored list is refused with EPERM, and an outbound TCP connect to an
// external destination is refused. Only the ADDRESS axis is unenforced, which
// is why the ports cell stays Full and this one moves to Partial.
//
// BIND IS NOT RESTRICTED ON THIS POSTURE, and an earlier draft of the string
// below claimed the opposite. `(deny network-bind)` is emitted only by
// appendSeatbeltIsolatedNetworkRules; the loopback path deliberately leaves
// bind alone so local services and the IDE bridge keep working
// (sandbox_seatbelt_test.go asserts the profile does NOT contain it, and the
// Darwin Local smoke requires net.Listen to SUCCEED). The false claim came from
// importing a property measured on the ISOLATED posture into this one — the
// same held-parameter mistake TCL-917 was about. Do not restore it: an operator
// must not be told the precondition is one the sandbox cannot arrange.
//
// SCOPE OF THE MEASUREMENT. TCP. External UDP was measured on neither Seatbelt
// path, so the operator-facing strings below say TCP rather than claiming IP
// generally — the policy recorded in docs/proxy-network-filtering.md. See the
// same note at appendSeatbeltLoopbackNetworkRules.
//
// WIDENING IS NOT DETECTABLE. No test would notice if Apple made `localhost`
// match addresses beyond this machine: the external-destination assertion moves
// the address AND the port together, so it is refused on port alone even under
// a widened host predicate. The strings below therefore describe what is
// refused TODAY without implying the boundary is monitored.
//
// NOT MITIGATED, DELIBERATELY. The operator ruled document-and-disclose and
// ruled AGAINST a launch-time port-collision check on both Seatbelt paths
// (TCL-917). Do not re-propose one here.
const (
	// SeatbeltNativeLoopbackSelectorDetail is the per-selector disclosure. It
	// names the escape and its precondition, so an operator can judge their own
	// exposure rather than discovering it.
	//
	// It covers BOTH authored shapes. A rule naming ports is enforced on those
	// ports; a rule naming NO ports takes the allowAllPorts path and emits
	// `(allow network-outbound (remote ip "localhost:*"))`, which is every port
	// on every address of this machine. Saying only "the authored ports are
	// enforced" would be false for that shape, which is the net-local preset.
	SeatbeltNativeLoopbackSelectorDetail = "Local-machine rules are not confined to loopback. " +
		"macOS sandbox rules accept only \"localhost\" as the host, which means every address assigned to this machine, " +
		"so a service listening on an allowed port at another of this machine's addresses — its LAN address, for example — is reachable from the sandbox as well. " +
		"A rule that names ports allows only those ports; a rule that names no ports allows every port on this machine. " +
		"Outbound TCP to destinations off this machine is refused, as is a port outside the list."

	// SeatbeltNativeLoopbackCondition carries the same escape on the ROW, so an
	// operator reading network.list learns it without opening the selector.
	SeatbeltNativeLoopbackCondition = "Local-machine rules are not confined to loopback: " +
		"a service listening on an allowed port at another of this machine's addresses is reachable from the sandbox as well. " +
		"A rule that names no ports allows every port on this machine."
)

func filteredNetworkDNSCaveat() string {
	return FilteredNetworkDNSIdentityCaveat + " " + FilteredNetworkDNSLeaseCaveat
}

// FilteredNetworkRuleAssessment describes the honest target rating for one IR
// entry. It is control-plane data, not a capability flip: NetworkList remains
// EnforceNone until a launch adapter and its named CI smoke establish the
// corresponding data-plane claim. DestinationDetail remains informational
// when DestinationLevel is Full.
type FilteredNetworkRuleAssessment struct {
	EntryIndex        int              `json:"entry_index"`
	Selector          string           `json:"selector"`
	DestinationLevel  EnforcementLevel `json:"destination_level"`
	DestinationDetail string           `json:"destination_detail"`
	PortLevel         EnforcementLevel `json:"port_level"`
	PortDetail        string           `json:"port_detail"`
}

// AssessFilteredNetworkRules gives future launch adapters one stable source for
// per-selector capability details. DNS-to-IP is the strongest enforceable
// name boundary for arbitrary TCP/UDP, and the synthetic host-loopback mapping
// enforces its authored destination and ports. Their caveats remain
// informational details rather than downgrading the capability rating.
func AssessFilteredNetworkRules(
	rules sandboxpolicy.FilteredNetworkRuleSet,
) ([]FilteredNetworkRuleAssessment, EnforcementLevel, error) {
	if strings.TrimSpace(rules.ProtocolContract) != sandboxpolicy.FilteredNetworkProtocolContract {
		return nil, EnforceNone, fmt.Errorf("filtered network rule set has an unknown protocol contract")
	}
	out := make([]FilteredNetworkRuleAssessment, 0, len(rules.Rules))
	aggregate := EnforceFull
	for _, rule := range rules.Rules {
		assessment := FilteredNetworkRuleAssessment{
			EntryIndex: rule.EntryIndex,
			Selector:   string(rule.Selector),
			PortLevel:  EnforceFull,
			PortDetail: FilteredNetworkPortDetail,
		}
		switch rule.Selector {
		case sandboxpolicy.NetworkSelectorHost, sandboxpolicy.NetworkSelectorDomain:
			assessment.DestinationLevel = EnforceFull
			assessment.DestinationDetail = filteredNetworkDNSCaveat()
		case sandboxpolicy.NetworkSelectorCIDR:
			assessment.DestinationLevel = EnforceFull
			assessment.DestinationDetail =
				"IPv4/IPv6 CIDR destination identity is enforced for the authored TCP/UDP contract."
		case sandboxpolicy.NetworkSelectorLoopback:
			assessment.DestinationLevel = EnforceFull
			assessment.DestinationDetail = FilteredNetworkLoopbackCaveat
		default:
			return nil, EnforceNone, fmt.Errorf(
				"filtered network rule %d has unknown selector %q",
				rule.EntryIndex, rule.Selector,
			)
		}
		if assessment.DestinationLevel < aggregate {
			aggregate = assessment.DestinationLevel
		}
		out = append(out, assessment)
	}
	return out, aggregate, nil
}
