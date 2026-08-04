package copilotfixture_test

import (
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
// NO_PROXY while pinning every other destination at a capturing proxy that
// records a target and answers 502 without dialing it. See
// RunOptions.WebEgressProxy.
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
//   - NEW: a tool that failed to fetch is not evidence about the prompt. The
//     network is walled off in every arm here, so "the fetch did not succeed"
//     is the uninformative default rather than a finding. What separates the
//     arms is WHICH layer stopped the call, and the three are distinguishable
//     by the exact string the CLI posted back to the model:
//
//     permission layer   "Permission to access this URL was denied."
//     tool absent        "Tool 'web_fetch' does not exist."
//     network layer      "WebFetchBlockedUrlError: ..." / "transport-level failure"
//
//     The layer ORDER is not assumed either; it is measured. See
//     TestCopilotPermissionWebFetchGate/deny-tool-url, where a deny rule on a
//     host that cannot resolve produces the permission-layer string with the
//     capture proxy having seen nothing at all — which is what establishes that
//     the permission layer runs BEFORE any name resolution, and therefore that
//     an arm which reached the network layer is an arm the permission layer let
//     through.

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
func webFetchRun(t *testing.T, turns []copilotfixture.Turn, args []string) (
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
	assertNoUnexpectedEgress(t, capture)
	return verdict, mock, capture, res
}

// assertNoUnexpectedEgress fails a run that tried to reach anything but the
// probe host.
//
// This is the guard that keeps dropping COPILOT_OFFLINE honest. That variable
// is documented as disabling GitHub auth, telemetry, the web tools, the GitHub
// MCP server and auto-update all at once, so removing it to expose web_fetch
// removes the suppression of the other four as well. The proxy is what
// substitutes for it: every destination is recorded and refused, so a release
// that started phoning home under this arm fails here loudly instead of
// quietly making the fixture non-hermetic.
//
// Loopback is exempt because loopback is where the mock provider lives and is
// the one destination NO_PROXY carves out; it never leaves the machine.
//
// THE LIMIT, stated rather than papered over: this observes what the CLI routed
// through its proxy configuration. A component that ignored the proxy variables
// entirely would not appear here. The probe URL's reserved TLD is the
// independent defence against that — no direct socket to it can reach anything
// either — but the destination SET is proxy-observed, not kernel-observed.
func assertNoUnexpectedEgress(t *testing.T, capture *copilotfixture.ProxyCapture) {
	t.Helper()
	var unexpected []string
	for _, host := range capture.Hosts() {
		switch host {
		case webFetchProbeHost, "127.0.0.1", "localhost", "::1":
			continue
		}
		unexpected = append(unexpected, host)
	}
	assert.Empty(t, unexpected,
		"an online-arm run tried to reach %v. Dropping COPILOT_OFFLINE is what makes "+
			"web_fetch measurable, and it also un-suppresses auth, telemetry, the GitHub "+
			"MCP server and auto-update. Every one of those was refused here rather than "+
			"reached, but a NEW destination means this arm is no longer the narrow "+
			"web-tools-only relaxation it is documented to be", unexpected)
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
		BaseURL:   mock.BaseURL(),
		Prompt:    "Answer in one word.",
		ExtraArgs: args,
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
		assertNoUnexpectedEgress(t, capture)
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
			verdict, mock, capture, res := webFetchRun(t, webFetchTurns(), tc.args)
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
				// An allowed arm is allowed precisely because it reached the
				// network layer and failed there. Pinned so the row cannot pass
				// on some other execution path.
				results := copilotfixture.ToolResults(
					mock.Requests()[len(mock.Requests())-1])
				assert.Empty(t, copilotfixture.DenialMarker(results),
					"an allowed arm must carry no permission denial: %q", results)
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
	verdict, mock, capture, res := webFetchRun(t, webFetchTurns(),
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
			verdict, mock, capture, res := webFetchRun(t, webFetchTurns(), args)

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
				results := copilotfixture.ToolResults(
					mock.Requests()[len(mock.Requests())-1])
				assert.Empty(t, copilotfixture.DenialMarker(results),
					"a rule that matches nothing must leave the call to the network "+
						"layer; a permission denial here would mean it IS enforced "+
						"and this row's finding is inverted: %q", results)
			}
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.WebFetchURLDeny, rows)
}
