package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const (
	FilteredNetworkNFTTable       = "tclaude_filter"
	FilteredNetworkLoopbackIPv4   = "169.254.2.2"
	FilteredNetworkLoopbackIPv6   = "fd00::2"
	FilteredNetworkBootstrapPath  = "/tmp/.tclaude-filtered-bootstrap"
	FilteredNetworkNFTPolicyPath  = "/tmp/.tclaude-filtered-policy.nft"
	FilteredNetworkBootstrapReady = "filtered-network-policy-ready"
)

// RenderFilteredNetworkNFT renders the M2b packet-filter subset. Host and
// domain entries deliberately refuse here: DNS-to-IP leases belong to M2c,
// and dropping those entries would narrow the authored policy.
//
// The batch starts from a fresh private namespace and is applied with nft -f,
// which commits the complete ruleset atomically. The output base chain is
// default-drop. Sandbox-private loopback remains available; the only path to
// host loopback is the fixed pasta mapping guarded by an authored loopback
// rule.
func RenderFilteredNetworkNFT(rules FilteredNetworkRuleSet) (string, error) {
	if rules.ProtocolContract != FilteredNetworkProtocolContract {
		return "", fmt.Errorf("filtered network protocol contract is invalid")
	}
	var body strings.Builder
	body.WriteString("flush ruleset\n")
	body.WriteString("table inet " + FilteredNetworkNFTTable + " {\n")
	body.WriteString("  chain output {\n")
	body.WriteString("    type filter hook output priority filter; policy drop;\n")
	body.WriteString("    oifname \"lo\" accept\n")
	// IPv6 needs neighbor discovery to reach pasta's tap gateway. These are
	// link-local control packets, not an application egress carve-out.
	body.WriteString("    ip6 daddr fe80::/10 icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit, nd-router-advert } accept\n")
	for i, rule := range rules.Rules {
		if rule.EntryIndex < 0 {
			return "", fmt.Errorf("filtered network rule %d has invalid authored index", i)
		}
		for portIndex, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return "", fmt.Errorf("filtered network rule %d has invalid port %d", i, port)
			}
			if portIndex > 0 && rule.Ports[portIndex-1] >= port {
				return "", fmt.Errorf("filtered network rule %d ports are not canonical", i)
			}
		}
		switch rule.Selector {
		case NetworkSelectorCIDR:
			prefix, err := netip.ParsePrefix(rule.Value)
			if err != nil {
				return "", fmt.Errorf("filtered network rule %d CIDR: %w", i, err)
			}
			if prefix.String() != rule.Value {
				return "", fmt.Errorf("filtered network rule %d CIDR is not canonical", i)
			}
			writeFilteredNFTRule(&body, prefix.Addr().Is4(), rule.Value, rule.Ports)
		case NetworkSelectorLoopback:
			if rule.Value != FilteredNetworkHostLoopbackName {
				return "", fmt.Errorf("filtered network rule %d has invalid loopback identity", i)
			}
			writeFilteredNFTRule(&body, true, FilteredNetworkLoopbackIPv4+"/32", rule.Ports)
			writeFilteredNFTRule(&body, false, FilteredNetworkLoopbackIPv6+"/128", rule.Ports)
		case NetworkSelectorHost, NetworkSelectorDomain:
			return "", fmt.Errorf(
				"filtered network rule %d selector %q requires the M2c DNS broker",
				i, rule.Selector,
			)
		default:
			return "", fmt.Errorf("filtered network rule %d has unknown selector %q", i, rule.Selector)
		}
	}
	body.WriteString("  }\n")
	body.WriteString("}\n")
	return body.String(), nil
}

func writeFilteredNFTRule(body *strings.Builder, ipv4 bool, destination string, ports []int) {
	family := "ip6"
	if ipv4 {
		family = "ip"
	}
	for _, protocol := range []string{"tcp", "udp"} {
		body.WriteString("    " + family + " daddr " + destination + " " + protocol)
		if len(ports) > 0 {
			body.WriteString(" dport { ")
			for i, port := range ports {
				if i > 0 {
					body.WriteString(", ")
				}
				body.WriteString(strconv.Itoa(port))
			}
			body.WriteString(" }")
		}
		body.WriteString(" accept\n")
	}
}

// FilteredNetworkHostsFile overlays only the synthetic host-loopback mapping.
// The caller supplies the host file captured at the trusted outer boundary so
// normal localhost and operator mappings survive.
func FilteredNetworkHostsFile(hostHosts []byte) ([]byte, error) {
	if len(hostHosts) > 1<<20 {
		return nil, fmt.Errorf("host /etc/hosts exceeds the filtered-network limit")
	}
	out := append([]byte(nil), hostHosts...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, []byte(
		FilteredNetworkLoopbackIPv4+" "+FilteredNetworkHostLoopbackName+"\n"+
			FilteredNetworkLoopbackIPv6+" "+FilteredNetworkHostLoopbackName+"\n",
	)...)
	return out, nil
}
