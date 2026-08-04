package copilotfixture_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-973, the web-fetch gap Phase 0 could not close.
//
// Phase 0 measured the URL permission gate through the SHELL tool and recorded,
// deliberately and repeatedly, that it could say nothing about `web_fetch`:
// every scenario in this suite runs under COPILOT_OFFLINE=true, which is what
// makes it hermetic, and that variable removes web_fetch from the advertised
// tool catalog entirely. The question was therefore not merely unmeasured, it
// was structurally unaskable.
//
// This file asks it, by replacing the hermeticity mechanism rather than
// dropping it. COPILOT_OFFLINE goes away — it has to, it is the thing hiding
// the tool — and in its place the run keeps the BYOK provider on loopback via
// NO_PROXY while routing proxy-aware non-loopback traffic to a capturing proxy
// that records a target and answers 502 without dialing it. See
// RunOptions.WebEgressProxy, and assertNoUnexpectedEgress for exactly how far
// that containment claim reaches — it is a proxy-observed destination set, not
// a kernel-enforced wall, and the difference is stated rather than glossed.
//
// Three disciplines carry over from permission_smoke_test.go, and a fourth is
// specific to this file:
//
//   - Every claim comes from a real PTY. Without a terminal the CLI cannot draw
//     a prompt, so it does not, and a headless run reports "no prompt" for the
//     launch that would deadlock a pane.
//   - Every blocked arm sits next to an allowed arm through the same rig.
//   - A denial and an execution both produce a follow-up provider request, so
//     the tool RESULT is what classifies a launch, never the request count.
//   - NEW: a tool that failed to fetch is not evidence about the prompt. Every
//     arm here routes proxy-aware traffic to the refusing capture, so "the
//     fetch did not succeed" is the uninformative default rather than a
//     finding. What separates the arms is WHICH layer stopped the call, and
//     the three are distinguishable by the exact string the CLI posted back to
//     the model — each ASSERTED, not merely eyeballed:
//
//     permission layer   "Permission to access this URL was denied."
//     tool absent        "Tool 'web_fetch' does not exist."
//     network layer      "Failed to fetch <url>" (the tool's own prefix)
//
//     The layer ORDER is not assumed either; it is measured — and it is the
//     CONTRAST between rows that measures it, not any single observation. With
//     an unresolvable host, TestCopilotPermissionWebFetchGate/deny-tool-url
//     returns the permission-layer string while the allow arms return "Failed
//     to fetch". One row's empty capture would be consistent with either order;
//     the pair is not. So the permission layer runs BEFORE name resolution, and
//     an arm which reached the network layer is an arm it let through.

// webFetchProbeURL is the target every scenario here fetches.
//
// Three properties, each load-bearing:
//
//   - A DOMAIN, not an IP literal. Copilot's URL rules are host-scoped, and a
//     rule-matching measurement built on a bare address would be measuring a
//     different code path from the one an agent's URLs take.
//   - Under .invalid, the RFC 2606 reserved TLD, so it is guaranteed never to
//     resolve to anything anywhere. The strongest arms never get as far as name
//     resolution, but the ALLOWED arms do by construction — that is what makes
//     them allowed — and this is what bounds where they can get to.
//   - Never registrable by anyone, so the guarantee cannot lapse.
const webFetchProbeURL = "https://copilotfixture.invalid/probe"

// webFetchProbeHost is the same host, for the rule-scoping rows.
const webFetchProbeHost = "copilotfixture.invalid"

// webFetchReachedNetwork is the prefix the tool itself emits once it has been
// permitted to run and has failed at the transport layer.
//
// This is the marker that makes an ALLOWED verdict mean what it says, and it is
// load-bearing rather than decorative. ClassifyPermission returns "allowed" for
// any follow-up carrying a tool result with no denial marker in it — and "Tool
// 'web_fetch' does not exist" is such a result. Without this assertion a
// release that renamed the tool, or stopped advertising it for launches without
// --allow-all-tools, would make every allow arm green while the permission gate
// was never reached at all.
//
// Matched on the tool's own "Failed to fetch <url>" prefix rather than on the
// specific transport error, deliberately: the error text below it is the
// resolver's ("Temporary failure in name resolution" here, something else on a
// runner with a different DNS setup), so asserting it would tie the suite to
// the host's networking. The prefix proves what the scenario needs — the tool
// existed, was permitted, ran, and got as far as the network.
const webFetchReachedNetwork = "Failed to fetch " + webFetchProbeURL

