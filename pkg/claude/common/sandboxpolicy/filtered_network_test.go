package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	require.Len(t, got.Rules, 4)
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 0, Selector: NetworkSelectorHost,
		Value: "api.example.com", Ports: []int{443, 8443},
	}, got.Rules[0])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 1, Selector: NetworkSelectorDomain,
		Value: "example.com", IncludeSubdomains: true,
	}, got.Rules[1])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 2, Selector: NetworkSelectorCIDR,
		Value: "192.0.2.0/24", Ports: []int{53},
	}, got.Rules[2])
	assert.Equal(t, FilteredNetworkRule{
		EntryIndex: 3, Selector: NetworkSelectorLoopback,
		Value: FilteredNetworkHostLoopbackName, Ports: []int{3000},
	}, got.Rules[3])
}

func TestCompileFilteredNetworkRulesRejectsNonListAndAmbiguousEntries(t *testing.T) {
	_, err := CompileFilteredNetworkRules(NetworkRules{Mode: AccessModeOpen})
	require.ErrorContains(t, err, `require network.mode "list"`)

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
