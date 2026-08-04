package copilotfixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The authenticated destination contract (TCL-984), checked OFFLINE.
//
// TestCopilotStartupDialsOnlyContractedHosts observes a credential-free
// startup, and says so in its own doc comment: it cannot see what a session
// reaches AFTER the token exchange. That gap mattered more than it looked.
// A credentialed 1.0.77 run through a pass-through CONNECT-observing proxy
// showed the model host is handed to the session BY the token exchange rather
// than compiled in, and that an individual-plan account is routed to
// api.individual.githubcopilot.com — a host the released pack did not name and
// the filtered wall therefore denied.
//
// The run that produced that is local, credentialed and one-off. This test is
// none of those: it reads the recorded host/port/phase metadata from testdata
// and checks the released pack against it, so the contract is enforced on every
// `go test` with no credential, no network and no fixture derived from live
// credential material. Nothing in the fixture came off the wire — it is a
// transcription of destination metadata, and the capture that produced it
// could not have recorded anything else (see ProxyCapture: tunnels are copied
// blind, and a non-CONNECT proxy request is refused rather than parsed).
//
// The two assertions pull in opposite directions on purpose:
//
//   - Every REQUIRED observed destination must be in the pack. A missing one is
//     an operator whose session dies at the wall.
//   - Every OPTIONAL observed destination must NOT be in the pack. Telemetry was
//     contacted by one phase and not the other, and the phase that skipped it
//     still completed, so admitting it would widen the wall for traffic no
//     session needs — the fail-closed posture inverted for convenience.
type postauthDestination struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Tunnels  int    `json:"tunnels"`
	Dialed   int    `json:"dialed"`
	Refused  int    `json:"refused"`
	Required bool   `json:"required"`
	Role     string `json:"role"`
}

type postauthPhase struct {
	Phase        string                `json:"phase"`
	Description  string                `json:"description"`
	ExitCode     int                   `json:"exitCode"`
	Destinations []postauthDestination `json:"destinations"`
}

type postauthEvidence struct {
	CLIVersion   string          `json:"cliVersion"`
	Platform     string          `json:"platform"`
	Capture      string          `json:"capture"`
	Phases       []postauthPhase `json:"phases"`
	EvidenceGaps []string        `json:"evidenceGaps"`
}

func loadPostauthEvidence(t *testing.T) postauthEvidence {
	t.Helper()
	path := filepath.Join("testdata", copilotfixture.PinnedCLIVersion, "postauth_destinations.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading the recorded post-auth destination evidence")
	var evidence postauthEvidence
	require.NoError(t, json.Unmarshal(raw, &evidence), "decoding %s", path)
	require.Equal(t, copilotfixture.PinnedCLIVersion, evidence.CLIVersion,
		"the evidence describes a different CLI release than the suite pins")
	require.NotEmpty(t, evidence.Phases, "the evidence records no phases")
	return evidence
}

func TestCopilotPostAuthDestinationsAreContracted(t *testing.T) {
	evidence := loadPostauthEvidence(t)

	packed, err := sandboxpolicy.ExpandNetworkPackEntries(harness.CopilotFirstPartyNetworkPack)
	require.NoError(t, err, "expanding pack %q", harness.CopilotFirstPartyNetworkPack)

	covers := func(host string, port int) bool {
		return slices.ContainsFunc(packed, func(entry sandboxpolicy.NetworkAllowEntry) bool {
			if !strings.EqualFold(entry.Domain, host) {
				return false
			}
			// No ports means every port; the pack authors 443 explicitly.
			return len(entry.Ports) == 0 || slices.Contains(entry.Ports, port)
		})
	}

	for _, phase := range evidence.Phases {
		require.NotEmpty(t, phase.Destinations,
			"phase %q records no destinations, so it proves nothing", phase.Phase)
		for _, destination := range phase.Destinations {
			if destination.Required {
				assert.True(t, covers(destination.Host, destination.Port),
					"phase %q reached %s:%d (%s) on an authenticated run, and the released "+
						"pack %q does not name it. Under a filtered policy that connection is "+
						"DENIED, so this account's session cannot work at all — the pack needs "+
						"the destination",
					phase.Phase, destination.Host, destination.Port, destination.Role,
					harness.CopilotFirstPartyNetworkPack)
				continue
			}
			assert.False(t, covers(destination.Host, destination.Port),
				"phase %q reached %s:%d (%s), which is recorded as NOT required — another "+
					"phase completed without it. The pack names it anyway, which widens the "+
					"wall for traffic no session needs; either the evidence is wrong about it "+
					"being optional, or the pack should not carry it",
				phase.Phase, destination.Host, destination.Port, destination.Role,
				harness.CopilotFirstPartyNetworkPack)
		}
	}
}

