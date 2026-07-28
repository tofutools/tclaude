package harness

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

const (
	FilteredNetworkDNSIdentityCaveat = "Host/domain selectors are not enforced in M2b; they require the M2c DNS broker."
	FilteredNetworkLoopbackCaveat    = "Host loopback uses host.tclaude.internal; 127.0.0.1 and ::1 remain sandbox-private."
	FilteredNetworkPortDetail        = "TCP and UDP destination ports are enforced; QUIC is covered as UDP."
)

// FilteredNetworkRuleAssessment describes the honest target rating for one IR
// entry. It is control-plane data, not a capability flip: NetworkList remains
// EnforceNone until a launch adapter and its named CI smoke establish the
// corresponding data-plane claim.
type FilteredNetworkRuleAssessment struct {
	EntryIndex        int              `json:"entry_index"`
	Selector          string           `json:"selector"`
	DestinationLevel  EnforcementLevel `json:"destination_level"`
	DestinationDetail string           `json:"destination_detail"`
	PortLevel         EnforcementLevel `json:"port_level"`
	PortDetail        string           `json:"port_detail"`
}

// AssessFilteredNetworkRules gives future launch adapters one stable source for
// per-selector capability details. Host/domain identity remains None until the
// M2c DNS broker; synthetic host-loopback remains Partial.
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
			assessment.DestinationLevel = EnforceNone
			assessment.DestinationDetail = FilteredNetworkDNSIdentityCaveat
		case sandboxpolicy.NetworkSelectorCIDR:
			assessment.DestinationLevel = EnforceFull
			assessment.DestinationDetail =
				"IPv4/IPv6 CIDR destination identity is enforced for the authored TCP/UDP contract."
		case sandboxpolicy.NetworkSelectorLoopback:
			assessment.DestinationLevel = EnforcePartial
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
