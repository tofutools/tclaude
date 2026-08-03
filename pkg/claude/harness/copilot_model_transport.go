package harness

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// Copilot's model transport under a filtered network (TCL-978).
//
// Filtered networking admits a destination only if the launch endpoint can be
// resolved BEFORE the process starts, so this file's whole job is to decide
// which Copilot launches have a resolvable route and to refuse the rest. It
// supports exactly one route: the first-party GitHub Copilot service on its
// default hosts. Everything else — a moved endpoint, an enterprise host, a
// BYOK provider — is a refusal, because each of them either points the traffic
// somewhere the authored allow list was never checked against, or (for BYOK)
// is a user-controlled endpoint that is not an approved first-party transport.
//
// WHERE THE HOSTS COME FROM. Not from documentation, which names none of them.
// They are read out of the pinned 1.0.77 CLI's own shipped runtime
// (prebuilds/<platform>/runtime.node, the native module every request goes
// through):
//
//   - https://api.githubcopilot.com is the built-in CAPI endpoint, and the
//     runtime carries the matching "CAPI URL is not configured" failure path,
//     so it is the default rather than one of several equals.
//   - api.github.com carries the control plane the CLI needs before and during
//     a session: the /copilot_internal/* routes (token exchange, content
//     exclusion, managed settings, subscription state) are all on it.
//
// WHAT MOVES THE ROUTE, and is therefore refused rather than followed:
// COPILOT_API_URL (real but undocumented — it appears in the runtime's own
// environment list, not in `copilot help environment`), the `copilotUrl`
// settings key, GH_HOST / COPILOT_GH_HOST (which select an Enterprise or
// data-residency host, changing BOTH the control plane and the CAPI host), and
// the COPILOT_PROVIDER_* BYOK family. Proxy variables are refused one level up,
// by the shared launch resolver, because a proxy hides the real destination
// from every harness equally.
//
// The honest limit: this names the destinations a launch NEEDS for model and
// control-plane traffic. It does not claim to enumerate every host a Copilot
// session may ever touch — telemetry, MCP servers, web tools and `gh` are
// separate features with their own destinations, and an operator who enables
// them authors those separately. That is the same contract Codex's and
// Claude's resolvers make, and the filtered wall keeps it honest by denying
// what nobody authored.

// CopilotFirstPartyNetworkPack is the release-owned pack covering the
// first-party Copilot route. The resolver names it as the requirement's
// Template so an operator gets "include template net-github-copilot" as the
// remedy instead of a list of hosts to retype.
const CopilotFirstPartyNetworkPack = "net-github-copilot"

// Copilot's default first-party hosts.
const (
	// CopilotDefaultCAPIHost serves model traffic.
	CopilotDefaultCAPIHost = "api.githubcopilot.com"
	// CopilotControlPlaneHost serves the /copilot_internal/* control plane a
	// session needs to obtain and refresh its Copilot token.
	CopilotControlPlaneHost = "api.github.com"
)

// copilotRouteMovingEnvVars are launch environment variables that change which
// endpoint the CLI talks to. Their VALUES are never inspected: a resolver that
// followed COPILOT_API_URL would be resolving a user-controlled endpoint, which
// is the same thing BYOK is refused for.
var copilotRouteMovingEnvVars = []string{
	// Undocumented but present in the runtime's own environment list; it
	// overrides the CAPI endpoint outright.
	"COPILOT_API_URL",
	// Select an Enterprise Server / data-residency host, which moves the
	// control plane and the CAPI host together.
	"COPILOT_GH_HOST",
	"GH_HOST",
	// The BYOK family. COPILOT_PROVIDER_BASE_URL alone activates it, but the
	// others are listed because a launch carrying them has BYOK intent and
	// should be told so by name rather than launched on the first-party route.
	"COPILOT_PROVIDER_BASE_URL",
	"COPILOT_PROVIDER_TYPE",
	"COPILOT_PROVIDER_WIRE_API",
	"COPILOT_PROVIDER_TRANSPORT",
	"COPILOT_PROVIDER_API_KEY",
	"COPILOT_PROVIDER_BEARER_TOKEN",
	"COPILOT_PROVIDER_GHES_TOKEN",
	"COPILOT_PROVIDER_HEADERS",
	"COPILOT_PROVIDER_MODEL_ID",
	"COPILOT_PROVIDER_WIRE_MODEL",
	"COPILOT_PROVIDER_AZURE_API_VERSION",
}

// CopilotRouteMovingEnvVars returns the refused variables, for the launch
// resolver that inspects the composed launch environment and for the test that
// keeps the two in step. A fresh slice each call so a caller cannot mutate the
// contract.
func CopilotRouteMovingEnvVars() []string {
	return append([]string(nil), copilotRouteMovingEnvVars...)
}

// copilotModelTransport is Copilot's ModelTransportResolver.
//
// It deliberately does not embed staticModelTransport (the shape Claude and
// Codex share). That helper treats a resolved BaseURL as a legitimate
// substitute endpoint and returns it as the requirement; for Copilot a
// non-default endpoint is exactly what must be REFUSED, so reusing it would
// invert the contract.
type copilotModelTransport struct{}

func (copilotModelTransport) ResolveModelTransport(
	resolved ResolvedModelTransport,
) (ModelTransportRequirement, error) {
	if !resolved.ProviderResolved {
		return ModelTransportRequirement{}, fmt.Errorf(
			"copilot provider configuration was not resolved")
	}
	if endpoint := strings.TrimSpace(resolved.BaseURL); endpoint != "" {
		return ModelTransportRequirement{}, fmt.Errorf(
			"this Copilot launch resolves to custom provider endpoint %q; a user-controlled BYOK "+
				"endpoint is not an approved first-party transport for filtered networking. "+
				"Remove the COPILOT_PROVIDER_* configuration to use the first-party GitHub "+
				"Copilot route, or use network open", endpoint)
	}
	provider := strings.ToLower(strings.TrimSpace(resolved.Provider))
	if provider != "" && provider != CopilotName {
		return ModelTransportRequirement{}, fmt.Errorf(
			"this Copilot launch resolves to provider %q, which has no reviewed filtered-network "+
				"endpoint; use the first-party GitHub Copilot route or network open",
			resolved.Provider)
	}
	return ModelTransportRequirement{
		Template:     CopilotFirstPartyNetworkPack,
		Destinations: CopilotFirstPartyDestinations(),
		ResolvedBy:   "GitHub Copilot first-party route (harness default endpoints)",
	}, nil
}

// CopilotFirstPartyDestinations is the destination set of the supported route.
// Exported so the network pack and the resolver are built from one list rather
// than two that could drift — a drift whose failure mode is a pack an operator
// selects that does not actually cover the launch.
func CopilotFirstPartyDestinations() []sandboxpolicy.NetworkAllowEntry {
	return []sandboxpolicy.NetworkAllowEntry{
		{Domain: CopilotDefaultCAPIHost, Ports: []int{443}},
		{Domain: CopilotControlPlaneHost, Ports: []int{443}},
	}
}
