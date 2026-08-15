package sandboxpolicy

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

const (
	FilteredNetworkNFTTable     = "tclaude_filter"
	FilteredNetworkLoopbackIPv4 = "169.254.2.2"
	FilteredNetworkLoopbackIPv6 = "fd00::2"
	FilteredNetworkGatewayIPv6  = "fe80::1"
	FilteredNetworkDNSIPv4      = "127.0.0.53"
	// FilteredNetworkDNSListenerPort is deliberately unprivileged. resolv.conf
	// still targets port 53; the namespace-local nft output hook redirects only
	// that private resolver address to this listener before filtering.
	FilteredNetworkDNSListenerPort = 1053
	FilteredNetworkBootstrapPath   = "/tmp/.tclaude-filtered-bootstrap"
	FilteredNetworkNFTPolicyPath   = "/tmp/.tclaude-filtered-policy.nft"
	FilteredNetworkBootstrapReady  = "filtered-network-policy-ready"
	FilteredNetworkDNSSetSize      = 4096
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
	defaultVerdict := FilteredNetworkDefaultVerdictForRules(rules)
	if defaultVerdict != FilteredNetworkDefaultDrop &&
		defaultVerdict != FilteredNetworkDefaultAccept {
		return "", fmt.Errorf("filtered network default verdict %q is invalid",
			defaultVerdict)
	}
	var body strings.Builder
	body.WriteString("flush ruleset\n")
	body.WriteString("table inet " + FilteredNetworkNFTTable + " {\n")
	body.WriteString("  chain dns_redirect {\n")
	body.WriteString("    type nat hook output priority dstnat; policy accept;\n")
	body.WriteString("    ip daddr " + FilteredNetworkDNSIPv4 +
		" udp dport 53 dnat ip to " + FilteredNetworkDNSIPv4 + ":" +
		strconv.Itoa(FilteredNetworkDNSListenerPort) + "\n")
	body.WriteString("    ip daddr " + FilteredNetworkDNSIPv4 +
		" tcp dport 53 dnat ip to " + FilteredNetworkDNSIPv4 + ":" +
		strconv.Itoa(FilteredNetworkDNSListenerPort) + "\n")
	body.WriteString("  }\n")
	for _, polarity := range []struct {
		deny  bool
		rules []FilteredNetworkRule
	}{
		{rules: rules.Rules},
		{deny: true, rules: rules.DenyRules},
	} {
		seenEntryIndexes := make(map[int]struct{}, len(polarity.rules))
		for i, rule := range polarity.rules {
			if rule.EntryIndex < 0 {
				return "", fmt.Errorf("filtered network rule %d has invalid stable index", i)
			}
			if _, exists := seenEntryIndexes[rule.EntryIndex]; exists {
				return "", fmt.Errorf(
					"filtered network rule %d repeats stable index %d",
					i, rule.EntryIndex,
				)
			}
			seenEntryIndexes[rule.EntryIndex] = struct{}{}
			if rule.Selector != NetworkSelectorHost &&
				rule.Selector != NetworkSelectorDomain {
				continue
			}
			ipv4Set, ipv6Set := FilteredNetworkDNSSetNamesForRule(
				polarity.deny, rule.EntryIndex)
			writeFilteredDNSSet(&body, ipv4Set, "ipv4_addr")
			writeFilteredDNSSet(&body, ipv6Set, "ipv6_addr")
		}
	}
	body.WriteString("  chain output {\n")
	body.WriteString("    type filter hook output priority filter; policy " +
		string(defaultVerdict) + ";\n")
	body.WriteString("    oifname \"lo\" accept\n")
	// IPv6 needs router and neighbor discovery to configure and reach pasta's
	// tap gateway. Limit that control plane to link-local destinations and
	// link-local multicast with the hop limit required by RFC 4861.
	ndRule := "    ip6 daddr { fe80::/10, ff02::/16 } ip6 hoplimit 255 icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept\n"
	if len(rules.DenyRules) > 0 {
		body.WriteString(ndRule)
		if err := writeFilteredNFTRules(&body, rules.DenyRules, true); err != nil {
			return "", err
		}
	}
	if rules.BlockHostLoopback {
		body.WriteString("    ip daddr " + FilteredNetworkLoopbackIPv4 + "/32 drop\n")
		body.WriteString("    ip6 daddr " + FilteredNetworkLoopbackIPv6 + "/128 drop\n")
	}
	// Denies precede established-flow acceptance so a fresh negative DNS lease
	// cuts matching established TCP and UDP authority immediately.
	body.WriteString("    ct state established accept\n")
	if len(rules.DenyRules) == 0 {
		body.WriteString(ndRule)
	}
	if err := writeFilteredNFTRules(&body, rules.Rules, false); err != nil {
		return "", err
	}
	body.WriteString("  }\n")
	body.WriteString("}\n")
	return body.String(), nil
}

