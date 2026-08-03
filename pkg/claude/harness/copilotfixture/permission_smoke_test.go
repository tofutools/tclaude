package copilotfixture_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-973 Phase 0: what Copilot 1.0.77 does to an UNATTENDED launch.
//
// tclaude spawns Copilot into a tmux pane with nobody watching. Every prompt
// the CLI can draw is therefore a permanent deadlock, and a deadlocked agent
// is indistinguishable from a working one. These scenarios measure which
// prompts exist, in what order, and which launch inputs actually remove them.
//
// Two disciplines run through the file, both learned by getting them wrong:
//
//   - Every permission claim comes from a REAL PTY (pty.go). Without a
//     terminal the CLI cannot draw a prompt, so it does not — a headless run
//     reports "no prompt" for the very launch that would deadlock a pane.
//     The non-TTY behavior is measured too, but as a separate, explicitly
//     labelled confound: see TestCopilotPermissionHeadlessIsNotEvidence.
//   - Every scenario carries its own positive control. "The tool did not run"
//     is the same observation for a permission block, a broken fixture and a
//     typo'd flag, so a blocked arm only means something next to an arm that
//     was allowed through the same rig.
//
// Runtime note: interactive mode never exits — after a turn the CLI sits at
// its input prompt — so each scenario ends on its own evidence via
// SettledWhen, not on process exit.

// permissionDeadline bounds one pty scenario.
//
// Sized off measured behavior, not guessed: a mock-backed turn reaches its
// tool-result follow-up in ~2-4s, and an allowed arm ends as soon as that
// arrives. Only genuinely blocked arms pay the full deadline, and 20s is many
// times the slowest observed startup while keeping the whole matrix inside the
// CI job's budget.
const permissionDeadline = 20 * time.Second

// safeShellCommand is a command Copilot classifies as trivially safe.
//
// It is here to be a CONTROL, and it encodes a finding: with folder trust
// granted and NO permission flags at all, `echo` executes without a prompt.
// A matrix built only on commands like this would conclude that Copilot never
// asks for tool approval, which is false. Pairing it with unsafeShellCommand
// is what makes the tool-approval gate visible.
const safeShellCommand = "echo copilotfixture-permission-probe"

// unsafeShellCommand is a command Copilot does NOT auto-approve.
//
// Destructive in shape but inert in fact: it removes a file that is never
// created, inside the run's own disposable working directory. The scenario
// needs Copilot's CLASSIFICATION of the command, not its effect.
const unsafeShellCommand = "rm -f ./copilotfixture-permission-victim"

// urlShellCommand reaches a URL through the SHELL tool.
//
// This is what makes the URL prompt measurable inside the suite's hermetic
// environment. COPILOT_OFFLINE=true removes the web-fetch tool from the
// catalog entirely, so the obvious instrument is unavailable — but the URL
// permission gate also covers shell commands, and it fires BEFORE any network
// access is attempted. So the prompt is observable with no egress whatsoever:
// nothing is ever dialled, because the CLI stops to ask first.
const urlShellCommand = "curl -s https://example.com"

func bashTurns(command string) []copilotfixture.Turn {
	return []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_permission",
			Name: "bash",
			Args: `{"command":"` + command + `","description":"copilotfixture permission probe"}`,
		}},
		{Text: "MOCK PERMISSION FOLLOW UP"},
	}
}

// permissionRun executes one pty scenario and classifies it.
//
// Every scenario goes through here so the instrument is identical across the
// matrix: the same evidence (a tool-result follow-up request), the same
// settle-early rule, and the same classifier — including its error arm, which
// is what stops an inconclusive run from being recorded as a finding.
func permissionRun(
	t *testing.T, turns []copilotfixture.Turn, trusted bool, opts copilotfixture.RunOptions,
) (copilotfixture.PermissionVerdict, *copilotfixture.MockProvider, copilotfixture.PTYResult) {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t, turns)
	dirs := copilotfixture.NewSandboxDirs(t)
	if trusted {
		copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
	}
	opts.Root, opts.Home, opts.Cache, opts.WorkDir = dirs.Root, dirs.Home, dirs.Cache, dirs.WorkDir
	opts.BaseURL = mock.BaseURL()
	if opts.Prompt == "" {
		opts.Prompt = "Use the tool as instructed."
	}

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: opts,
		Deadline:   permissionDeadline,
		// The follow-up request is conclusive: it can only exist if the tool
		// executed and produced a result to post back.
		SettledWhen: func() bool { return len(mock.Requests()) >= 2 },
	})

	total := len(mock.Requests())
	followUps := max(total-1, 0)
	verdict, err := copilotfixture.ClassifyPermission(
		total, followUps, !res.Exited, res.Quiesced, res.TranscriptText())
	require.NoError(t, err, "the launch could not be classified")

	// The line CI greps. Asserting the VALUE rather than merely that the test
	// passed is what keeps a scenario from silently starting to measure a
	// different arm than the one it is named for.
	t.Logf("permission verdict: %s (%s)", verdict.Outcome, verdict.Evidence)
	return verdict, mock, res
}

