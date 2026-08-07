package copilotfixture_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The credentialed capture that produced testdata/<version>/postauth_destinations.json.
//
// This is an EVIDENCE STEP, not a test in the ordinary sense, and the
// difference decides everything about how it is gated. It runs the real CLI
// with a real Copilot account against the real service, which means it can only
// ever run locally on a machine an operator has already authenticated, one
// deliberate invocation at a time. It must never run in CI: no provider
// credential belongs in CI, and no fixture in this repo may be derived from
// live credential material beyond the host/port/phase metadata below.
//
// Hence three separate gates — the fixture smoke gate, an explicit opt-in for
// THIS scenario, and an operator-named list of credential stores to stage.
// Missing any one of them skips. Nothing here stages a credential on its own,
// discovers one, or reads one: the operator names directories, they are copied
// byte-for-byte into the disposable root, and the CLI reads them or does not.
//
// What it proves is what the offline contract test then enforces forever after:
// the destinations an authenticated session reaches are the ones the released
// pack names. Running it is how you'd re-record that evidence for a new CLI
// release or a different account tier; the fixture is edited by hand from what
// it prints, which keeps a transcription step — and a human — between a live
// session and the repo.
const (
	authCaptureEnv       = "TCLAUDE_COPILOT_AUTH_CAPTURE"
	authCaptureStoresEnv = "TCLAUDE_COPILOT_AUTH_STORES"
	// Set when the operator's own machine forces outbound traffic through a
	// proxy; the capture then chains through it instead of dialing directly.
	authCaptureUpstreamEnv = "TCLAUDE_COPILOT_AUTH_UPSTREAM_PROXY"
)

func requireAuthCapture(t *testing.T) []string {
	t.Helper()
	requireLab(t)
	if os.Getenv(authCaptureEnv) != "1" {
		t.Skipf("set %s=1 to run the local credentialed destination capture "+
			"(never in CI: it uses a real Copilot account)", authCaptureEnv)
	}
	stores := strings.Split(os.Getenv(authCaptureStoresEnv), string(os.PathListSeparator))
	stores = slices.DeleteFunc(stores, func(path string) bool {
		return strings.TrimSpace(path) == ""
	})
	if len(stores) == 0 {
		t.Skipf("set %s to the credential store directories to stage into the "+
			"disposable home, e.g. %s=$HOME/.config/gh. They are copied verbatim "+
			"and never read by this test", authCaptureStoresEnv, authCaptureStoresEnv)
	}
	return stores
}

// stageAuthStores copies each operator-named directory into the disposable
// root at the same position it occupies under the real home, so the CLI finds
// it exactly where it looks.
//
// The copy is a plain recursive `cp`: this test does not open, parse, redact or
// log a single byte of what it moves, and the destination is a temp root the
// test framework removes. A store outside the real home is copied by basename,
// and an unreadable one fails loudly rather than producing a run that looks
// unauthenticated for a reason nobody can see.
func stageAuthStores(t *testing.T, root string, stores []string) {
	t.Helper()
	home, err := os.UserHomeDir()
	require.NoError(t, err, "resolving the operator home to place credential stores")
	for _, store := range stores {
		source, err := filepath.Abs(strings.TrimSpace(store))
		require.NoError(t, err, "resolving credential store %q", store)
		info, err := os.Stat(source)
		require.NoErrorf(t, err, "credential store %q is not readable; the capture would "+
			"run unauthenticated and prove nothing", source)
		require.Truef(t, info.IsDir(), "credential store %q is not a directory", source)

		relative, err := filepath.Rel(home, source)
		if err != nil || strings.HasPrefix(relative, "..") {
			relative = filepath.Base(source)
		}
		// COPILOT_HOME is redirected away from ~/.copilot by the runner, so a
		// store staged there lands where nothing reads it. Caught here rather
		// than left to surface as the generic "did not complete a turn", which
		// is the same symptom as having staged nothing at all.
		require.NotEqualf(t, ".copilot", relative,
			"staging %q has no effect: the runner points COPILOT_HOME at its own "+
				"disposable directory, so the CLI never reads <root>/.copilot. Name the "+
				"store the CLI actually authenticates from, e.g. $HOME/.config/gh", source)
		destination := filepath.Join(root, relative)
		require.NoError(t, os.MkdirAll(filepath.Dir(destination), 0o700))
		out, err := exec.Command("cp", "-a", source, destination).CombinedOutput()
		require.NoErrorf(t, err, "staging credential store %q: %s", source, out)
	}
}