// webFetchRoutableProbeURL is the target of the egress-wall positive control.
//
// RFC 5737 TEST-NET-1: reserved for documentation and not intended to be
// routed, and an IP literal, so the fetch needs no name resolution and reaches
// the proxy's CONNECT path instead of dying at DNS the way the .invalid host
// does. That is the whole reason a second target exists — see
// TestCopilotPermissionWebFetchEgressWallIsInForce.
//
// Stated at RFC strength deliberately: 5737 says such addresses SHOULD NOT
// appear on the public Internet, which is a convention rather than a
// guarantee that a packet cannot leave. The containment this arm relies on is
// the proxy plus the absence of credentials, not this address.
const webFetchRoutableProbeURL = "https://192.0.2.1/probe"

// webFetchRoutableProbeHost is the same address, as the proxy records it.
const webFetchRoutableProbeHost = "192.0.2.1"

// webFetchToolName is the tool identifier as the CLI spells it in its catalog
// and in --excluded-tools.
//
// Pinned as a constant because the exclusion measurement is ABOUT the spelling.
// The brief this file answers asked for the exact identifier to be proven
// rather than guessed, since a spawner that excluded "webfetch" or "web-fetch"
// would silently exclude nothing and leave the tool live.
const webFetchToolName = "web_fetch"

// webFetchDeadline bounds one web-fetch scenario.
//
// Longer than permissionDeadline because the allowed arms run a real DNS
// lookup, which on a host with an unreachable or slow resolver costs seconds
// that the shell-only scenarios never pay. Blocked arms still settle at
// quiescence, so the extra headroom costs nothing when it is not needed.
const webFetchDeadline = 20 * time.Second

// webFetchTurns scripts a single web_fetch call at the probe URL.
func webFetchTurns() []copilotfixture.Turn {
	return []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_webfetch",
			Name: webFetchToolName,
			Args: `{"url":"` + webFetchProbeURL + `"}`,
		}},
		{Text: "MOCK WEB FETCH FOLLOW UP"},
	}
}

// webFetchRun executes one online-arm pty scenario and classifies it.
//
// It mirrors permissionRun deliberately rather than sharing code with it: the
// two differ in the one respect that matters here — this one drops
// COPILOT_OFFLINE and installs the egress wall — and factoring them together
// would invite an edit that silently reintroduced the offline flag and turned
// every scenario in this file into a measurement of a tool that is not there.
//
// probeHost is the ONE non-loopback destination this run is allowed to route a
// connection to — the host of the URL its scripted tool call targets. It is a
// parameter rather than a package-level allowlist so that each call site
// declares what it intends: a row that targeted an unexpected host would
// otherwise be silently blessed by an allowlist widened for some other
// scenario. Pass "" for a run that must route nothing at all.
func webFetchRun(t *testing.T, turns []copilotfixture.Turn, probeHost string, args []string) (
	copilotfixture.PermissionVerdict, *copilotfixture.MockProvider,
	*copilotfixture.ProxyCapture, copilotfixture.PTYResult,
) {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t, turns)
	capture := copilotfixture.NewProxyCapture(t)
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: copilotfixture.RunOptions{
			Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
			BaseURL:        mock.BaseURL(),
			WebEgressProxy: capture.Endpoint(),
			Prompt:         "Fetch the URL as instructed.",
			// Dropped so no arm smuggles in a grant it is not naming. This is
			// not a detail: the runner ADDS --allow-all-tools by default, so an
			// arm named "no-flags" that omitted this would silently be an
			// allow-all arm, and the whole matrix would report that web_fetch
			// never prompts. That mistake was made and caught while writing
			// this file, which is why it is stated here rather than assumed.
			OmitAllowAllTools: true,
			ExtraArgs:         args,
		},
		Deadline:    webFetchDeadline,
		SettledWhen: func() bool { return len(mock.Requests()) >= 2 },
	})

	requests := mock.Requests()
	total := len(requests)
	var toolResults []string
	if total > 1 {
		toolResults = copilotfixture.ToolResults(requests[total-1])
	}
	verdict, err := copilotfixture.ClassifyPermission(
		total, max(total-1, 0), !res.Exited, res.Quiesced, res.TranscriptText(), toolResults)
	require.NoError(t, err, "the launch could not be classified")

	t.Logf("permission verdict: %s (%s)", verdict.Outcome, verdict.Evidence)
	assertNoUnexpectedEgress(t, capture, probeHost)
	return verdict, mock, capture, res
}