// TestCopilotPermissionFolderTrustBlocksFirst is the headline measurement, and
// it reorders TCL-973's whole problem statement.
//
// The plan this ticket serves assumed the first thing a detached Copilot agent
// hits is a tool-approval prompt. It is not. With a fresh COPILOT_HOME the CLI
// blocks at LAUNCH on a folder-trust dialog, before the model is contacted at
// all — zero provider requests. A detached pane dies here, before any flag in
// the proposed approval catalog has a chance to matter.
func TestCopilotPermissionFolderTrustBlocksFirst(t *testing.T) {
	requireSmoke(t)

	verdict, mock, res := permissionRun(t, bashTurns(safeShellCommand), false,
		copilotfixture.RunOptions{})

	assert.Equal(t, copilotfixture.PermissionBlocked, verdict.Outcome)
	assert.Empty(t, mock.Requests(),
		"the trust gate must block BEFORE the provider is contacted; a request here "+
			"would mean the gate moved and the whole ordering claim needs remeasuring")
	assert.True(t, res.Contains(copilotfixture.TrustPromptMarker),
		"the blocking dialog must be the folder-trust one")
	// The specific wording that makes this a human decision rather than a
	// technical gate. It is quoted here because tclaude pre-answering it is a
	// security-review question, not an implementation detail.
	assert.True(t, res.Contains("Do you trust the files in this folder?"))
	assert.False(t, res.Exited, "a blocked pane stays alive forever; that IS the deadlock")
}

// TestCopilotPermissionTrustBypassSurface maps everything that could plausibly
// clear the trust gate, because the answer decides what shape tclaude's fix
// has to take.
//
// The result is that NO launch flag clears it. Only a pre-launch config write
// does. That makes DirTrust a config-FILE contract for Copilot — the direct
// analogue of codex_dir_trust.go — and not something an argv renderer can
// express, which is what the TCL-973 plan had assumed.
func TestCopilotPermissionTrustBypassSurface(t *testing.T) {
	requireSmoke(t)

	var rows []string
	for _, tc := range []struct {
		name   string
		args   []string
		seed   string
		clears bool
	}{
		// Every flag a reader would reasonably expect to work. None do.
		{name: "allow-all-tools", args: []string{"--allow-all-tools"}},
		{name: "allow-all", args: []string{"--allow-all"}},
		{name: "allow-all-paths", args: []string{"--allow-all-paths"}},
		{name: "add-dir-workdir", args: []string{"--add-dir"}},
		// The only thing that does.
		{name: "config-json-trustedFolders", seed: "config.json", clears: true},
		// Measured separately because it is the file the CLI MIGRATES TO, so
		// it is the file a reader would assume is authoritative. A flat
		// trustedFolders key written there was NOT honoured by 1.0.77. Stated
		// exactly that narrowly: this does not establish that settings.json can
		// never carry the grant, only that this spelling did not.
		{name: "settings-json-trustedFolders", seed: "settings.json"},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			mock := copilotfixture.NewMockProvider(t, bashTurns(safeShellCommand))
			dirs := copilotfixture.NewSandboxDirs(t)
			args := tc.args
			if tc.name == "add-dir-workdir" {
				args = []string{"--add-dir", dirs.WorkDir}
			}
			if tc.seed != "" {
				seedTrustedFolders(t, dirs.Home, dirs.WorkDir, tc.seed)
			}
			res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
				RunOptions: copilotfixture.RunOptions{
					Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
					BaseURL: mock.BaseURL(),
					Prompt:  "Use the tool as instructed.",
					// Dropped so no arm smuggles in a permission grant the row
					// is not naming; the flags under test are supplied above.
					OmitAllowAllTools: true,
					ExtraArgs:         args,
				},
				Deadline:    permissionDeadline,
				SettledWhen: func() bool { return len(mock.Requests()) >= 2 },
			})

			reached := len(mock.Requests()) > 0
			t.Logf("permission verdict: trust-cleared=%v (provider requests: %d)",
				reached, len(mock.Requests()))
			assert.Equal(t, tc.clears, reached,
				"whether the launch got past the folder-trust gate")
			assert.Equal(t, !tc.clears, res.Contains(copilotfixture.TrustPromptMarker),
				"the trust dialog's presence must agree with whether the provider was reached")
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.TrustBypass, rows)
}

