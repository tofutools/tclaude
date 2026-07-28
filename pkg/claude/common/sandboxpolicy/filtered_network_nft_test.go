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
	assert.Contains(t, got, "ip daddr 192.0.2.0/24 tcp dport { 443, 8443 } accept")
	assert.Contains(t, got, "ip daddr 192.0.2.0/24 udp dport { 443, 8443 } accept")
	assert.Contains(t, got, "ip6 daddr 2001:db8::/32 tcp accept")
	assert.NotContains(t, got, "icmp type echo-request")
	assert.NotContains(t, got, "icmpv6 type echo-request")
	assert.Contains(t, got, "ip daddr "+FilteredNetworkLoopbackIPv4+"/32 tcp dport { 3000 } accept")
	assert.Contains(t, got, "ip6 daddr "+FilteredNetworkLoopbackIPv6+"/128 udp dport { 3000 } accept")
}

func TestRenderFilteredNetworkNFTRefusesDNSSelectors(t *testing.T) {
	for _, entry := range []NetworkAllowEntry{
		{Host: "api.example.test", Ports: []int{443}},
		{Domain: "example.test", IncludeSubdomains: true, Ports: []int{443}},
	} {
		ir, err := CompileFilteredNetworkRules(NetworkRules{
			Mode: AccessModeList, Allow: []NetworkAllowEntry{entry},
		})
		require.NoError(t, err)
		_, err = RenderFilteredNetworkNFT(ir)
		require.ErrorContains(t, err, "M2c DNS broker")
	}
}

func TestFilteredNetworkHostsFilePreservesHostRowsAndAddsSyntheticMapping(t *testing.T) {
	got, err := FilteredNetworkHostsFile([]byte("127.0.0.1 localhost"))
	require.NoError(t, err)
	assert.Equal(t,
		"127.0.0.1 localhost\n"+
			FilteredNetworkLoopbackIPv4+" "+FilteredNetworkHostLoopbackName+"\n"+
			FilteredNetworkLoopbackIPv6+" "+FilteredNetworkHostLoopbackName+"\n",
		string(got),
	)
}