// assertNoUnexpectedEgress fails a run that routed a connection to anything but
// a probe host.
//
// This is the guard that keeps dropping COPILOT_OFFLINE honest. That variable is
// documented as disabling GitHub auth, telemetry, the web tools, the GitHub MCP
// server and auto-update all at once, so removing it to expose web_fetch removes
// the suppression of the other four as well. The proxy substitutes for it: a
// proxy-aware connection is recorded and refused with a 502 rather than dialed,
// so a release that started phoning home under this arm fails here loudly.
//
// Loopback is exempt because loopback is where the mock provider lives and is
// the one destination NO_PROXY carves out; it never leaves the machine. The
// exemption cannot widen the blast radius: the only other entry in the
// allowlist is a probe host, so any real destination fails this assertion.
//
// WHAT THIS DOES AND DOES NOT ESTABLISH, stated precisely because the natural
// phrasing overstates it:
//
//   - It DOES establish that all PROXY-AWARE non-loopback traffic went to the
//     refusing capture, and that no unexpected destination was observed.
//     TestCopilotPermissionWebFetchEgressWallIsInForce is what stops that from
//     being vacuous, by proving the proxy is genuinely on the path.
//   - It does NOT establish a kernel-enforced no-egress boundary. This is a
//     proxy-observed destination set, not a kernel-observed one, so a component
//     that ignored the proxy variables entirely would not appear here and is
//     not ruled out.
//
// Three independent properties are what make that residual gap acceptable
// rather than merely acknowledged: the run carries no credentials (every token
// variable is scrubbed, exactly as in every other arm), the only
// model-directed destinations are reserved addresses that route nowhere, and
// the behavioural finding does not depend on any external access succeeding —
// every arm is classified from what the CLI told the model, not from a fetch.
func assertNoUnexpectedEgress(
	t *testing.T, capture *copilotfixture.ProxyCapture, probeHost string,
) {
	t.Helper()
	var unexpected []string
	for _, host := range capture.Hosts() {
		switch host {
		case "127.0.0.1", "localhost", "::1":
			continue
		case probeHost:
			// Only the host this particular run's tool call targets, and only
			// when the caller named it. Never a package-level allowlist.
			if probeHost != "" {
				continue
			}
		}
		unexpected = append(unexpected, host)
	}
	assert.Empty(t, unexpected,
		"an online-arm run routed a connection to %v. Dropping COPILOT_OFFLINE is what "+
			"makes web_fetch measurable, and it also un-suppresses auth, telemetry, the "+
			"GitHub MCP server and auto-update. None of those was observed here, but a "+
			"NEW destination means this arm is no longer the narrow web-tools-only "+
			"relaxation it is documented to be", unexpected)
}

// assertReachedNetworkLayer fails an arm whose tool result does not show the
// tool having actually run.
//
// This is what separates "the permission gate opened" from every other way a
// launch can produce a follow-up request with no denial in it. The classifier
// cannot make that distinction on its own — it reports "allowed" for any tool
// result without a denial marker, and an absent tool's "Tool 'web_fetch' does
// not exist" qualifies. So an allow arm asserts positively that the tool
// existed, was permitted, ran, and failed at the transport layer, rather than
// merely asserting the absence of a denial.
func assertReachedNetworkLayer(t *testing.T, mock *copilotfixture.MockProvider) {
	t.Helper()
	requests := mock.Requests()
	require.GreaterOrEqual(t, len(requests), 2, "no follow-up request to read a tool result from")
	results := copilotfixture.ToolResults(requests[len(requests)-1])
	require.NotEmpty(t, results, "the follow-up request carried no tool result")
	joined := strings.Join(results, "\n")

	assert.Empty(t, copilotfixture.DenialMarker(results),
		"an allowed arm must carry no permission denial: %q", results)
	assert.NotContains(t, joined, "does not exist",
		"the tool was ABSENT rather than permitted, so this arm says nothing about the "+
			"permission gate. A release that renamed or stopped advertising web_fetch "+
			"would otherwise classify as 'allowed': %q", results)
	assert.Contains(t, joined, webFetchReachedNetwork,
		"an allowed arm must show the tool having run and reached the transport layer, "+
			"which is what proves the permission gate opened rather than the call having "+
			"been stopped by some earlier path: %q", results)
}

