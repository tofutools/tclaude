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
