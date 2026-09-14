//go:build linux

package session

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/net/dns/dnsmessage"
	"testing"
	"time"
)

func TestNetworkReloadReevaluatesCachedCNAMEAndRevokesEstablished(t *testing.T) {
	b := &filteredNetworkDNSBroker{}
	name, err := dnsmessage.NewName("allowed.example.")
	require.NoError(t, err)
	record := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: name, Type: dnsmessage.TypeA, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 9}}}
	require.NoError(t, b.observeDNS(record, map[string]struct{}{"allowed.example.": {}, "target.example.": {}}))
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen, Deny: []sandboxpolicy.NetworkAllowEntry{{Host: "target.example"}}})
	require.NoError(t, err)
	policy, err := renderReloadNetworkPolicy(b, rules, time.Now())
	require.NoError(t, err)
	require.NotContains(t, policy, "ct state established accept")
	require.Contains(t, policy, "203.0.113.9 timeout 59s")
	require.Contains(t, policy, "add element inet")
	policy, err = renderReloadNetworkPolicy(b, rules, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NotContains(t, policy, "203.0.113.9")
}

func TestNetworkReloadRetainsEarlierLongDNSAnswer(t *testing.T) {
	b := &filteredNetworkDNSBroker{}
	record := dnsmessage.Resource{Header: dnsmessage.ResourceHeader{TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 9}}}
	names := map[string]struct{}{"target.example.": {}}
	require.NoError(t, b.observeDNS(record, names))
	record.Header.TTL = 1
	require.NoError(t, b.observeDNS(record, names))
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen, Deny: []sandboxpolicy.NetworkAllowEntry{{Host: "target.example"}}})
	require.NoError(t, err)
	policy, err := renderReloadNetworkPolicy(b, rules, time.Now().Add(2*time.Second))
	require.NoError(t, err)
	require.Contains(t, policy, "203.0.113.9 timeout 57s")
}