// webFetchCatalog returns the advertised tool names for one launch shape.
//
// The catalog is the strongest instrument available for any question about tool
// availability: it is a pure wire read, so it depends neither on scraping the
// TUI nor on the model choosing to call anything.
func webFetchCatalog(t *testing.T, online bool, args ...string) []string {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t,
		[]copilotfixture.Turn{{Text: "MOCK WEB FETCH CATALOG PROBE"}})
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
	opts := copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "Answer in one word.",
		// The catalog is observed for the FLAG-FREE launch shape, so the
		// precondition it establishes — that web_fetch is advertised at all —
		// covers the no-flags row rather than only the allow-all ones. The turn
		// carries no tool call, so nothing here can prompt.
		OmitAllowAllTools: true,
		ExtraArgs:         args,
	}
	var capture *copilotfixture.ProxyCapture
	if online {
		capture = copilotfixture.NewProxyCapture(t)
		opts.WebEgressProxy = capture.Endpoint()
	}
	copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: opts,
		Deadline:   webFetchDeadline,
		// The catalog rides on the first request, so there is nothing to gain
		// by waiting for a turn that has no tool call in it.
		SettledWhen: func() bool { return len(mock.Requests()) >= 1 },
	})
	requests := mock.Requests()
	require.NotEmpty(t, requests, "the catalog probe must reach the provider")
	assertCredentialFree(t, mock)
	if capture != nil {
		// A catalog probe's turn carries no tool call, so it must route nothing
		// at all: there is no probe host to permit.
		assertNoUnexpectedEgress(t, capture, "")
	}
	return newSanitizer(dirs).Request(requests[0]).ToolNames
}

// TestCopilotPermissionWebFetchNeedsTheOnlineArm establishes the precondition
// every other scenario in this file rests on, and it is worth a test of its own
// because the alternative is a suite whose arms all silently measure nothing.
//
// Phase 0's stated blocker was that COPILOT_OFFLINE=true removes web_fetch from
// the catalog. That is confirmed here rather than taken on trust, and the
// measurement is deliberately two-sided: the online arm must ADD web_fetch, and
// it must add web_fetch AND NOTHING ELSE. The second half is the one that
// matters for the security story. Dropping COPILOT_OFFLINE is documented as
// re-enabling auth, telemetry, the GitHub MCP server and auto-update along with
// the web tools, so if that relaxation widened the model's reach beyond the one
// tool under study — an MCP toolset appearing, say — this arm would not be the
// narrow probe it is described as.
func TestCopilotPermissionWebFetchNeedsTheOnlineArm(t *testing.T) {
	requireSmoke(t)

	offline := webFetchCatalog(t, false)
	assert.NotContains(t, offline, webFetchToolName,
		"COPILOT_OFFLINE=true must remove web_fetch; if it stops doing so, this suite's "+
			"other scenarios are no longer hermetic for the reason they claim to be")

	online := webFetchCatalog(t, true)
	assert.Contains(t, online, webFetchToolName,
		"dropping COPILOT_OFFLINE must advertise web_fetch, or every scenario in this "+
			"file is measuring the permission behaviour of a tool that is not there")

	assert.Len(t, online, len(offline)+1,
		"the online arm must add web_fetch and nothing else. A wider catalog means "+
			"dropping COPILOT_OFFLINE gave the model reach this measurement does not "+
			"account for: %v", online)
	t.Logf("permission verdict: web_fetch requires the online arm "+
		"(catalog %d -> %d tools, added exactly %q)",
		len(offline), len(online), webFetchToolName)
}

