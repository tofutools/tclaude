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
		{CodexName, "net-openai-codex", []string{"api.openai.com:443"}},
	} {
		provider := "anthropic"
		if tc.harnessName == CodexName {
			provider = "openai"
		}
		requirement, err := ResolveModelTransportRequirement(
			MustGet(tc.harnessName),
			ResolvedModelTransport{
				Provider: provider, ProviderResolved: true,
			},
		)
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
	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model: "gpt-5.6-sol", Provider: "openai", ProviderResolved: true,
	})
	require.NoError(t, err)
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Domain: "api.openai.com", Ports: []int{80},
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

	rules.Allow[0].Ports = []int{443}
	require.NoError(t, ValidateModelTransportCoverage(h, rules, requirement))
}

func TestModelTransportCoverageHonorsExplicitSubdomainAndPortBounds(t *testing.T) {
	h := MustGet(DefaultName)
	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model: "sonnet", Provider: "anthropic", ProviderResolved: true,
	})
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

func TestModelTransportCoverageAcceptsResolvedIPInsideAuthoredCIDR(t *testing.T) {
	h := MustGet(DefaultName)
	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model:            "smoke",
		Provider:         "custom",
		BaseURL:          "http://198.18.0.10:41001/v1",
		ProviderResolved: true,
	})
	require.NoError(t, err)
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "198.18.0.0/25", Ports: []int{41001},
		}},
	}
	require.NoError(t, ValidateModelTransportCoverage(h, rules, requirement))
	rules.Allow[0].CIDR = "198.18.0.128/25"
	require.Error(t, ValidateModelTransportCoverage(h, rules, requirement))
}

func TestOpenCodeModelTransportRequiresResolvedProviderEndpoint(t *testing.T) {
	_, err := ResolveModelTransportRequirement(
		MustGet(OpenCodeName),
		ResolvedModelTransport{
			Model: "custom/model", Provider: "custom", ProviderResolved: true,
		},
	)
	require.Error(t, err)
	var capability *SandboxCapabilityError
	require.True(t, errors.As(err, &capability))
	assert.Equal(t, SandboxCapabilityModelTransport, capability.Kind)
	assert.Contains(t, err.Error(), "resolved provider endpoint")
}

func TestModelTransportRequiresProviderResolutionAndUsesConcreteCustomEndpoint(t *testing.T) {
	h := MustGet(DefaultName)
	_, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model: "sonnet",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "provider configuration was not resolved")
	assert.Contains(t, err.Error(), "choose a resolvable provider")

	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model:            "sonnet",
		Provider:         "anthropic",
		BaseURL:          "https://gateway.example/v1",
		ProviderResolved: true,
	})
	require.NoError(t, err)
	assert.Empty(t, requirement.Template)
	assert.Equal(t, "resolved anthropic provider endpoint", requirement.ResolvedBy)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
		Domain: "gateway.example", Ports: []int{443},
	}}, requirement.Destinations)

	requirement, err = ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model:            "sonnet",
		Provider:         "anthropic",
		BaseURL:          "https://api.anthropic.com:8443/v1",
		ProviderResolved: true,
	})
	require.NoError(t, err)
	assert.Empty(t, requirement.Template,
		"a non-default port on the first-party host is still a custom endpoint")
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
		Domain: "api.anthropic.com", Ports: []int{8443},
	}}, requirement.Destinations)

	openCode, err := ResolveModelTransportRequirement(
		MustGet(OpenCodeName),
		ResolvedModelTransport{
			Model:            "custom/model",
			Provider:         "custom",
			BaseURL:          "http://host.tclaude.internal:11434/v1",
			ProviderResolved: true,
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
		Loopback: true, Ports: []int{11434},
	}}, openCode.Destinations)

	for _, baseURL := range []string{
		"http://127.0.0.1:11434/v1",
		"http://[::1]:11434/v1",
		"http://localhost:11434/v1",
	} {
		_, loopbackErr := ResolveModelTransportRequirement(
			MustGet(OpenCodeName),
			ResolvedModelTransport{
				Model:            "custom/model",
				Provider:         "custom",
				BaseURL:          baseURL,
				ProviderResolved: true,
			},
		)
		require.Error(t, loopbackErr)
		assert.Contains(t, loopbackErr.Error(), "sandbox-private localhost")
		assert.Contains(t, loopbackErr.Error(), sandboxpolicy.FilteredNetworkHostLoopbackName)
	}
}
