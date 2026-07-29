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
	FilteredNetworkGatewayIPv6    = "fe80::1"
	FilteredNetworkDNSIPv4        = "127.0.0.53"
	FilteredNetworkBootstrapPath  = "/tmp/.tclaude-filtered-bootstrap"
	FilteredNetworkNFTPolicyPath  = "/tmp/.tclaude-filtered-policy.nft"
	FilteredNetworkBootstrapReady = "filtered-network-policy-ready"
	FilteredNetworkDNSSetSize     = 4096
)

// The batch starts from a fresh private namespace and is applied with nft -f,
// which commits the complete ruleset atomically. The output base chain is
// default-drop. Sandbox-private loopback remains available; the only path to
// host loopback is the fixed pasta mapping guarded by an authored loopback rule.
// Host/domain entries use empty, bounded named sets at launch. The outer DNS
// broker may add only TTL-bound elements through the namespace-owned netfilter
// socket handed out by the trusted bootstrap.
func RenderFilteredNetworkNFT(rules FilteredNetworkRuleSet) (string, error) {
	if rules.ProtocolContract != FilteredNetworkProtocolContract {
		return "", fmt.Errorf("filtered network protocol contract is invalid")
	}
	var body strings.Builder
	body.WriteString("flush ruleset\n")
	body.WriteString("table inet " + FilteredNetworkNFTTable + " {\n")
	seenEntryIndexes := make(map[int]struct{}, len(rules.Rules))
	for i, rule := range rules.Rules {
		if rule.EntryIndex < 0 {
			return "", fmt.Errorf("filtered network rule %d has invalid authored index", i)
		}
		if _, exists := seenEntryIndexes[rule.EntryIndex]; exists {
			return "", fmt.Errorf(
				"filtered network rule %d repeats authored index %d",
				i, rule.EntryIndex,
			)
		}
		seenEntryIndexes[rule.EntryIndex] = struct{}{}
		if rule.Selector != NetworkSelectorHost &&
			rule.Selector != NetworkSelectorDomain {
			continue
		}
		ipv4Set, ipv6Set := FilteredNetworkDNSSetNames(rule.EntryIndex)
		writeFilteredDNSSet(&body, ipv4Set, "ipv4_addr")
		writeFilteredDNSSet(&body, ipv6Set, "ipv6_addr")
	}
	body.WriteString("  chain output {\n")
	body.WriteString("    type filter hook output priority filter; policy drop;\n")
	body.WriteString("    oifname \"lo\" accept\n")
	// Lease expiry stops new flows without tearing down traffic admitted while
	// the lease was valid. Deliberately omit "related": protocol-created side
	// channels must satisfy authored destination and port rules of their own.
	body.WriteString("    ct state established accept\n")
	// IPv6 needs router and neighbor discovery to configure and reach pasta's
	// tap gateway. Limit that control plane to link-local destinations and
	// link-local multicast with the hop limit required by RFC 4861.
	body.WriteString("    ip6 daddr { fe80::/10, ff02::/16 } ip6 hoplimit 255 icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	for i, rule := range rules.Rules {
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
			if strings.TrimSpace(rule.Value) == "" {
				return "", fmt.Errorf(
					"filtered network rule %d selector %q has no DNS identity",
					i, rule.Selector,
				)
			}
			ipv4Set, ipv6Set := FilteredNetworkDNSSetNames(rule.EntryIndex)
			writeFilteredNFTSetRule(&body, true, ipv4Set, rule.Ports)
			writeFilteredNFTSetRule(&body, false, ipv6Set, rule.Ports)
		default:
			return "", fmt.Errorf("filtered network rule %d has unknown selector %q", i, rule.Selector)
		}
	}
	body.WriteString("  }\n")
	body.WriteString("}\n")
	return body.String(), nil
}

// FilteredNetworkDNSSetNames is the single naming contract shared by the
// launch-time nft renderer and the outer DNS broker.
func FilteredNetworkDNSSetNames(entryIndex int) (ipv4, ipv6 string) {
	return "dns4_" + strconv.Itoa(entryIndex), "dns6_" + strconv.Itoa(entryIndex)
}