// TestCopilotPermissionWebFetchEgressWallIsInForce is the positive control for
// every "the proxy saw nothing" assertion in this file, and without it they are
// all vacuous.
//
// The problem it solves: "the capture observed no unexpected destination" is
// satisfied just as well by a capture that observed NOTHING — including because
// the CLI stopped honouring the proxy variables and dialed directly. That is
// precisely the regression the wall exists to catch, and it is the one shape in
// which every other egress assertion in this file goes green while the arm is
// no longer contained at all. The sibling scenario for the first-party route
// (TestCopilotStartupDialsOnlyContractedHosts) guards the same hole with a
// NotEmpty check; this is that guard for the online arm.
//
// It needs its own target, and that is the whole reason a second URL constant
// exists. The .invalid host every other scenario uses dies at name resolution,
// which happens before the proxy is ever consulted — so those runs legitimately
// observe nothing and cannot serve as the control. A TEST-NET-1 IP literal needs
// no resolution, so the fetch reaches the proxy's CONNECT path, is recorded, and
// is refused with a 502 without a packet being sent to it. RFC 5737 reserves
// the address for documentation and it is not intended to be routed, so a
// component that ignored the proxy entirely would have nothing to reach — a
// convention this leans on, not a guarantee it asserts.
func TestCopilotPermissionWebFetchEgressWallIsInForce(t *testing.T) {
	requireSmoke(t)

	turns := []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_webfetch_egress",
			Name: webFetchToolName,
			Args: `{"url":"` + webFetchRoutableProbeURL + `"}`,
		}},
		{Text: "MOCK WEB FETCH EGRESS FOLLOW UP"},
	}
	// Permitted, so the call reaches the transport layer: a blocked or denied
	// arm never gets far enough to exercise the proxy at all.
	_, mock, capture, _ := webFetchRun(t, turns, webFetchRoutableProbeHost,
		[]string{"--allow-all-tools", "--no-ask-user"})

	assert.Contains(t, capture.Hosts(), webFetchRoutableProbeHost,
		"web_fetch traffic must traverse the capture proxy. If it does not, the CLI is "+
			"no longer honouring the proxy variables for this path, every "+
			"'the proxy observed nothing' assertion in this file has become vacuous, "+
			"and the online arm is not contained by the mechanism it claims. Observed: %v",
		capture.Hosts())

	results := copilotfixture.ToolResults(mock.Requests()[len(mock.Requests())-1])
	require.NotEmpty(t, results)
	assert.Contains(t, strings.Join(results, "\n"), "Failed to fetch "+webFetchRoutableProbeURL,
		"the refusal must come back as a transport failure, i.e. the proxy answered "+
			"instead of the destination: %q", results)
	t.Logf("permission verdict: the online arm's egress wall is in force "+
		"(web_fetch dialed %v through the refusing capture proxy)", capture.Hosts())
	assertCredentialFree(t, mock)
}

// TestCopilotPermissionWebFetchGate is the measurement TCL-973 was missing: does
// a real interactive pane deadlock on web_fetch, and does the posture the
// approval-core work proposes close it?
//
// The answer is yes and yes, and BOTH halves are findings.
//
// The no-flags arm BLOCKS on the URL dialog. So web_fetch is a genuine third
// deadlock source for a detached agent, alongside folder trust and shell tool
// approval — Phase 0 could not establish that, and the conservative reading it
// left behind (treat web_fetch as unknown, keep detached spawn fail-closed) was
// the right one.
//
// And `--allow-all-tools` closes it. That extends Phase 0's shell-path
// correction to the web-fetch path: the TCL-973 plan's assumption that URL
// access is an axis needing its own grant alongside tool approval does not hold
// for EITHER URL consumer in 1.0.77. `--no-ask-user` is measured alongside
// rather than separately because the proposed unattended posture is the pair,
// and a posture is only nonblocking if it is nonblocking as a whole.
func TestCopilotPermissionWebFetchGate(t *testing.T) {
	var rows []string
	for _, tc := range []struct {
		name string
		args []string
		want copilotfixture.PermissionOutcome
	}{
		// The deadlock claim itself.
		{name: "no-flags", want: copilotfixture.PermissionBlocked},
		// The flag that closes it, alone — so the row below cannot be credited
		// to --no-ask-user, which has nothing to do with URLs.
		{name: "allow-all-tools", args: []string{"--allow-all-tools"},
			want: copilotfixture.PermissionAllowed},
		// The posture actually proposed for unattended spawn.
		{name: "allow-all-tools/no-ask-user",
			args: []string{"--allow-all-tools", "--no-ask-user"},
			want: copilotfixture.PermissionAllowed},
		// The dedicated URL grant closes it too, which is what makes the
		// prompt above a URL decision rather than a tool-approval one.
		{name: "allow-all-urls", args: []string{"--allow-all-urls"},
			want: copilotfixture.PermissionAllowed},
		// The narrow deny, and the row that establishes the LAYER ORDER the
		// whole file depends on: it denies at the permission layer with the
		// capture proxy having observed nothing, on a host that could not have
		// resolved. So the permission layer precedes name resolution, and an
		// arm that reached a network error is an arm it let through.
		{name: "deny-tool-url", args: []string{"--deny-tool", "url"},
			want: copilotfixture.PermissionDenied},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmoke(t)
			verdict, mock, capture, res := webFetchRun(t, webFetchTurns(), webFetchProbeHost, tc.args)
			assert.Equal(t, tc.want, verdict.Outcome)

			switch tc.want {
			case copilotfixture.PermissionBlocked:
				// Without this the row would read "blocked" for a release that
				// dropped or errored the call instead of asking, which is a
				// different finding entirely.
				assert.True(t, res.Contains("attempting to access the following URL"),
					"a blocked web_fetch must show the URL dialog, which is what makes "+
						"this a URL decision rather than ordinary tool approval")
				assert.True(t, res.Contains(webFetchProbeURL),
					"the dialog must name the URL it is asking about")
				assert.False(t, res.Exited,
					"a blocked pane stays alive forever; that IS the deadlock")
				assert.Empty(t, capture.Hosts(),
					"the gate must fire BEFORE any network access, so a blocked arm "+
						"dials nothing at all")

			case copilotfixture.PermissionDenied:
				assert.Empty(t, capture.Hosts(),
					"a permission-layer denial must precede name resolution; a dialed "+
						"host here would mean the deny rule ran AFTER the network and "+
						"this file's layer-ordering reasoning is wrong")

			case copilotfixture.PermissionAllowed:
				assertReachedNetworkLayer(t, mock)
				assert.False(t, res.Contains("attempting to access the following URL"),
					"an allowed arm must not have drawn the URL dialog")
			}

			require.NotEmpty(t, mock.Requests(),
				"trust was pre-granted, so the provider must have been reached")
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.WebFetchGate, rows)
}

