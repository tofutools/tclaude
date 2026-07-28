package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"strings"
)

// MatchFilteredNetworkDNSName returns the authored host/domain rules whose DNS
// identity covers qname. Domain suffixes are label-bound: badexample.com never
// matches example.com.
func MatchFilteredNetworkDNSName(
	rules FilteredNetworkRuleSet,
	qname string,
) ([]FilteredNetworkRule, error) {
	if rules.ProtocolContract != FilteredNetworkProtocolContract {
		return nil, fmt.Errorf("filtered network protocol contract is invalid")
	}
	qname = strings.TrimSuffix(strings.TrimSpace(qname), ".")
	normalized, err := normalizeDNSName(qname)
	if err != nil {
		return nil, fmt.Errorf("filtered DNS query name %q: %w", qname, err)
	}
	matches := make([]FilteredNetworkRule, 0, 1)
	for i, rule := range rules.Rules {
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
	for _, rule := range rules.Rules {
		if rule.Selector == NetworkSelectorLoopback &&
			rule.Value == FilteredNetworkHostLoopbackName {
			return true
		}
	}
	return false
}

// FilteredNetworkCIDRCoversAddress lets the broker preserve CIDR-authored
// reachability for applications which name a destination instead of dialing an
// IP literal. No dynamic lease is needed: the static CIDR rule remains the
// packet authority and its authored ports still apply.
func FilteredNetworkCIDRCoversAddress(
	rules FilteredNetworkRuleSet,
	address netip.Addr,
) bool {
	if !address.IsValid() {
		return false
	}
	for _, rule := range rules.Rules {
		if rule.Selector != NetworkSelectorCIDR {
			continue
		}
		prefix, err := netip.ParsePrefix(rule.Value)
		if err == nil && prefix.Contains(address) {
			return true
		}
	}
	return false
}