func writeFilteredDNSSet(body *strings.Builder, name, setType string) {
	body.WriteString("  set " + name + " {\n")
	body.WriteString("    type " + setType + "\n")
	body.WriteString("    flags timeout\n")
	body.WriteString("    size " + strconv.Itoa(FilteredNetworkDNSSetSize) + "\n")
	body.WriteString("  }\n")
}

func writeFilteredNFTRule(body *strings.Builder, ipv4 bool, destination string, ports []int) {
	family := "ip6"
	if ipv4 {
		family = "ip"
	} else {
		// Neighbor Unreachability Detection can probe an on-link neighbor by
		// unicast after initial multicast discovery. Keep that IPv6 control
		// traffic within the authored destination, independent of port.
		body.WriteString("    ip6 daddr " + destination + " ip6 hoplimit 255 icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept\n")
	}
	for _, protocol := range []string{"tcp", "udp"} {
		body.WriteString("    " + family + " daddr " + destination + " ")
		if len(ports) > 0 {
			body.WriteString(protocol + " dport { ")
			for i, port := range ports {
				if i > 0 {
					body.WriteString(", ")
				}
				body.WriteString(strconv.Itoa(port))
			}
			body.WriteString(" }")
		} else {
			// A bare "tcp" or "udp" expression is invalid nft syntax.
			// meta l4proto selects that protocol without adding a port
			// restriction, preserving the contract's TCP/UDP-only floor.
			body.WriteString("meta l4proto " + protocol)
		}
		body.WriteString(" accept\n")
	}
}

func writeFilteredNFTSetRule(body *strings.Builder, ipv4 bool, setName string, ports []int) {
	family := "ip6"
	if ipv4 {
		family = "ip"
	}
	for _, protocol := range []string{"tcp", "udp"} {
		body.WriteString("    " + family + " daddr @" + setName + " ")
		if len(ports) > 0 {
			body.WriteString(protocol + " dport { ")
			for i, port := range ports {
				if i > 0 {
					body.WriteString(", ")
				}
				body.WriteString(strconv.Itoa(port))
			}
			body.WriteString(" }")
		} else {
			body.WriteString("meta l4proto " + protocol)
		}
		body.WriteString(" accept\n")
	}
}

// FilteredNetworkHostsFile renders only sandbox-private localhost plus the
// synthetic host-loopback mapping. Arbitrary host mappings are resolved by the
// outer DNS broker; copying them here would bypass its TTL leases and loopback
// rebinding check.
func FilteredNetworkHostsFile(hostHosts []byte) ([]byte, error) {
	if len(hostHosts) > 1<<20 {
		return nil, fmt.Errorf("host /etc/hosts exceeds the filtered-network limit")
	}
	for _, line := range strings.Split(string(hostHosts), "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) < 2 {
			continue
		}
		for _, alias := range fields[1:] {
			if strings.EqualFold(alias, FilteredNetworkHostLoopbackName) {
				return nil, fmt.Errorf(
					"host /etc/hosts already defines reserved filtered-network name %s; remove that host mapping before launch",
					FilteredNetworkHostLoopbackName,
				)
			}
		}
	}
	return []byte(
		"127.0.0.1 localhost\n" +
			"::1 localhost ip6-localhost ip6-loopback\n" +
			FilteredNetworkLoopbackIPv4 + " " + FilteredNetworkHostLoopbackName + "\n" +
			FilteredNetworkLoopbackIPv6 + " " + FilteredNetworkHostLoopbackName + "\n",
	), nil
}

// FilteredNetworkResolvConf makes the broker authoritative inside the private
// namespace. Only sandbox-private loopback is named, so no packet can bypass
// the broker through pasta or a host resolver address.
func FilteredNetworkResolvConf() []byte {
	return []byte(
		"nameserver " + FilteredNetworkDNSIPv4 + "\n" +
			"options timeout:2 attempts:2\n",
	)
}
