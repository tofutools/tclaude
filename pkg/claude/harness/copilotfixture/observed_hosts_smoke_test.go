package copilotfixture_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TestCopilotStartupDialsOnlyContractedHosts converts the filtered-network
// host contract from DERIVED evidence into OBSERVED evidence.
//
// harness/copilot_model_transport.go names its destinations from strings in
// the pinned CLI's shipped runtime module. That proves the hosts are in the
// binary; it does not prove a launch dials them, and it cannot prove the list
// is complete. This runs a credential-free startup through a proxy that logs
// and refuses every tunnel, so the hosts the CLI actually wanted become
// observable without a token and without any traffic leaving the machine.
//
// The assertion is deliberately ONE-SIDED, and the direction matters:
//
//   - An observed host the contract does not name FAILS the test. That is the
//     regression that would hurt an operator, because the filtered wall denies
//     exactly what nobody authored, so an unnamed destination is a launch that
//     silently loses a connection it needed.
//   - A contracted host that this particular startup did not dial does NOT
//     fail it. An unauthenticated startup stops early — it has no Copilot
//     token to exchange — so which of the two contracted hosts it reaches
//     depends on how far it gets, and requiring both would make the test a
//     record of the CLI's failure path rather than of its destinations.
//
// THE LIMIT, stated because it is a real acceptance gap rather than an
// oversight: this observes PRE-authentication traffic only. A launch that gets
// past the token exchange may reach further hosts, and no credential-free run
// can see them.
//
// That gap has since been closed from the other side, and it mattered: a
// credentialed run showed the model host is assigned BY the token exchange, so
// the account under test was routed to a host the pack did not name. See
// postauth_destinations_test.go for the recorded evidence and the offline
// contract check, and postauth_capture_smoke_test.go for the local capture that
// produced it. This scenario stays as it is — credential-free, refusing every
// tunnel — because it is the one that can run anywhere.
func TestCopilotStartupDialsOnlyContractedHosts(t *testing.T) {
	requireLabParallel(t)

	capture := copilotfixture.NewProxyCapture(t)
	dirs := copilotfixture.NewSandboxDirs(t)

	// No BaseURL: this is the one scenario that must NOT activate BYOK, since
	// the first-party route is its entire subject. A non-zero exit is expected
	// and is not a failure — an unauthenticated CLI whose every connection is
	// refused cannot complete a turn, and the evidence is what it dialed.
	_ = copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir:       dirs.WorkDir,
		ProxyEndpoint: capture.Endpoint(),
		Prompt:        "Observed-host question.",
	})

	observed := capture.Hosts()
	t.Logf("credential-free startup dialed: %v", observed)

	contracted := map[string]bool{}
	for _, destination := range harness.CopilotFirstPartyDestinations() {
		contracted[strings.ToLower(destination.Domain)] = true
	}

	var uncontracted []string
	for _, host := range observed {
		// Loopback is never a first-party destination and never traverses the
		// wall the contract describes; the CLI's own local servers (ACP, voice,
		// the webview) live there.
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			continue
		}
		if !contracted[host] {
			uncontracted = append(uncontracted, host)
		}
	}
	assert.Empty(t, uncontracted,
		"a credential-free Copilot startup dialed %v, which the filtered-network contract "+
			"(harness.CopilotFirstPartyDestinations) does not name. Under a filtered policy "+
			"those connections are DENIED, so either the contract needs the destination or "+
			"the launch needs to stop making it — do not widen the pack without deciding which",
		uncontracted)

	// The contract must not have become vacuous: if a startup dials nothing at
	// all, this scenario is silently proving something about a CLI that never
	// ran, and the derived host list would be unguarded again.
	require.NotEmpty(t, observed,
		"the credential-free startup dialed nothing through the capture proxy, so this "+
			"scenario observed no destinations at all; the proxy variables or the CLI's "+
			"proxy honouring must have changed")

	// And at least one host it did dial must be a contracted one — otherwise
	// the CLI is reaching its service through names the contract never
	// mentions, and the emptiness check above would pass on pure loopback.
	dialedContracted := slices.ContainsFunc(observed, func(host string) bool {
		return contracted[host]
	})
	assert.True(t, dialedContracted,
		"none of the contracted first-party hosts %v appeared in the observed set %v; "+
			"the derived destination list no longer describes what a startup dials",
		harness.CopilotFirstPartyDestinations(), observed)
}