// TestCopilotPermissionToolApprovalGate measures the gate the plan expected to
// find first, now that the trust gate is out of the way.
//
// It exists, `--allow-all-tools` removes it, and it does NOT apply to every
// tool call: Copilot auto-approves commands it considers safe. That last part
// is the reason this test carries three rows instead of two. A matrix built on
// a safe command alone would have reported the gate as absent.
func TestCopilotPermissionToolApprovalGate(t *testing.T) {
	requireSmoke(t)

	var rows []string
	for _, tc := range []struct {
		name    string
		command string
		allow   bool
		want    copilotfixture.PermissionOutcome
	}{
		// The deadlock claim itself: an unsafe command, no permission flags.
		{name: "unsafe-command/no-flags", command: unsafeShellCommand,
			want: copilotfixture.PermissionBlocked},
		// The positive control. Same rig, same command, one flag added — so a
		// blocked verdict above cannot be a broken fixture.
		{name: "unsafe-command/allow-all-tools", command: unsafeShellCommand, allow: true,
			want: copilotfixture.PermissionAllowed},
		// The scope limit, recorded so nobody re-derives it as a surprise.
		{name: "safe-command/no-flags", command: safeShellCommand,
			want: copilotfixture.PermissionAllowed},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			verdict, mock, _ := permissionRun(t, bashTurns(tc.command), true,
				copilotfixture.RunOptions{OmitAllowAllTools: !tc.allow})
			assert.Equal(t, tc.want, verdict.Outcome)
			require.NotEmpty(t, mock.Requests(),
				"trust was pre-granted, so the provider must have been reached")
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.ToolApproval, rows)
}

// TestCopilotPermissionURLGateUnderToolApproval measures URL access, and the
// result corrects the TCL-973 plan in the plan's own favour — which is why it
// is worth stating carefully rather than quietly.
//
// The plan assumed URL access is an axis independent of tool approval, and
// built its proposed default around closing the two separately. Half of that
// is right and half is not. With no permission flags, a shell command that
// reaches a URL blocks on a dialog that is unmistakably about the URL
// ("Copilot is attempting to access the following URL"), not about the
// command — so it IS a distinct prompt with its own chrome and its own
// decision. But `--allow-all-tools` closes it: the same launch with that one
// flag runs the command through.
//
// So for the shell path there is no second deadlock to close, and the plan's
// stated need for a URL deny alongside `--allow-all-tools` does not follow
// from this measurement.
//
// SCOPE, and it is the load-bearing limitation: this measures the SHELL path
// only. Copilot's web-fetch tool is the other URL consumer, and it is absent
// from the catalog entirely under COPILOT_OFFLINE=true, which is what makes
// this suite hermetic. Whether web_fetch is gated the same way is therefore
// not something these scenarios can answer, and it is recorded as unmeasured
// in permission_contract.json rather than generalized from the shell result.
func TestCopilotPermissionURLGateUnderToolApproval(t *testing.T) {
	requireSmoke(t)

	var rows []string
	for _, tc := range []struct {
		name  string
		allow bool
		want  copilotfixture.PermissionOutcome
	}{
		// The URL prompt exists and is its own dialog.
		{name: "no-flags", want: copilotfixture.PermissionBlocked},
		// And tool approval covers it. This row is the correction: it was
		// written expecting a block, and the real binary said otherwise.
		{name: "allow-all-tools", allow: true, want: copilotfixture.PermissionAllowed},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			verdict, mock, res := permissionRun(t, bashTurns(urlShellCommand), true,
				copilotfixture.RunOptions{OmitAllowAllTools: !tc.allow})
			assert.Equal(t, tc.want, verdict.Outcome)
			if tc.want == copilotfixture.PermissionBlocked {
				assert.True(t, res.Contains("attempting to access the following URL"),
					"the blocking dialog must be the URL one, which is what makes this a "+
						"distinct prompt rather than ordinary tool approval")
			}
			// No egress happened either way: the CLI asks before it dials, so
			// the whole measurement runs with no network at all.
			require.NotEmpty(t, mock.Requests())
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.URLGate, rows)
}