func writeFilteredNFTRules(
	body *strings.Builder,
	rules []FilteredNetworkRule,
	deny bool,
) error {
	action := "accept"
	if deny {
		action = "drop"
	}
	for i, rule := range rules {
		for portIndex, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("filtered network rule %d has invalid port %d", i, port)
			}
			if portIndex > 0 && rule.Ports[portIndex-1] >= port {
				return fmt.Errorf("filtered network rule %d ports are not canonical", i)
			}
		}
		switch rule.Selector {
		case NetworkSelectorCIDR:
			prefix, err := netip.ParsePrefix(rule.Value)
			if err != nil {
				return fmt.Errorf("filtered network rule %d CIDR: %w", i, err)
			}
			if prefix.String() != rule.Value {
				return fmt.Errorf("filtered network rule %d CIDR is not canonical", i)
			}
			writeFilteredNFTRule(body, prefix.Addr().Is4(), rule.Value, rule.Ports, action)
		case NetworkSelectorLoopback:
			if rule.Value != FilteredNetworkHostLoopbackName {
				return fmt.Errorf("filtered network rule %d has invalid loopback identity", i)
			}
			writeFilteredNFTRule(body, true, FilteredNetworkLoopbackIPv4+"/32", rule.Ports, action)
			writeFilteredNFTRule(body, false, FilteredNetworkLoopbackIPv6+"/128", rule.Ports, action)
		case NetworkSelectorHost, NetworkSelectorDomain:
			if strings.TrimSpace(rule.Value) == "" {
				return fmt.Errorf(
					"filtered network rule %d selector %q has no DNS identity",
					i, rule.Selector,
				)
			}
			ipv4Set, ipv6Set := FilteredNetworkDNSSetNamesForRule(
				deny, rule.EntryIndex)
			writeFilteredNFTSetRule(body, true, ipv4Set, rule.Ports, action)
			writeFilteredNFTSetRule(body, false, ipv6Set, rule.Ports, action)
		default:
			return fmt.Errorf("filtered network rule %d has unknown selector %q", i, rule.Selector)
		}
	}
	return nil
}

// FilteredNetworkDNSSetNames is the single naming contract shared by the
// launch-time nft renderer and the outer DNS broker.
func FilteredNetworkDNSSetNames(entryIndex int) (ipv4, ipv6 string) {
	return "dns4_" + strconv.Itoa(entryIndex), "dns6_" + strconv.Itoa(entryIndex)
}

// FilteredNetworkDNSSetNamesForRule namespaces deny leases independently while
// preserving the legacy allow-only naming contract.
func FilteredNetworkDNSSetNamesForRule(
	deny bool,
	entryIndex int,
) (ipv4, ipv6 string) {
	if !deny {
		return FilteredNetworkDNSSetNames(entryIndex)
	}
	return "dns4_d_" + strconv.Itoa(entryIndex),
		"dns6_d_" + strconv.Itoa(entryIndex)
}

func writeFilteredDNSSet(body *strings.Builder, name, setType string) {
	body.WriteString("  set " + name + " {\n")
	body.WriteString("    type " + setType + "\n")
	body.WriteString("    flags timeout\n")
	body.WriteString("    size " + strconv.Itoa(FilteredNetworkDNSSetSize) + "\n")
	body.WriteString("  }\n")
}

func writeFilteredNFTRule(
	body *strings.Builder,
	ipv4 bool,
	destination string,
	ports []int,
	action string,
) {
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
		body.WriteString(" " + action + "\n")
	}
}

func writeFilteredNFTSetRule(
	body *strings.Builder,
	ipv4 bool,
	setName string,
	ports []int,
	action string,
) {
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
		body.WriteString(" " + action + "\n")
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

// ProxyNetworkHostsFile is the proxy engine's namespace hosts file: the
// loopback names a process needs to talk to itself, and nothing else.
//
// Every host-derived mapping is dropped, and that is a policy decision rather
// than tidiness. The proxy engine authorizes destinations by NAME — a host row,
// a domain row, a deny row — and it can only do that for names the sandbox
// hands it instead of resolving itself. A namespace that inherited the host's
// /etc/hosts would let a process turn any mapped name into an address literal
// without any query leaving it, and a literal is matched against CIDR selectors
// only. An authored deny on that name would then have nothing to match, so a
// refused name would become a permitted literal wherever some CIDR row covers
// its address.
//
// The packet engine reaches the same place by a different route: it synthesizes
// its own hosts file too, and its DNS broker holds the name authority.
//
// Only /etc/hosts is replaced, because it is the one resolution path the FLOOR
// ITSELF provides that needs no network. The resolver configuration is left
// alone: a query has nowhere to go in an empty namespace, the socket-backed NSS
// modules (systemd-resolved, nscd, sssd) live under /run, which the constructed
// root does not bind, and a resolv.conf naming a loopback stub can only reach
// the sandbox's own loopback — where the floor grants no capabilities and no
// namespace root, so nothing can bind port 53 to answer.
//
// What this does NOT cover, stated because it is the boundary of the guarantee:
// the sandbox inherits the host's /etc/nsswitch.conf and NSS modules, so an
// operator who hands a resolver socket back to the sandbox restores exactly the
// name-to-literal conversion this file exists to prevent. Two authored axes can
// do that, and both are refused at the capability surface, where an authored
// engine first meets the rest of the policy: NetworkEngineResolverSocketConflict
// on the unix_sockets axis, and NetworkEngineResolverFilesystemConflict on the
// filesystem axis, over ONE list of known resolver paths (TCL-883).
//
// What remains outside the guarantee is a resolver that list does not know —
// a private NSS module over a socket an operator builds themselves. The list
// refuses the resolvers a real host ships; it is not a proof of exhaustiveness,
// and this boundary is stated rather than claimed away.
//
// The delivered property is therefore "no automatic name-to-address conversion",
// not "no host-derived address knowledge": /etc/resolv.conf and friends remain
// readable and still disclose host addresses. Disclosure is not authorization —
// a literal the sandbox learns still has to pass the proxy's CIDR rows.
func ProxyNetworkHostsFile() []byte {
	return []byte(
		"127.0.0.1 localhost\n" +
			"::1 localhost ip6-localhost ip6-loopback\n",
	)
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
