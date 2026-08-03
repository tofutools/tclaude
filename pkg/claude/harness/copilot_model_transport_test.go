package harness

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// TestCopilotModelTransportResolvesFirstPartyRoute pins the one supported
// route and, through the network pack, the remedy an operator is given.
func TestCopilotModelTransportResolvesFirstPartyRoute(t *testing.T) {
	h := MustGet(CopilotName)
	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Model: "claude-sonnet-5", Provider: CopilotName, ProviderResolved: true,
	})
	if err != nil {
		t.Fatalf("ResolveModelTransportRequirement = %v", err)
	}
	if requirement.Template != CopilotFirstPartyNetworkPack {
		t.Fatalf("Template = %q, want %q", requirement.Template, CopilotFirstPartyNetworkPack)
	}
	wantHosts := []string{CopilotDefaultCAPIHost, CopilotControlPlaneHost}
	var gotHosts []string
	for _, destination := range requirement.Destinations {
		gotHosts = append(gotHosts, destination.Domain)
		if !slices.Equal(destination.Ports, []int{443}) {
			t.Errorf("destination %q ports = %v, want [443]", destination.Domain, destination.Ports)
		}
	}
	slices.Sort(gotHosts)
	slices.Sort(wantHosts)
	if !slices.Equal(gotHosts, wantHosts) {
		t.Fatalf("destinations = %v, want %v", gotHosts, wantHosts)
	}

	// The enterprise CAPI host appears in the shipped runtime, but its
	// selection is not inspectable at this seam, so it must NOT be in the
	// default route. A launch that wants it is refused (see the GH_HOST case in
	// the session resolver) rather than silently granted a second host.
	for _, destination := range requirement.Destinations {
		if strings.Contains(destination.Domain, "enterprise") {
			t.Fatalf("the default route must not carry the enterprise host %q", destination.Domain)
		}
	}
}

// TestCopilotFirstPartyNetworkPackMatchesResolver keeps the operator-selectable
// pack and the resolver's requirement in step. They live in different packages
// (the policy package cannot import the harness one), so nothing but this test
// stops a pack from drifting into "selectable but does not cover the launch".
func TestCopilotFirstPartyNetworkPackMatchesResolver(t *testing.T) {
	entries, err := sandboxpolicy.ExpandNetworkPackEntries(CopilotFirstPartyNetworkPack)
	if err != nil {
		t.Fatalf("ExpandNetworkPackEntries(%q) = %v", CopilotFirstPartyNetworkPack, err)
	}
	want := CopilotFirstPartyDestinations()
	if len(entries) != len(want) {
		t.Fatalf("pack has %d entries, resolver requires %d", len(entries), len(want))
	}
	for _, required := range want {
		found := false
		for _, entry := range entries {
			if strings.EqualFold(entry.Domain, required.Domain) &&
				slices.Equal(entry.Ports, required.Ports) {
				found = true
			}
		}
		if !found {
			t.Errorf("pack %q does not cover required destination %q:%v",
				CopilotFirstPartyNetworkPack, required.Domain, required.Ports)
		}
	}

	// The pack must actually satisfy the coverage check the launch runs, not
	// merely contain equal-looking rows.
	h := MustGet(CopilotName)
	requirement, err := ResolveModelTransportRequirement(h, ResolvedModelTransport{
		Provider: CopilotName, ProviderResolved: true,
	})
	if err != nil {
		t.Fatalf("ResolveModelTransportRequirement = %v", err)
	}
	if err := ValidateModelTransportCoverage(h, sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList, Allow: entries,
	}, requirement); err != nil {
		t.Fatalf("the pack does not cover the resolved Copilot route: %v", err)
	}
}

// TestCopilotModelTransportRefusesUnapprovedRoutes is the fail-closed half.
func TestCopilotModelTransportRefusesUnapprovedRoutes(t *testing.T) {
	h := MustGet(CopilotName)
	for _, tc := range []struct {
		name     string
		resolved ResolvedModelTransport
		wantWord string
	}{
		{
			name:     "an unresolved provider is not proof of the harness default",
			resolved: ResolvedModelTransport{Model: "gpt-5.4"},
			wantWord: "not resolved",
		},
		{
			name: "a BYOK endpoint is user-controlled, not a first-party transport",
			resolved: ResolvedModelTransport{
				Provider: CopilotName, ProviderResolved: true,
				BaseURL: "https://byok.example.com/v1",
			},
			wantWord: "BYOK",
		},
		{
			name: "a loopback BYOK endpoint is refused on the same terms",
			resolved: ResolvedModelTransport{
				Provider: CopilotName, ProviderResolved: true,
				BaseURL: "http://127.0.0.1:4141/v1",
			},
			wantWord: "BYOK",
		},
		{
			name: "another provider has no reviewed filtered endpoint",
			resolved: ResolvedModelTransport{
				Provider: "azure", ProviderResolved: true,
			},
			wantWord: "no reviewed filtered-network",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveModelTransportRequirement(h, tc.resolved)
			var capErr *SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a *SandboxCapabilityError", err)
			}
			if capErr.Kind != SandboxCapabilityModelTransport {
				t.Errorf("Kind = %q, want %q", capErr.Kind, SandboxCapabilityModelTransport)
			}
			if !strings.Contains(capErr.Message, tc.wantWord) {
				t.Errorf("message %q does not explain the refusal (%q)", capErr.Message, tc.wantWord)
			}
		})
	}
}

// TestCopilotRouteMovingEnvVarsAreImmutable guards the refusal list itself: it
// is a contract the session resolver iterates, and a caller that could mutate
// the backing array would silently shrink the set of refused launches.
func TestCopilotRouteMovingEnvVarsAreImmutable(t *testing.T) {
	first := CopilotRouteMovingEnvVars()
	if len(first) == 0 {
		t.Fatal("the route-moving variable list must not be empty")
	}
	for _, required := range []string{
		"COPILOT_API_URL", "GH_HOST", "COPILOT_GH_HOST", "COPILOT_PROVIDER_BASE_URL",
	} {
		if !slices.Contains(first, required) {
			t.Errorf("route-moving variables omit %s", required)
		}
	}
	first[0] = "MUTATED"
	if slices.Contains(CopilotRouteMovingEnvVars(), "MUTATED") {
		t.Fatal("CopilotRouteMovingEnvVars must return a fresh slice")
	}
}
