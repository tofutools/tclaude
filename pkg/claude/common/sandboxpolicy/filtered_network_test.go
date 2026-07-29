package sandboxpolicy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFilteredNetworkRuleOrderProducesIdenticalIRAndNFT(t *testing.T) {
	first := NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Domain: "example.test", IncludeSubdomains: true, Ports: []int{443}},
			{CIDR: "192.0.2.0/24"},
			{Host: "api.example.test", Ports: []int{8443, 443}},
		},
		Deny: []NetworkAllowEntry{
			{Host: "blocked.example.test"},
			{CIDR: "192.0.2.44/32", Ports: []int{443}},
		},
	}
	second := NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			first.Allow[2], first.Allow[0], first.Allow[1],
		},
		Deny: []NetworkAllowEntry{first.Deny[1], first.Deny[0]},
	}
	firstIR, err := CompileFilteredNetworkRules(first)
	require.NoError(t, err)
	secondIR, err := CompileFilteredNetworkRules(second)
	require.NoError(t, err)
	firstJSON, err := json.Marshal(firstIR)
	require.NoError(t, err)
	secondJSON, err := json.Marshal(secondIR)
	require.NoError(t, err)
	assert.Equal(t, firstJSON, secondJSON)

	firstNFT, err := RenderFilteredNetworkNFT(firstIR)
	require.NoError(t, err)
	secondNFT, err := RenderFilteredNetworkNFT(secondIR)
	require.NoError(t, err)
	assert.Equal(t, firstNFT, secondNFT)
}

func TestFilteredNetworkLegacyIRDefaultsToExactDropSemantics(t *testing.T) {
	legacyJSON := []byte(`{
		"protocol_contract": "ordinary IPv4/IPv6 TCP and UDP connections (QUIC is UDP; raw and packet sockets are not an authored class)",
		"rules": [{"entry_index": 0, "selector": "cidr", "value": "192.0.2.0/24", "ports": [443]}]
	}`)
	var legacy FilteredNetworkRuleSet
	require.NoError(t, json.Unmarshal(legacyJSON, &legacy))
	assert.Empty(t, legacy.DefaultVerdict)
	assert.Empty(t, legacy.DenyRules)

	current, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{{
			CIDR: "192.0.2.0/24", Ports: []int{443},
		}},
	})
	require.NoError(t, err)
	legacyNFT, err := RenderFilteredNetworkNFT(legacy)
	require.NoError(t, err)
	currentNFT, err := RenderFilteredNetworkNFT(current)
	require.NoError(t, err)
	assert.Equal(t, currentNFT, legacyNFT)
}

func TestCompileFilteredNetworkRulesPreservesNormalizedSelectorIdentity(t *testing.T) {
	got, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Host: "API.Example.COM", Ports: []int{8443, 443, 443}},
			{Domain: "Example.COM", IncludeSubdomains: true},
			{CIDR: "192.0.2.9/24", Ports: []int{53}},
			{Loopback: true, Ports: []int{3000}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, FilteredNetworkProtocolContract, got.ProtocolContract)
	assert.Equal(t, FilteredNetworkDefaultDrop, got.DefaultVerdict)
	require.Len(t, got.Rules, 4)
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 0, Selector: NetworkSelectorCIDR,
		Value: "192.0.2.0/24", Ports: []int{53},
	}, got.Rules[0])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 1, Selector: NetworkSelectorDomain,
		Value: "example.com", IncludeSubdomains: true,
	}, got.Rules[1])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 2, Selector: NetworkSelectorHost,
		Value: "api.example.com", Ports: []int{443, 8443},
	}, got.Rules[2])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 3, Selector: NetworkSelectorLoopback,
		Value: FilteredNetworkHostLoopbackName, Ports: []int{3000},
	}, got.Rules[3])
}

func TestCompileFilteredNetworkRulesAcceptsOpenDenyAndRejectsAmbiguousEntries(t *testing.T) {
	open, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeOpen,
		Deny: []NetworkAllowEntry{{CIDR: "192.0.2.0/24"}},
	})
	require.NoError(t, err)
	assert.Equal(t, FilteredNetworkDefaultAccept, open.DefaultVerdict)
	require.Len(t, open.DenyRules, 1)

	_, err = CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{{
			Host: "api.example.com", CIDR: "192.0.2.0/24",
		}},
	})
	require.ErrorContains(t, err, "exactly one")
}

func TestLoopbackOnlyClassifiersRequireNonEmptyAllLoopbackRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules NetworkRules
		want  bool
	}{
		{"preset", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Loopback: true}}}, true},
		{"port scoped", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{{Loopback: true, Ports: []int{11434}}}}, true},
		{"multiple port rows", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
			{Loopback: true, Ports: []int{3000}},
			{Loopback: true, Ports: []int{11434}},
		}}, true},
		{"empty list", NetworkRules{Mode: AccessModeList}, false},
		{"closed", NetworkRules{Mode: AccessModeClosed}, false},
		{"mixed", NetworkRules{Mode: AccessModeList, Allow: []NetworkAllowEntry{
			{Loopback: true}, {Domain: "api.example.com"},
		}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NetworkRulesAreLoopbackOnly(tc.rules))
			if tc.rules.Mode != AccessModeList {
				return
			}
			ir, err := CompileFilteredNetworkRules(tc.rules)
			require.NoError(t, err)
			assert.Equal(t, tc.want, FilteredNetworkRulesAreLoopbackOnly(&ir))
		})
	}
	assert.False(t, FilteredNetworkRulesAreLoopbackOnly(nil))
	assert.False(t, FilteredNetworkRulesAreLoopbackOnly(&FilteredNetworkRuleSet{
		ProtocolContract: "future",
		Rules:            []FilteredNetworkRule{{Selector: NetworkSelectorLoopback}},
	}))
}

func TestFilteredNetworkProtocolContractStaysBounded(t *testing.T) {
	assert.Contains(t, FilteredNetworkProtocolContract, "IPv4/IPv6 TCP and UDP")
	assert.Contains(t, FilteredNetworkProtocolContract, "QUIC is UDP")
	assert.Contains(t, FilteredNetworkProtocolContract, "raw and packet sockets are not")
	assert.NotContains(t, FilteredNetworkProtocolContract, "ICMP")
}