// TestCopilotPermissionAmbientAllowAllPromotes pins the environment variable
// that can silently change a launch tclaude recorded as something else.
//
// `--help` documents COPILOT_ALLOW_ALL as the env alias for
// `--allow-all-tools`. It is strictly stronger than that: the flag does not
// clear the folder-trust gate and the variable does. An operator with it
// exported turns every tclaude-spawned Copilot pane into an allow-all session
// with no record anywhere, which is why the spawner must UNSET it rather than
// set it falsy.
func TestCopilotPermissionAmbientAllowAllPromotes(t *testing.T) {
	requireSmoke(t)

	var rows []string
	for _, tc := range []struct {
		name     string
		value    string
		promotes bool
	}{
		{name: "true", value: "true", promotes: true},
		// Case-sensitive, literal equality — not a general truthy parse. Worth
		// pinning because a spawner that "disabled" it by writing TRUE or 1
		// would be relying on a parse that could widen in any release.
		{name: "TRUE", value: "TRUE"},
		{name: "one", value: "1"},
		{name: "false", value: "false"},
		{name: "empty", value: ""},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			mock := copilotfixture.NewMockProvider(t, bashTurns(unsafeShellCommand))
			dirs := copilotfixture.NewSandboxDirs(t)
			// NOT trusted: the trust gate is the detector here, because it is
			// the gate no flag can clear. Getting past it isolates the env
			// var's effect from anything argv could have done.
			res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
				RunOptions: copilotfixture.RunOptions{
					Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
					BaseURL:           mock.BaseURL(),
					Prompt:            "Use the tool as instructed.",
					OmitAllowAllTools: true,
					ExtraEnv:          []string{"COPILOT_ALLOW_ALL=" + tc.value},
				},
				Deadline:    permissionDeadline,
				SettledWhen: func() bool { return len(mock.Requests()) >= 2 },
			})

			promoted := len(mock.Requests()) >= 2
			t.Logf("permission verdict: ambient-promotion=%v (provider requests: %d)",
				promoted, len(mock.Requests()))
			assert.Equal(t, tc.promotes, promoted,
				"whether COPILOT_ALLOW_ALL=%q silently granted trust AND tool approval", tc.value)
			assert.Equal(t, !tc.promotes, res.Contains(copilotfixture.TrustPromptMarker))
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.AmbientAllowAll, rows)
}

