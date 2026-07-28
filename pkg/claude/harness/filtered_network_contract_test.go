package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestAssessFilteredNetworkRulesRatesActualEntries(t *testing.T) {
	ir, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{CIDR: "192.0.2.0/24", Ports: []int{443}},
			{Host: "api.example.com", Ports: []int{443}},
			{Loopback: true, Ports: []int{3000}},
		},
	})
	require.NoError(t, err)
	assessments, aggregate, err := AssessFilteredNetworkRules(ir)
	require.NoError(t, err)
	require.Len(t, assessments, 3)
	assert.Equal(t, EnforceNone, aggregate)
	assert.Equal(t, EnforceFull, assessments[0].DestinationLevel)
	assert.Equal(t, EnforceNone, assessments[1].DestinationLevel)
	assert.Equal(t, FilteredNetworkDNSIdentityCaveat, assessments[1].DestinationDetail)
	assert.Equal(t, EnforcePartial, assessments[2].DestinationLevel)
	assert.Equal(t, FilteredNetworkLoopbackCaveat, assessments[2].DestinationDetail)
	for _, assessment := range assessments {
		assert.Equal(t, EnforceFull, assessment.PortLevel)
		assert.Contains(t, assessment.PortDetail, "TCP and UDP")
		assert.Contains(t, assessment.PortDetail, "QUIC")
	}
}

func TestAssessFilteredNetworkRulesCIDROnlyTargetMatchesM2bCapabilityCells(t *testing.T) {
	ir, err := sandboxpolicy.CompileFilteredNetworkRules(sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "2001:db8::/32", Ports: []int{443},
		}},
	})
	require.NoError(t, err)
	_, aggregate, err := AssessFilteredNetworkRules(ir)
	require.NoError(t, err)
	assert.Equal(t, EnforceFull, aggregate)

	for _, harnessName := range []string{DefaultName, CodexName, OpenCodeName} {
		h := MustGet(harnessName)
		caps, tableErr := accessEnforcementTable(
			h, sandboxpolicy.ImplementationTclaudeLayer,
			sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList}},
			"", "linux", true,
		)
		require.NoError(t, tableErr)
		want := EnforceFull
		if harnessName == OpenCodeName {
			want = EnforceNone
		}
		assert.Equal(t, want, caps.NetworkList,
			"M2b flips only Claude/Codex tclaude-layer; OpenCode waits for M3")
	}
}

func TestAssessFilteredNetworkRulesRejectsUnknownContractAndSelector(t *testing.T) {
	_, _, err := AssessFilteredNetworkRules(sandboxpolicy.FilteredNetworkRuleSet{})
	require.ErrorContains(t, err, "unknown protocol contract")

	_, _, err = AssessFilteredNetworkRules(sandboxpolicy.FilteredNetworkRuleSet{
		ProtocolContract: sandboxpolicy.FilteredNetworkProtocolContract,
		Rules: []sandboxpolicy.FilteredNetworkRule{{
			EntryIndex: 7, Selector: "future",
		}},
	})
	require.ErrorContains(t, err, `rule 7 has unknown selector "future"`)
}
