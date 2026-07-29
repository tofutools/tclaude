package sandboxpolicy

import (
	"fmt"
	"sort"
)

// FilteredNetworkProtocolContract is the authored network-list contract. It is
// deliberately narrower than "all IP": profiles describe ordinary IPv4/IPv6
// TCP and UDP connections. QUIC is UDP. Raw and packet sockets are not
// authored connection classes.
const FilteredNetworkProtocolContract = "ordinary IPv4/IPv6 TCP and UDP connections (QUIC is UDP; raw and packet sockets are not an authored class)"

const FilteredNetworkHostLoopbackName = "host.tclaude.internal"

type FilteredNetworkDefaultVerdict string

const (
	FilteredNetworkDefaultDrop   FilteredNetworkDefaultVerdict = "drop"
	FilteredNetworkDefaultAccept FilteredNetworkDefaultVerdict = "accept"
)

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
	ProtocolContract string `json:"protocol_contract"`
	// DefaultVerdict is omitted by legacy allow-only relays. An absent value
	// therefore means drop forever.
	DefaultVerdict FilteredNetworkDefaultVerdict `json:"default_verdict,omitempty"`
	Rules          []FilteredNetworkRule         `json:"rules"`
	DenyRules      []FilteredNetworkRule         `json:"deny_rules,omitempty"`
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

// NetworkRulesAreLoopbackOnly identifies the one filtered-list shape Darwin
// can enforce natively without a proxy. The list must be non-empty: an empty
// list is "nothing allowed", not the editor's Local access preset.
func NetworkRulesAreLoopbackOnly(rules NetworkRules) bool {
	if rules.Mode != AccessModeList || len(rules.Allow) == 0 {
		return false
	}
	for _, entry := range rules.Allow {
		if !entry.Loopback || entry.Host != "" || entry.Domain != "" || entry.CIDR != "" {
			return false
		}
	}
	return true
}

// FilteredNetworkRulesAreLoopbackOnly is the launch-IR equivalent of
// NetworkRulesAreLoopbackOnly. Appliers use it as a final fail-closed check:
// capability planning may select Darwin's native path only for this shape.
func FilteredNetworkRulesAreLoopbackOnly(rules *FilteredNetworkRuleSet) bool {
	if rules == nil || rules.ProtocolContract != FilteredNetworkProtocolContract ||
		len(rules.Rules) == 0 || len(rules.DenyRules) > 0 ||
		FilteredNetworkDefaultVerdictForRules(*rules) != FilteredNetworkDefaultDrop {
		return false
	}
	for _, rule := range rules.Rules {
		if rule.Selector != NetworkSelectorLoopback {
			return false
		}
	}
	return true
}

// CompileFilteredNetworkRules creates stable gateway IR from effective launch
// intent. Rows are canonicalized before stable polarity-local indices are
// assigned, so authoring order cannot perturb the rendered IR or nft policy.
func CompileFilteredNetworkRules(rules NetworkRules) (FilteredNetworkRuleSet, error) {
	if rules.Baseline != "" || len(rules.Packs) > 0 || len(rules.DenyPacks) > 0 {
		return FilteredNetworkRuleSet{}, fmt.Errorf(
			"filtered network rules require materialized launch intent")
	}
	if err := validateAccessMode("network", rules.Mode); err != nil {
		return FilteredNetworkRuleSet{}, err
	}
	if rules.Mode == AccessModeUnset {
		return FilteredNetworkRuleSet{}, fmt.Errorf(
			"filtered network rules require an explicit network mode")
	}
	if rules.Mode != AccessModeList && len(rules.Allow) > 0 {
		return FilteredNetworkRuleSet{}, fmt.Errorf(
			`network.allow is only valid with mode "list"`)
	}
	allow, err := normalizeNetworkEntries(rules.Allow, "allow")
	if err != nil {
		return FilteredNetworkRuleSet{}, err
	}
	deny, err := normalizeNetworkEntries(rules.Deny, "deny")
	if err != nil {
		return FilteredNetworkRuleSet{}, err
	}
	out := FilteredNetworkRuleSet{
		ProtocolContract: FilteredNetworkProtocolContract,
		DefaultVerdict:   FilteredNetworkDefaultDrop,
	}
	if rules.Mode == AccessModeOpen {
		out.DefaultVerdict = FilteredNetworkDefaultAccept
	}
	out.Rules = compileFilteredNetworkEntries(allow)
	out.DenyRules = compileFilteredNetworkEntries(deny)
	return out, nil
}

func compileFilteredNetworkEntries(
	entries []NetworkAllowEntry,
) []FilteredNetworkRule {
	out := make([]FilteredNetworkRule, 0, len(entries))
	for i, entry := range entries {
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
		}
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool {
		return filteredNetworkRuleKey(out[i]) < filteredNetworkRuleKey(out[j])
	})
	for i := range out {
		out[i].EntryIndex = i
	}
	return out
}

func filteredNetworkRuleKey(rule FilteredNetworkRule) string {
	entry := NetworkAllowEntry{
		IncludeSubdomains: rule.IncludeSubdomains,
		Ports:             rule.Ports,
	}
	switch rule.Selector {
	case NetworkSelectorHost:
		entry.Host = rule.Value
	case NetworkSelectorDomain:
		entry.Domain = rule.Value
	case NetworkSelectorCIDR:
		entry.CIDR = rule.Value
	case NetworkSelectorLoopback:
		entry.Loopback = true
	}
	return networkEntryKey(entry)
}

// FilteredNetworkDefaultVerdictForRules applies the forever-compatibility
// meaning of legacy IR: an omitted default verdict is drop.
func FilteredNetworkDefaultVerdictForRules(
	rules FilteredNetworkRuleSet,
) FilteredNetworkDefaultVerdict {
	if rules.DefaultVerdict == "" {
		return FilteredNetworkDefaultDrop
	}
	return rules.DefaultVerdict
}
