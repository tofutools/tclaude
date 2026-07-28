package sandboxpolicy

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatchFilteredNetworkDNSNameIsExactAndLabelBound(t *testing.T) {
	rules, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Host: "api.example.com", Ports: []int{443}},
			{Domain: "example.net", Ports: []int{443}},
			{Domain: "example.org", IncludeSubdomains: true},
			{CIDR: "192.0.2.0/24"},
		},
	})
	require.NoError(t, err)

	tests := []struct {
		name string
		want []int
	}{
		{"api.example.com.", []int{0}},
		{"API.EXAMPLE.COM", []int{0}},
		{"other.example.com", nil},
		{"example.net", []int{1}},
		{"child.example.net", nil},
		{"example.org", []int{2}},
		{"child.example.org", []int{2}},
		{"deep.child.example.org.", []int{2}},
		{"badexample.org", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			matches, matchErr := MatchFilteredNetworkDNSName(rules, test.name)
			require.NoError(t, matchErr)
			var got []int
			for _, match := range matches {
				got = append(got, match.EntryIndex)
			}
			assert.Equal(t, test.want, got)
		})
	}
}

func TestFilteredNetworkCIDRCoversAddress(t *testing.T) {
	rules, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{CIDR: "192.0.2.0/24", Ports: []int{443}},
			{CIDR: "2001:db8::/32"},
		},
	})
	require.NoError(t, err)
	assert.True(t, FilteredNetworkCIDRCoversAddress(
		rules, netip.MustParseAddr("192.0.2.41")))
	assert.False(t, FilteredNetworkCIDRCoversAddress(
		rules, netip.MustParseAddr("192.0.3.41")))
	assert.True(t, FilteredNetworkCIDRCoversAddress(
		rules, netip.MustParseAddr("2001:db8::41")))
}

func TestMatchFilteredNetworkDNSNameRejectsMalformedQueries(t *testing.T) {
	rules, err := CompileFilteredNetworkRules(NetworkRules{
		Mode:  AccessModeList,
		Allow: []NetworkAllowEntry{{Domain: "example.com"}},
	})
	require.NoError(t, err)

	for _, qname := range []string{"", ".", "-bad.example.com", "bad..example.com"} {
		_, matchErr := MatchFilteredNetworkDNSName(rules, qname)
		require.Error(t, matchErr, qname)
	}
}

func TestFilteredNetworkAllowsDNSLoopbackAnswerRequiresAuthoredSelector(t *testing.T) {
	without, err := CompileFilteredNetworkRules(NetworkRules{
		Mode:  AccessModeList,
		Allow: []NetworkAllowEntry{{Domain: "example.com"}},
	})
	require.NoError(t, err)
	assert.False(t, FilteredNetworkAllowsDNSLoopbackAnswer(without))

	with, err := CompileFilteredNetworkRules(NetworkRules{
		Mode: AccessModeList,
		Allow: []NetworkAllowEntry{
			{Domain: "example.com"},
			{Loopback: true, Ports: []int{443}},
		},
	})
	require.NoError(t, err)
	assert.True(t, FilteredNetworkAllowsDNSLoopbackAnswer(with))
}
