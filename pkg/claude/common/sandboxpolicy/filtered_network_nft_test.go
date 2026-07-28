package sandboxpolicy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderFilteredNetworkNFTRendersCIDRPortsAndSyntheticLoopback(t *testing.T) {
	ir, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{CIDR: "192.0.2.0/24", Ports: []int{443, 8443}},
			{CIDR: "2001:db8::/32"},
			{Loopback: true, Ports: []int{3000}},
		},
	})
	require.NoError(t, err)
	got, err := RenderFilteredNetworkNFT(ir)
	require.NoError(t, err)
	assert.Contains(t, got, "type filter hook output priority filter; policy drop;")
	assert.Contains(t, got,
		"ip6 daddr { fe80::/10, ff02::/16 } ip6 hoplimit 255 "+
			"icmpv6 type { nd-router-solicit, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } accept",
	)
	assert.Contains(t, got, "ip daddr 192.0.2.0/24 tcp dport { 443, 8443 } accept")
	assert.Contains(t, got, "ip daddr 192.0.2.0/24 udp dport { 443, 8443 } accept")
	assert.NotContains(t, got, "ip daddr 192.0.2.0/24 icmp")
	assert.Contains(t, got,
		"ip6 daddr 2001:db8::/32 ip6 hoplimit 255 "+
			"icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept",
	)
	assert.Contains(t, got, "ip6 daddr 2001:db8::/32 tcp accept")
	assert.NotContains(t, got, "icmp type echo-request")
	assert.NotContains(t, got, "icmpv6 type echo-request")
	assert.Contains(t, got, "ip daddr "+FilteredNetworkLoopbackIPv4+"/32 tcp dport { 3000 } accept")
	assert.Contains(t, got,
		"ip6 daddr "+FilteredNetworkLoopbackIPv6+"/128 ip6 hoplimit 255 "+
			"icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert } accept",
	)
	assert.Contains(t, got, "ip6 daddr "+FilteredNetworkLoopbackIPv6+"/128 udp dport { 3000 } accept")
}

func TestRenderFilteredNetworkNFTRendersBoundedTTLLeaseSets(t *testing.T) {
	ir, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Host: "api.example.test", Ports: []int{443}},
			{Domain: "example.test", IncludeSubdomains: true, Ports: []int{443, 8443}},
		},
	})
	require.NoError(t, err)
	got, err := RenderFilteredNetworkNFT(ir)
	require.NoError(t, err)
	for _, setName := range []string{"dns4_0", "dns6_0", "dns4_1", "dns6_1"} {
		assert.Contains(t, got, "set "+setName+" {")
		assert.Contains(t, got, "flags timeout")
		assert.Contains(t, got, "size 4096")
	}
	assert.Contains(t, got, "ip daddr @dns4_0 tcp dport { 443 } accept")
	assert.Contains(t, got, "ip6 daddr @dns6_0 udp dport { 443 } accept")
	assert.Contains(t, got, "ip daddr @dns4_1 tcp dport { 443, 8443 } accept")
	assert.Contains(t, got, "ip6 daddr @dns6_1 udp dport { 443, 8443 } accept")
}

func TestRenderFilteredNetworkNFTRejectsDuplicateAuthoredIndexes(t *testing.T) {
	_, err := RenderFilteredNetworkNFT(FilteredNetworkRuleSet{
		ProtocolContract: FilteredNetworkProtocolContract,
		Rules: []FilteredNetworkRule{
			{EntryIndex: 4, Selector: NetworkSelectorHost, Value: "one.example"},
			{EntryIndex: 4, Selector: NetworkSelectorDomain, Value: "two.example"},
		},
	})
	require.ErrorContains(t, err, "repeats authored index 4")
}

func TestFilteredNetworkHostsFileMovesHostRowsBehindBroker(t *testing.T) {
	got, err := FilteredNetworkHostsFile([]byte(
		"\n# runner-managed hosts\n192.0.2.44 api.example.test\n",
	))
	require.NoError(t, err)
	assert.Equal(t,
		"127.0.0.1 localhost\n"+
			"::1 localhost ip6-localhost ip6-loopback\n"+
			FilteredNetworkLoopbackIPv4+" "+FilteredNetworkHostLoopbackName+"\n"+
			FilteredNetworkLoopbackIPv6+" "+FilteredNetworkHostLoopbackName+"\n",
		string(got),
	)
	assert.NotContains(t, string(got), "api.example.test")
}

func TestFilteredNetworkHostsFileRefusesCompetingSyntheticMapping(t *testing.T) {
	_, err := FilteredNetworkHostsFile([]byte(
		"192.0.2.4 alias " + FilteredNetworkHostLoopbackName + " # stale\n",
	))
	require.ErrorContains(t, err, "reserved filtered-network name")
	require.ErrorContains(t, err, "remove that host mapping")
}

func TestFilteredNetworkResolvConfNamesOnlyPrivateBroker(t *testing.T) {
	got := string(FilteredNetworkResolvConf())
	assert.Contains(t, got, "nameserver "+FilteredNetworkDNSIPv4)
	assert.NotContains(t, got, "8.8.8.8")
	assert.NotContains(t, got, FilteredNetworkLoopbackIPv4)
}
