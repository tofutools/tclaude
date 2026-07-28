package harness

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestModelTransportRequirementsAreHarnessOwnedAndExplicit(t *testing.T) {
	for _, tc := range []struct {
		harnessName  string
		template     string
		destinations []string
	}{
		{DefaultName, "net-anthropic", []string{"api.anthropic.com:443"}},
		{CodexName, "net-openai-codex", []string{
			"api.openai.com:443", "chatgpt.com:443", "auth.openai.com:443",
		}},
	} {
		requirement, err := ResolveModelTransportRequirement(MustGet(tc.harnessName), "")
		require.NoError(t, err)
		assert.Equal(t, tc.template, requirement.Template)
		detail := DescribeModelTransportRequirement(requirement)
		assert.Contains(t, detail, "profile allow list only")
		assert.Contains(t, detail, "no hidden model-traffic bypass")
		for _, destination := range tc.destinations {
			assert.Contains(t, detail, destination)
		}
	}
}

func TestModelTransportCoverageRefusesWithoutMutatingPolicy(t *testing.T) {
	h := MustGet(CodexName)
	requirement, err := ResolveModelTransportRequirement(h, "gpt-5.6-sol")
	require.NoError(t, err)
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Domain: "api.openai.com", Ports: []int{443},
		}},
	}
	before := rules
	err = ValidateModelTransportCoverage(h, rules, requirement)
	require.Error(t, err)
	var capability *SandboxCapabilityError
	require.True(t, errors.As(err, &capability))
	assert.Equal(t, SandboxCapabilityModelTransport, capability.Kind)
	assert.Contains(t, err.Error(), "no hidden model-traffic bypass")
	assert.Contains(t, err.Error(), "include template net-openai-codex")
	assert.Contains(t, err.Error(), "choose a resolvable provider")
	assert.Contains(t, err.Error(), "network open")
	assert.Equal(t, before, rules, "preflight must not auto-union model destinations")

	rules.Allow = append(rules.Allow,
		sandboxpolicy.NetworkAllowEntry{Domain: "chatgpt.com", Ports: []int{443}},
		sandboxpolicy.NetworkAllowEntry{Domain: "auth.openai.com", Ports: []int{443}},
	)
	require.NoError(t, ValidateModelTransportCoverage(h, rules, requirement))
}

func TestModelTransportCoverageHonorsExplicitSubdomainAndPortBounds(t *testing.T) {
	h := MustGet(DefaultName)
	requirement, err := ResolveModelTransportRequirement(h, "sonnet")
	require.NoError(t, err)
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Domain: "anthropic.com", IncludeSubdomains: true, Ports: []int{80},
		}},
	}
	require.Error(t, ValidateModelTransportCoverage(h, rules, requirement))
	rules.Allow[0].Ports = []int{443}
	require.NoError(t, ValidateModelTransportCoverage(h, rules, requirement))
}

func TestOpenCodeModelTransportRequiresResolvedProviderEndpoint(t *testing.T) {
	_, err := ResolveModelTransportRequirement(MustGet(OpenCodeName), "custom/model")
	require.Error(t, err)
	var capability *SandboxCapabilityError
	require.True(t, errors.As(err, &capability))
	assert.Equal(t, SandboxCapabilityModelTransport, capability.Kind)
	assert.Contains(t, err.Error(), "resolved provider endpoint")
}