// TestCopilotPermissionDenyToolGrammar pins which `--deny-tool` patterns 1.0.77
// PARSES, and it is the cheapest scenario in the suite: argument validation
// runs BEFORE the credential check, so the two outcomes are distinguishable
// with no provider, no PTY and no network at all.
//
// It exists because the TCL-973 plan's proposed daemon default contained
// `--deny-tool 'url()'`, which this measures as a hard parse error. A spawner
// emitting it would have killed every Copilot pane at launch with exit 1.
//
// PARSING IS NOT ENFORCEMENT, and for URL rules the two come apart — which is
// the trap this test alone cannot catch. `url(*)` parses cleanly here and then
// matches nothing at runtime, so a reader who took "accepted" for "works"
// would ship a blanket URL deny that denies nothing. Enforcement is a separate
// measurement; see the url-access entry in permission_contract.json, which
// carries all three columns (parses / enforced / verdict) precisely because
// this gap is what would have shipped a broken default.
func TestCopilotPermissionDenyToolGrammar(t *testing.T) {
	requireSmoke(t)

	var rows []string
	for _, tc := range []struct {
		pattern string
		valid   bool
	}{
		// The plan's proposed default. Empty parens are not the documented
		// "omitted argument means all" form; they do not parse.
		{pattern: "url()"},
		{pattern: "shell()"},
		{pattern: "write()"},
		{pattern: "*"},
		// Accepted spellings.
		{pattern: "url", valid: true},
		{pattern: "url(*)", valid: true},
		{pattern: "url(example.com)", valid: true},
		{pattern: "shell(*)", valid: true},
		{pattern: "write(/tmp)", valid: true},
	} {
		rows = append(rows, tc.pattern)
		t.Run(tc.pattern, func(t *testing.T) {
			dirs := copilotfixture.NewSandboxDirs(t)
			// No BaseURL: the run is expected to stop at the credential check,
			// which is precisely the marker that says the arguments parsed.
			result := copilotfixture.Run(t, copilotfixture.RunOptions{
				Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
				Prompt:    "This turn never runs.",
				ExtraArgs: []string{"--deny-tool", tc.pattern},
				Timeout:   permissionDeadline,
			})
			out := result.Stdout + result.Stderr
			rejected := contains(out, "Invalid --deny-tool value")
			reachedAuth := contains(out, "No authentication information found")
			t.Logf("permission verdict: pattern %q accepted=%v", tc.pattern, !rejected)
			assert.Equal(t, !tc.valid, rejected, "whether the pattern was rejected at parse")
			if tc.valid {
				assert.True(t, reachedAuth,
					"an accepted pattern must let the launch proceed past argument parsing")
			}
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.DenyToolGrammar, rows)
}

// TestCopilotPermissionNoAskUserRemovesTheTool measures the second deadlock
// source: the `ask_user` tool, through which the model can stop and ask a
// question that nobody is there to answer.
//
// The instrument is the tool catalog in the provider request body, which is
// the best one available anywhere in this matrix — it is a pure wire read, so
// it neither scrapes a TUI nor depends on the model choosing to call anything.
//
// It still needs a PTY, for a reason worth stating: `ask_user` is only offered
// when there is a terminal to ask through. Headless the catalog never contains
// it, so `--no-ask-user` is a no-op there and a headless scenario would report
// the flag as working while measuring nothing.
func TestCopilotPermissionNoAskUserRemovesTheTool(t *testing.T) {
	requireSmoke(t)

	catalog := func(t *testing.T, args ...string) []string {
		t.Helper()
		mock := copilotfixture.NewMockProvider(t,
			[]copilotfixture.Turn{{Text: "MOCK CATALOG PROBE"}})
		dirs := copilotfixture.NewSandboxDirs(t)
		copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
		copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
			RunOptions: copilotfixture.RunOptions{
				Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
				BaseURL:   mock.BaseURL(),
				Prompt:    "Answer in one word.",
				ExtraArgs: args,
			},
			Deadline: permissionDeadline,
			// One request is all this needs: the catalog is advertised on the
			// very first call, so there is nothing to gain by waiting for a
			// turn that has no tool call in it.
			SettledWhen: func() bool { return len(mock.Requests()) >= 1 },
		})
		requests := mock.Requests()
		require.NotEmpty(t, requests, "the catalog probe must reach the provider")
		assertCredentialFree(t, mock)
		return newSanitizer(dirs).Request(requests[0]).ToolNames
	}

	base := catalog(t)
	assert.Contains(t, base, "ask_user",
		"the default interactive catalog must offer ask_user; without it this scenario "+
			"would be asserting the removal of something that was never there")

	suppressed := catalog(t, "--no-ask-user")
	assert.NotContains(t, suppressed, "ask_user",
		"--no-ask-user must remove the tool from the advertised catalog")
	assert.Len(t, suppressed, len(base)-1,
		"--no-ask-user must remove ask_user and nothing else")
	t.Logf("permission verdict: ask_user removed by --no-ask-user (catalog %d -> %d tools)",
		len(base), len(suppressed))
}

// TestCopilotPermissionHeadlessIsNotEvidence records the confound that makes
// every other test in this file use a PTY, and makes it fail loudly if it ever
// stops being true.
//
// Without a terminal the CLI cannot draw a prompt, so it does not — and an
// unsafe command that BLOCKS on a pty runs to completion headlessly. A suite
// that measured permissions headlessly would report a confident green for a
// launch that deadlocks a real pane.
//
// It is deliberately an assertion rather than a comment: if a future release
// starts refusing headlessly instead of auto-allowing, the PTY discipline this
// file is built on has changed and somebody must re-read the reasoning.
func TestCopilotPermissionHeadlessIsNotEvidence(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, bashTurns(unsafeShellCommand))
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "Use the tool as instructed.",
		// The identical posture that blocks on a pty in
		// TestCopilotPermissionToolApprovalGate.
		OmitAllowAllTools: true,
		Timeout:           permissionDeadline,
	})

	t.Logf("permission verdict: headless-auto-allows=%v (provider requests: %d)",
		len(mock.Requests()) >= 2, len(mock.Requests()))
	assert.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assert.Len(t, mock.Requests(), 2,
		"headless, the same posture that blocks on a pty runs the tool to completion. "+
			"If this ever changes, the PTY-only discipline in this file needs re-justifying.")
	assertCredentialFree(t, mock)
}

