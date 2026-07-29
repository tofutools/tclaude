package sandboxpolicy

import (
	"fmt"
	"strings"
)

type FilteredNetworkDNSMatches struct {
	Allow []FilteredNetworkRule
	Deny  []FilteredNetworkRule
}

// MatchFilteredNetworkDNSName returns the authored host/domain rules whose DNS
// identity covers qname. Domain suffixes are label-bound: badexample.com never
// matches example.com.
func MatchFilteredNetworkDNSName(
	rules FilteredNetworkRuleSet,
	qname string,
) ([]FilteredNetworkRule, error) {
	matches, err := MatchFilteredNetworkDNSPolicy(rules, qname)
	return matches.Allow, err
}

// MatchFilteredNetworkDNSPolicy returns both matching polarities. Callers must
// apply deny before allow regardless of authoring or match order.
func MatchFilteredNetworkDNSPolicy(
	rules FilteredNetworkRuleSet,
	qname string,
) (FilteredNetworkDNSMatches, error) {
	if rules.ProtocolContract != FilteredNetworkProtocolContract {
		return FilteredNetworkDNSMatches{},
			fmt.Errorf("filtered network protocol contract is invalid")
	}
	qname = strings.TrimSuffix(strings.TrimSpace(qname), ".")
	normalized, err := normalizeDNSName(qname)
	if err != nil {
		return FilteredNetworkDNSMatches{},
			fmt.Errorf("filtered DNS query name %q: %w", qname, err)
	}
	allow, err := matchFilteredNetworkDNSRules(rules.Rules, normalized)
	if err != nil {
		return FilteredNetworkDNSMatches{}, err
	}
	deny, err := matchFilteredNetworkDNSRules(rules.DenyRules, normalized)
	if err != nil {
		return FilteredNetworkDNSMatches{}, err
	}
	return FilteredNetworkDNSMatches{Allow: allow, Deny: deny}, nil
}

func matchFilteredNetworkDNSRules(
	rules []FilteredNetworkRule,
	normalized string,
) ([]FilteredNetworkRule, error) {
	matches := make([]FilteredNetworkRule, 0, 1)
	for i, rule := range rules {
		switch rule.Selector {
		case NetworkSelectorHost:
			if rule.Value == normalized {
				matches = append(matches, rule)
			}
		case NetworkSelectorDomain:
			if rule.Value == normalized ||
				(rule.IncludeSubdomains &&
					strings.HasSuffix(normalized, "."+rule.Value)) {
				matches = append(matches, rule)
			}
		case NetworkSelectorCIDR, NetworkSelectorLoopback:
			continue
		default:
			return nil, fmt.Errorf(
				"filtered network rule %d has unknown selector %q",
				i, rule.Selector,
			)
		}
	}
	return matches, nil
}

// FilteredNetworkAllowsDNSLoopbackAnswer reports whether the operator
// explicitly authored host-loopback reachability. The DNS broker still rewrites
// loopback answers to the synthetic host mapping; the static loopback nft rule
// remains the port authority.
func FilteredNetworkAllowsDNSLoopbackAnswer(rules FilteredNetworkRuleSet) bool {
	for _, rule := range rules.DenyRules {
		if rule.Selector == NetworkSelectorLoopback &&
			rule.Value == FilteredNetworkHostLoopbackName {
			return false
		}
	}
	for _, rule := range rules.Rules {
		if rule.Selector == NetworkSelectorLoopback &&
			rule.Value == FilteredNetworkHostLoopbackName {
			return true
		}
	}
	return false
}