func TestCopilotAuthenticatedCaptureMatchesContractedDestinations(t *testing.T) {
	stores := requireAuthCapture(t)

	capture := copilotfixture.NewProxyCaptureWithOptions(t, copilotfixture.ProxyCaptureOptions{
		PassThrough:   true,
		UpstreamProxy: os.Getenv(authCaptureUpstreamEnv),
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	stageAuthStores(t, dirs.Root, stores)

	// No Model and no Effort anywhere in this scenario: the account under test
	// cannot select either, so the run must use Copilot's automatic per-question
	// selection. Passing them would measure a launch shape this account does not
	// have rather than the one it does.
	base := copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir:              dirs.WorkDir,
		ProxyEndpoint:        capture.Endpoint(),
		AuthenticatedCapture: true,
		// Generously above RunTimeout: a real turn plus a tool call through a
		// chained proxy took over a minute when this evidence was recorded, and
		// a capture that times out mid-turn under-reports destinations.
		Timeout: 5 * time.Minute,
	}

	sessionID := "8f1c6d2a-3b47-4e58-9a01-2c7d5e6f8b90"

	// Phase one covers startup, the token exchange, one automatically-selected
	// model turn, and one read-only tool call — the shell listing files the
	// runner itself staged, so the tool call touches nothing of the operator's.
	capture.SetPhase("startup-token-exchange-turn-tool")
	first := base
	first.SessionID = sessionID
	first.Prompt = "Use the shell to list the files in the current directory, " +
		"then reply with just the file names."
	firstRun := copilotfixture.Run(t, first)
	require.Equalf(t, 0, firstRun.ExitCode,
		"the authenticated capture did not complete a turn, so its destination set is "+
			"the CLI's failure path rather than its route (stderr: %s)", briefStderr(firstRun.Stderr))

	// Phase two is the resume-by-exact-id path, which relaunches the CLI and so
	// repeats the token exchange; whether it also repeats the model host is the
	// question a resume phase exists to answer.
	capture.SetPhase("exact-id-resume")
	second := base
	second.ResumeID = sessionID
	second.Prompt = "Reply with just the word ok."
	secondRun := copilotfixture.Run(t, second)
	require.Equalf(t, 0, secondRun.ExitCode,
		"the authenticated resume did not complete (stderr: %s)", briefStderr(secondRun.Stderr))

	observations := capture.Observations()
	require.NotEmpty(t, observations, "the authenticated capture observed no destinations at all")
	logObservations(t, observations)

	packed, err := sandboxpolicy.ExpandNetworkPackEntries(harness.CopilotFirstPartyNetworkPack)
	require.NoError(t, err)

	// EVERY dialed destination is checked, not just the ones on domains this
	// change already knows about. Restricting the check to githubcopilot.com
	// and the control-plane host would rebuild the assumption this whole
	// exercise disproved: the endpoints are ASSIGNED, so the next surprise host
	// is exactly the one nobody predicted the shape of. Two skips, both
	// deliberate — loopback never crosses the wall, and telemetry is the
	// observed destination the pack knowingly omits (the wall phase proves a
	// session survives without it).
	var uncontracted []string
	for _, observation := range observations {
		if observation.Dialed == 0 {
			continue
		}
		if isLoopbackHost(observation.Host) {
			continue
		}
		if strings.HasPrefix(observation.Host, "telemetry.") {
			t.Logf("telemetry destination %s:%d observed; deliberately not contracted",
				observation.Host, observation.Port)
			continue
		}
		if !packCovers(packed, observation.Host, observation.Port) {
			uncontracted = append(uncontracted,
				fmt.Sprintf("%s:%d", observation.Host, observation.Port))
		}
	}
	assert.Empty(t, uncontracted,
		"an authenticated session reached %v, which the released pack %q does not name. "+
			"Under a filtered policy those connections are DENIED. Add the destination to "+
			"the pack and record it in testdata/%s/postauth_destinations.json so the "+
			"offline contract test enforces it without a credential",
		uncontracted, harness.CopilotFirstPartyNetworkPack, copilotfixture.PinnedCLIVersion)
}

// TestCopilotAuthenticatedSessionSurvivesThePackOnlyWall is the other half of
// the evidence, and the half a pure observation cannot give.
//
// Watching a session reach a host proves the pack must CONTAIN that host. It
// says nothing about whether the pack is ENOUGH: every other destination the
// session touched was reachable during the observation, so a session that
// silently depended on one would still have completed. This phase removes that
// doubt by building the wall out of the pack itself — only pack-contracted
// destinations are dialed, everything else is refused exactly as a filtered
// launch would refuse it — and then requiring a full turn plus a read-only tool
// call to complete anyway.
//
// It is what turns "telemetry appears optional" into "a session completed with
// telemetry refused".
func TestCopilotAuthenticatedSessionSurvivesThePackOnlyWall(t *testing.T) {
	stores := requireAuthCapture(t)

	packed, err := sandboxpolicy.ExpandNetworkPackEntries(harness.CopilotFirstPartyNetworkPack)
	require.NoError(t, err)
	var allowed []string
	for _, entry := range packed {
		if entry.Domain == "" {
			continue
		}
		for _, port := range entry.Ports {
			allowed = append(allowed, fmt.Sprintf("%s:%d", entry.Domain, port))
		}
	}
	require.NotEmpty(t, allowed, "the pack authorizes no host:port, so there is no wall to test")

	capture := copilotfixture.NewProxyCaptureWithOptions(t, copilotfixture.ProxyCaptureOptions{
		PassThrough:         true,
		UpstreamProxy:       os.Getenv(authCaptureUpstreamEnv),
		AllowedDestinations: allowed,
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	stageAuthStores(t, dirs.Root, stores)

	capture.SetPhase("pack-only-wall")
	run := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir:              dirs.WorkDir,
		ProxyEndpoint:        capture.Endpoint(),
		AuthenticatedCapture: true,
		Timeout:              5 * time.Minute,
		Prompt: "Use the shell to list the files in the current directory, " +
			"then reply with just the file names.",
	})

	observations := capture.Observations()
	logObservations(t, observations)
	require.Equalf(t, 0, run.ExitCode,
		"an authenticated session could not complete a turn when only the pack's own "+
			"destinations were reachable, so the pack is INSUFFICIENT for the covered "+
			"path — something it does not name is load-bearing (stderr: %s)", briefStderr(run.Stderr))

	// The wall has to have bitten SOMETHING OUTSIDE THE PACK, or this proves
	// only that the run happened to touch nothing extra today. A bare
	// "something was refused" would also be satisfied by a refused non-CONNECT
	// request, or by a refusal of a host the pack does name, neither of which
	// is the wall doing its job.
	walled := slices.ContainsFunc(observations, func(observation copilotfixture.ProxyObservation) bool {
		return observation.Refused > 0 &&
			!packCovers(packed, observation.Host, observation.Port)
	})
	assert.True(t, walled,
		"no destination OUTSIDE the pack was refused, so the pack-only wall was never "+
			"exercised; the session may simply not have tried anything outside the pack "+
			"on this run, and the sufficiency claim would rest on that accident")

	// A dial that never completed is the machine's problem, and it must not be
	// read as the wall's decision — the two are the same 502 to the CLI.
	for _, observation := range observations {
		assert.Zerof(t, observation.Failed,
			"%s:%d could not be dialed %d time(s); this run's destination set reflects a "+
				"local network failure rather than the pack's boundary, so it is not "+
				"evidence either way",
			observation.Host, observation.Port, observation.Failed)
	}
}

func logObservations(t *testing.T, observations []copilotfixture.ProxyObservation) {
	t.Helper()
	for _, observation := range observations {
		t.Logf("phase %s reached %s:%d (tunnels=%d dialed=%d refused=%d failed=%d)",
			observation.Phase, observation.Host, observation.Port,
			observation.Tunnels, observation.Dialed, observation.Refused, observation.Failed)
	}
}

// briefStderr bounds what a failure message carries out of a credentialed run.
//
// The CLI's stderr is diagnostics, not model output, and a failing capture is
// unreadable without it — but it is the one place in this scenario where bytes
// from an authenticated session reach a transcript an operator may paste into a
// ticket, so it is capped rather than passed through whole.
func briefStderr(stderr string) string {
	const limit = 400
	stderr = strings.TrimSpace(stderr)
	if len(stderr) <= limit {
		return stderr
	}
	return stderr[:limit] + "… (truncated)"
}

// isLoopbackHost reports the CLI's own local servers (ACP, voice, the
// webview), which never traverse the wall the pack describes.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// packCovers answers whether the released pack authorizes this host and port.
func packCovers(packed []sandboxpolicy.NetworkAllowEntry, host string, port int) bool {
	return slices.ContainsFunc(packed, func(entry sandboxpolicy.NetworkAllowEntry) bool {
		return strings.EqualFold(entry.Domain, host) &&
			(len(entry.Ports) == 0 || slices.Contains(entry.Ports, port))
	})
}