// TestCopilotPostAuthEvidenceProvesThePackIsSufficient checks the other half of
// the recorded evidence: not just that the pack CONTAINS what a session
// reached, but that what it contains is ENOUGH.
//
// The two are genuinely different claims. Every destination in the observing
// phases was reachable while they ran, so a session quietly depending on one
// the pack omits would still have completed and the observation would have
// looked clean. The pack-only-wall phase removes that doubt by making the pack
// the wall — and the fixture has to keep showing that it completed with
// something refused, or the sufficiency claim has quietly become an assumption
// again.
func TestCopilotPostAuthEvidenceProvesThePackIsSufficient(t *testing.T) {
	evidence := loadPostauthEvidence(t)

	index := slices.IndexFunc(evidence.Phases, func(phase postauthPhase) bool {
		return phase.Phase == "pack-only-wall"
	})
	require.GreaterOrEqual(t, index, 0,
		"the evidence records no pack-only-wall phase, so nothing in it shows a session "+
			"surviving with the pack as its only reachable destination set")
	wall := evidence.Phases[index]

	assert.Equal(t, 0, wall.ExitCode,
		"the pack-only-wall phase did not complete, which would mean the pack is "+
			"insufficient for the covered path rather than merely accurate about it")
	assert.True(t,
		slices.ContainsFunc(wall.Destinations, func(destination postauthDestination) bool {
			return destination.Refused > 0 && !destination.Required
		}),
		"the pack-only-wall phase refused nothing, so the wall was never exercised and "+
			"the phase proves only that this particular run stayed inside the pack")
	for _, destination := range wall.Destinations {
		if destination.Required {
			assert.Positive(t, destination.Dialed,
				"%s:%d is recorded as required but was never dialed in the wall phase",
				destination.Host, destination.Port)
		}
	}
}

// TestCopilotPostAuthEvidenceKeepsItsGaps stops the evidence from quietly
// growing into a completeness claim it never made.
//
// What the capture covered is narrow: two phases on one platform with one
// account tier. Token refresh, managed settings, content exclusion and the
// other plan tiers were NOT observed, and the fixture says so. If those
// sentences are ever dropped, the file stops reading as bounded evidence and
// starts reading as an enumeration — which is exactly the mistake the derived
// host list made before an authenticated run contradicted it.
func TestCopilotPostAuthEvidenceKeepsItsGaps(t *testing.T) {
	evidence := loadPostauthEvidence(t)
	assert.NotEmpty(t, evidence.EvidenceGaps,
		"the post-auth evidence lists no gaps; a capture of two phases on one platform "+
			"with one account tier has them, so an empty list means they stopped being "+
			"recorded rather than that they closed")

	joined := strings.ToLower(strings.Join(evidence.EvidenceGaps, "\n"))
	for _, uncovered := range []string{"token refresh", "enterprise"} {
		assert.Contains(t, joined, uncovered,
			"the evidence no longer records %q as uncovered. It was never observed, so if "+
				"the gap is closed it must be closed by a capture that adds a phase, not by "+
				"deleting the sentence that admits it", uncovered)
	}
}