// TestCopilotPermissionContractIsBackedByScenarios makes the contract table an
// assertion instead of prose.
//
// The brief asks for an honest PROVEN / DISPROVEN / UNVERIFIED table, and the
// failure mode of any such table is that it drifts into optimism: a status
// gets flipped to proven, or the scenario behind it is deleted, and the
// document keeps claiming the measurement stands. This test makes both of
// those a build failure.
func TestCopilotPermissionContractIsBackedByScenarios(t *testing.T) {
	// Deliberately NOT gated on requireSmoke: it reads committed files and
	// needs no binary, so it runs in plain `go test ./...` where everyone sees
	// it, rather than only in the CLI-provisioned job.
	contract := copilotfixture.LoadPermissionContract(t,
		filepath.Join("testdata", copilotfixture.PinnedCLIVersion, "permission_contract.json"))

	assert.Equal(t, copilotfixture.PinnedCLIVersion, contract.CLIVersion,
		"the contract describes one CLI release; bump it with the pin")

	// The eight measurements the Phase 0 brief asked for. Pinned as a set so a
	// measurement cannot be quietly dropped from the table.
	wantIDs := []string{
		"default-interactive-blocking",
		"no-ask-user",
		"url-access",
		"out-of-cwd-paths",
		"folder-trust",
		"resume-submits-prompt",
		"in-pane-allow-all-override",
		"ambient-allow-all-env",
	}
	var gotIDs []string
	for _, e := range contract.Entries {
		gotIDs = append(gotIDs, e.ID)
	}
	assert.ElementsMatch(t, wantIDs, gotIDs,
		"the contract must cover exactly the eight Phase 0 measurements")

	declared := map[string]bool{}
	for _, name := range copilotfixture.RegisteredScenarios() {
		declared[name] = true
	}

	for _, e := range contract.Entries {
		t.Run(e.ID, func(t *testing.T) {
			assert.NotEmpty(t, e.Claim, "every entry states the question it answers")
			assert.NotEmpty(t, e.Finding, "every entry states what was measured")
			switch e.Status {
			case copilotfixture.StatusProven, copilotfixture.StatusDisproven:
				require.NotEmpty(t, e.Scenarios,
					"a %s entry must name the scenarios that establish it", e.Status)
				for _, name := range e.Scenarios {
					assert.True(t, declared[name],
						"entry %q claims scenario %q, which no test declares. Either the "+
							"scenario was deleted and the status is now unbacked, or the "+
							"name is wrong. Declared: %v",
						e.ID, name, copilotfixture.RegisteredScenarios())
				}
				assert.Empty(t, e.Blocker, "a measured entry has no blocker")
			case copilotfixture.StatusUnverified:
				// The point of the whole file: unverified must be a claim
				// nobody can accidentally read as measured.
				assert.Empty(t, e.Scenarios,
					"an unverified entry must name NO scenario; naming one implies a "+
						"measurement that was not made")
				assert.NotEmpty(t, e.Blocker,
					"an unverified entry must say why it could not be measured, so the "+
						"gap is actionable rather than merely absent")
			default:
				t.Fatalf("unknown status %q", e.Status)
			}

			// Corroborating claims are the failure mode this guard was
			// extended for: an entry whose STATUS its scenarios genuinely
			// establish, carrying a finding that also asserted neighbouring
			// behaviors nothing here measured. The guard blessed the entry and
			// the entry blessed the extra claims by association. Keeping them
			// in their own field is what makes the boundary visible; requiring
			// each to say so in its own text is what keeps that boundary from
			// eroding as the file is edited.
			for i, claim := range e.Corroborating {
				assert.NotEmpty(t, strings.TrimSpace(claim),
					"corroborating claim %d is empty", i)
				assert.Contains(t, claim, "NOT measured",
					"corroborating claim %d must state plainly that this suite does not "+
						"measure it. A reader scanning a proven entry has to be able to "+
						"tell, at the claim itself, where the evidence stops", i)
			}
		})
	}
}