// TestCopilotPermissionWebFetchExclusionRemovesTheTool measures the other way to
// make web_fetch nonblocking: take it away instead of granting it.
//
// This is the option a fail-closed detached posture wants, because it removes
// the capability rather than permitting it, and the brief asked for the exact
// tool identifier and the flag's real behaviour to be PROVEN rather than
// guessed — a spawner that excluded "webfetch" or "web-fetch" would exclude
// nothing at all and leave the tool live under an argv that reads as if it had.
//
// Two things are established, and the second is what makes the option usable.
// The tool leaves the catalog. And a model that asks for it anyway does not
// deadlock: the CLI answers "Tool 'web_fetch' does not exist", names the tools
// that do, and the turn continues — no prompt, with NO permission flags granted
// anywhere in the launch.
func TestCopilotPermissionWebFetchExclusionRemovesTheTool(t *testing.T) {
	requireSmoke(t)

	base := webFetchCatalog(t, true)
	require.Contains(t, base, webFetchToolName,
		"without this the scenario would be asserting the removal of something that "+
			"was never advertised")

	excluded := webFetchCatalog(t, true, "--excluded-tools", webFetchToolName)
	assert.NotContains(t, excluded, webFetchToolName,
		"--excluded-tools %s must remove the tool from the advertised catalog",
		webFetchToolName)
	assert.Len(t, excluded, len(base)-1,
		"--excluded-tools must remove web_fetch and nothing else")

	// The half that matters for a detached pane: an excluded tool the model
	// calls anyway must not become a prompt.
	verdict, mock, capture, res := webFetchRun(t, webFetchTurns(), webFetchProbeHost,
		[]string{"--excluded-tools", webFetchToolName})
	require.GreaterOrEqual(t, len(mock.Requests()), 2,
		"the CLI must answer the model rather than stopping to ask")
	results := copilotfixture.ToolResults(mock.Requests()[len(mock.Requests())-1])
	assert.Contains(t, results[0], "does not exist",
		"the model must be told the tool is absent, which is what lets the turn "+
			"continue instead of parking the pane")
	assert.False(t, res.Contains("attempting to access the following URL"),
		"an absent tool cannot prompt; a URL dialog here would mean the exclusion "+
			"did not take effect")
	assert.Empty(t, capture.Hosts(),
		"an excluded tool must reach no network at all")
	// Classified for the record rather than asserted as a permission outcome:
	// "the tool does not exist" is not a permission verdict, and reading it as
	// one would be exactly the overstatement this suite avoids elsewhere.
	t.Logf("permission verdict: --excluded-tools %s removes the tool "+
		"(catalog %d -> %d) and a call to it is answered, not prompted "+
		"(classified %s)", webFetchToolName, len(base), len(excluded), verdict.Outcome)
	assertCredentialFree(t, mock)
}

