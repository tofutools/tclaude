package sandboxpolicy

import "fmt"

// FilteredNetworkProtocolContract is the authored network-list contract. It is
// deliberately narrower than "all IP": profiles describe ordinary IPv4/IPv6
// TCP and UDP connections. QUIC is UDP. Portless rules may permit ICMP echo as
// a best-effort transport feature, but raw and packet sockets are not authored
// connection classes.
const FilteredNetworkProtocolContract = "ordinary IPv4/IPv6 TCP and UDP connections (QUIC is UDP; ICMP echo is best-effort for portless entries; raw and packet sockets are not an authored class)"

const FilteredNetworkHostLoopbackName = "host.tclaude.internal"

type NetworkSelectorKind string

const (
	NetworkSelectorHost     NetworkSelectorKind = "host"
	NetworkSelectorDomain   NetworkSelectorKind = "domain"
	NetworkSelectorCIDR     NetworkSelectorKind = "cidr"
	NetworkSelectorLoopback NetworkSelectorKind = "loopback"
)

// FilteredNetworkRuleSet is the normalized, harness-independent control-plane
// input for the future filtered gateway. It is inert until a launch adapter
// consumes it: compiling this IR does not establish enforcement.
type FilteredNetworkRuleSet struct {
	ProtocolContract string                `json:"protocol_contract"`
	Rules            []FilteredNetworkRule `json:"rules"`
}

// FilteredNetworkRule preserves the authored entry index so capability and
// launch disclosures can identify exactly which rule carries a caveat.
type FilteredNetworkRule struct {
	EntryIndex        int                 `json:"entry_index"`
	Selector          NetworkSelectorKind `json:"selector"`
	Value             string              `json:"value"`
	IncludeSubdomains bool                `json:"include_subdomains,omitempty"`
	Ports             []int               `json:"ports,omitempty"`
}

// CompileFilteredNetworkRules creates the stable gateway IR from a validated
// list. It reuses profile normalization so callers cannot smuggle an ambiguous
// selector into the future data plane.
func CompileFilteredNetworkRules(rules NetworkRules) (FilteredNetworkRuleSet, error) {
	normalized, err := normalizeNetworkRules(&rules)
	if err != nil {
		return FilteredNetworkRuleSet{}, err
	}
	if normalized.Mode != AccessModeList {
		return FilteredNetworkRuleSet{}, fmt.Errorf(
			`filtered network rules require network.mode "list" (got %q)`,
			normalized.Mode,
		)
	}
	out := FilteredNetworkRuleSet{
		ProtocolContract: FilteredNetworkProtocolContract,
		Rules:            make([]FilteredNetworkRule, 0, len(normalized.Allow)),
	}
	// Normalize each original row again so the IR retains authored indices.
	// normalizeNetworkRules sorts a complete list for stable policy storage;
	// that ordering is useful there but would make a persisted Entries warning
	// point at the wrong editor row here.
	for i, authored := range rules.Allow {
		single, singleErr := normalizeNetworkRules(&NetworkRules{
			Mode: AccessModeList, Allow: []NetworkAllowEntry{authored},
		})
		if singleErr != nil {
			return FilteredNetworkRuleSet{}, singleErr
		}
		entry := single.Allow[0]
		rule := FilteredNetworkRule{
			EntryIndex: i,
			Ports:      append([]int(nil), entry.Ports...),
		}
		switch {
		case entry.Host != "":
			rule.Selector = NetworkSelectorHost
			rule.Value = entry.Host
		case entry.Domain != "":
			rule.Selector = NetworkSelectorDomain
			rule.Value = entry.Domain
			rule.IncludeSubdomains = entry.IncludeSubdomains
		case entry.CIDR != "":
			rule.Selector = NetworkSelectorCIDR
			rule.Value = entry.CIDR
		case entry.Loopback:
			rule.Selector = NetworkSelectorLoopback
			rule.Value = FilteredNetworkHostLoopbackName
		default:
			return FilteredNetworkRuleSet{}, fmt.Errorf(
				"network.allow[%d] has no destination selector", i,
			)
		}
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}