// TestCopilotPermissionWebFetchURLDenyEnforcement converts the single most
// consequential unfixtured claim in the Phase 0 contract into a measurement,
// and overturns half of it.
//
// Phase 0 recorded, from an independent uncommitted rig, that URL rules PARSE
// and ENFORCE differently: wildcard spellings are accepted at the command line
// and then match nothing at runtime. Its own deny-tool grammar scenario could
// only see the parse half, and the contract was explicit that reading "parses"
// as "works" would ship a blanket URL deny that denies nothing.
//
// Measured here against the real binary through web_fetch, the wildcard half
// holds: `url(*)`, `--deny-url '*'` and `--deny-url 'https://*'` all parse, all
// match nothing, and the call falls through to the network layer.
//
// The other half does not. Phase 0's corroborating note concluded that there is
// therefore NO working blanket URL deny in 1.0.77 by any spelling. There is: the
// BARE KIND `--deny-tool url`, with no parentheses, denies every URL at the
// permission layer with no prompt and no name resolution. It is a different
// spelling from the wildcard forms and it behaves differently from them, which
// is precisely why it could not be inferred from them.
//
// Every row pairs its rule with `--allow-all-tools --no-ask-user`, so a denial
// can only come from the rule under test and never from an absent grant — and
// so each denying row also re-establishes that a launch-time deny beats a
// blanket allow, on the URL axis this time rather than the shell one.
func TestCopilotPermissionWebFetchURLDenyEnforcement(t *testing.T) {
	var rows []string
	for _, tc := range []struct {
		name     string
		rule     []string
		enforced bool
	}{
		// The bare kind: a real blanket URL deny.
		{name: "deny-tool/url", rule: []string{"--deny-tool", "url"}, enforced: true},
		// Host-scoped spellings, both flags. These were the ones Phase 0's
		// corroboration expected to work, and they do.
		{name: "deny-tool/url(host)",
			rule: []string{"--deny-tool", "url(" + webFetchProbeHost + ")"}, enforced: true},
		{name: "deny-url/host",
			rule: []string{"--deny-url", webFetchProbeHost}, enforced: true},
		// The wildcards. Each PARSES — the launch reaches the provider, which
		// TestCopilotPermissionDenyToolGrammar pins independently — and then
		// matches nothing.
		{name: "deny-tool/url(*)", rule: []string{"--deny-tool", "url(*)"}},
		{name: "deny-url/*", rule: []string{"--deny-url", "*"}},
		{name: "deny-url/https-star", rule: []string{"--deny-url", "https://*"}},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmoke(t)
			args := append([]string{"--allow-all-tools", "--no-ask-user"}, tc.rule...)
			verdict, mock, capture, res := webFetchRun(t, webFetchTurns(), webFetchProbeHost, args)

			want := copilotfixture.PermissionAllowed
			if tc.enforced {
				want = copilotfixture.PermissionDenied
			}
			assert.Equal(t, want, verdict.Outcome,
				"whether %v is ENFORCED at the permission layer, which is a different "+
					"question from whether it parses", tc.rule)

			// The rule was accepted either way; a parse error would have killed
			// the launch before the provider. Without this a non-enforcing row
			// could pass while actually being a rejected flag.
			require.NotEmpty(t, mock.Requests(),
				"every rule here must PARSE, so the launch must reach the provider")
			// No arm may prompt: a deny rule never asks, and the paired
			// allow-all closes the ordinary URL dialog.
			assert.False(t, res.Contains("attempting to access the following URL"),
				"no row here may draw the URL dialog")

			if tc.enforced {
				assert.Empty(t, capture.Hosts(),
					"an enforced rule must stop the call before name resolution")
			} else {
				// Asserted positively rather than as the absence of a denial: a
				// rule that "matches nothing" must be shown to have let the call
				// through to the transport layer, not merely shown not to have
				// denied it. Otherwise a row could report a wildcard as inert
				// when the call was actually stopped somewhere else entirely.
				assertReachedNetworkLayer(t, mock)
			}
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.WebFetchURLDeny, rows)
}
