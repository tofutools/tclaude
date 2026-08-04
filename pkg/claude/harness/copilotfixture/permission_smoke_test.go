package copilotfixture_test

import (
	"os"
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

// permissionDeadline is the OUTER bound on one pty scenario. Neither arm is
// meant to reach it.
//
// It used to be the blocked arm's stopwatch as well, at 12s, and that made it
// two things at once: the margin over the slowest startup, AND the price every
// blocked arm paid. Those pull in opposite directions, and the tension got
// sharper the moment the suite started running scenarios concurrently — CPU
// contention stretches startup, which wants a bigger number, while a dozen
// blocked arms each paying it wants a smaller one.
//
// blockedQuiet below split the roles, so this value is now free to be generous.
// A run that genuinely never settles still lands in ClassifyPermission's error
// arm, which fails the scenario with a message naming the deadline as the
// likely cause; it cannot silently downgrade a working launch into a "blocked"
// finding. What this bound buys is that a starved runner reaches that arm
// because the CLI misbehaved, not because the runner was busy.
const permissionDeadline = 30 * time.Second

// headlessPermissionDeadline bounds the non-PTY permission probes in this file.
//
// A headless Run either exits or fails; it has no PTY permission prompt whose
// blocked arm pays the deadline. In particular, the grammar probe's verdict
// comes from the launch parse error and whether the provider was reached. The
// other headless probes use the same non-blocking shape. They therefore do not
// share permissionDeadline's sizing trade-off, where a tighter bound keeps
// every genuinely blocked PTY row cheap. Sixty seconds gives ample margin over
// the observed loaded startup without adding cost to those blocked scenarios.
// Only use this for a headless row with no legitimate blocking arm: a
// non-completion must be a startup or hang failure, never the measurement.
const headlessPermissionDeadline = 60 * time.Second

// blockedQuiet is how long a pty scenario's transcript must stand still, with
// no follow-up request, before the run is called blocked and stopped.
//
// MEASURED, off the LOADED case rather than the comfortable one. Every pty run
// logs "PTY TIMING … first-output=… max-output-gap=…", and the two arms
// separate cleanly:
//
//   - A working turn's widest gap is 2.2s — and stays 2.2s under load. That is
//     PTYQuiescence closing on a turn that had already finished, so genuine
//     mid-turn gaps are smaller still.
//   - A blocked arm stops emitting at 3.9-5.5s, when the dialog finishes
//     rendering, and then never emits again however loaded the host is.
//
// The "stays 2.2s" is the part worth reading twice, because the first version
// of this constant was sized against numbers that said otherwise: gaps
// appearing to stretch to 8.4s under a two-core budget. They were not gaps.
// MaxOutputGap counted from launch, so a slow Node startup was being billed as
// a silence in a turn that had not begun — the measurement conflating exactly
// what the blocked verdict must not conflate. Once startup moved to its own
// field, the contention effect on working turns vanished: what actually
// stretches under load is time-to-first-byte (to 5.8s on two cores), and that
// is not a state any quiet window should be classifying.
//
// So 10s is ~4.5x the widest working gap measured on a deliberately starved
// box, and roughly double the point at which a blocked arm has gone silent for
// good. Being wrong in the tight direction is the failure mode to care about —
// an allowed arm cut short is recorded as blocked, a false finding in a suite
// whose whole subject is what a detached agent may do — so the margin is
// deliberately lopsided. Being wrong loose costs seconds, on arms that run
// concurrently with everything else anyway.
//
// The timing log stays in the merged code so the next tightening, or the next
// pinned-binary bump, argues from a real job's numbers rather than from this
// comment.
//
// EVERYTHING ABOVE IS TRUE ON LINUX AND ONLY ON LINUX. It was measured, it was
// measured correctly, and it has held for 108 Linux runs — working turns there
// still top out at 2.2s exactly as claimed. It was never re-checked on macOS,
// where the same measurement gives different numbers (1746 runs, 30 CI jobs):
//
//	                     allowed arms        blocked arms
//	linux   widest gap   2.2s max            >=10s      -> 86% caught here
//	macos   widest gap   5.8s max            4.1s min   -> 19% caught here
//
// Read the macOS row against itself. An allowed arm — one that executed the
// tool and posted the result — goes quiet for as long as 5.8s. A blocked arm
// can have a widest gap of only 4.1s. THE POPULATIONS OVERLAP, so no value of
// this constant separates them there. Below 5.8s it cuts working turns short
// and records them as blocked, which is the lopsided error the paragraphs above
// exist to avoid. Above it, blocked arms sail past and pay the full deadline.
// There is no correct value; on macOS silence is not evidence of blocking.
//
// It works on Linux only because the two populations are cleanly separated
// there, with nothing between 2.2s and 10s. That separation is the load-bearing
// property, not the 10s, and it is a property of the platform's render cadence
// rather than of Copilot's blocking behaviour.
//
// So the blocked verdict no longer rests on this constant: ClassifyPermission
// identifies a parked launch by the dialog on its screen, and this window is
// now only an early exit that saves wall clock where it happens to fire. Anyone
// inventing another silence-based detector should read the table first — the
// mistake this comment made was not guessing, it was measuring one platform and
// writing the result as if it were the behaviour.
const blockedQuiet = 10 * time.Second

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
	requireCompletionsWire(t, opts)
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
		RunOptions:   opts,
		Deadline:     permissionDeadline,
		BlockedAfter: blockedQuiet,
		// The follow-up request is conclusive: it can only exist if the tool
		// executed and produced a result to post back.
		SettledWhen: func() bool { return len(mock.Requests()) >= 2 },
	})

	requests := mock.Requests()
	total := len(requests)
	followUps := max(total-1, 0)
	// The tool results ride on the LAST request: that is the one carrying what
	// the CLI told the model the tool did, which is the only thing that
	// separates an execution from a silent refusal.
	var toolResults []string
	if total > 1 {
		toolResults = copilotfixture.ToolResults(requests[total-1])
	}
	verdict, err := copilotfixture.ClassifyPermission(
		total, followUps, !res.Exited, res.Quiesced, res.TranscriptText(), toolResults)
	require.NoError(t, err, "the launch could not be classified")

	// The line CI greps. Asserting the VALUE rather than merely that the test
	// passed is what keeps a scenario from silently starting to measure a
	// different arm than the one it is named for.
	t.Logf("permission verdict: %s (%s)", verdict.Outcome, verdict.Evidence)
	return verdict, mock, res
}

// requireCompletionsWire fails a scenario that would classify permissions on a
// wire whose tool-result shape this suite has never measured.
//
// It exists because of how the two pieces fail together. ToolResults reads the
// COMPLETIONS wire only, and returns nil for anything else — which is the right
// conservative behavior, since an unmeasured shape must not be guessed at. But
// nil tool results are also what ClassifyPermission sees when a follow-up
// carries none, so a scenario silently switched to the Responses wire would
// stop being a permission measurement and start being an undecidable error,
// with nothing pointing at the wire as the cause.
//
// Failing here instead names the actual problem at the actual call site. The
// empty value means WireCompletions (see RunOptions.Wire), so the default path
// passes without a scenario having to say anything.
func requireCompletionsWire(t *testing.T, opts copilotfixture.RunOptions) {
	t.Helper()
	if opts.Wire != "" && opts.Wire != copilotfixture.WireCompletions {
		t.Fatalf(
			"this scenario classifies permissions from tool results, and ToolResults reads "+
				"the completions wire only, so a %q run would return no tool results and "+
				"misreport the launch as unclassifiable. Measure the Responses wire's "+
				"tool-result shape and teach ToolResults about it before using it here",
			opts.Wire)
	}
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
	requireSmokeParallel(t)

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
		// `--yolo` is documented as an ALIAS of --allow-all, and this suite does
		// not accept documentation as measurement — the row above it is why:
		// COPILOT_ALLOW_ALL is documented as the env alias for
		// --allow-all-tools and was measured strictly stronger, clearing this
		// very gate. So the alias is measured rather than inferred, because
		// tclaude's `yolo` approval token renders THIS spelling and its mode
		// help tells the operator folder trust still blocks.
		{name: "yolo", args: []string{"--yolo"}},
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
			requireSmokeParallel(t)
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
				Deadline:     permissionDeadline,
				BlockedAfter: blockedQuiet,
				SettledWhen:  func() bool { return len(mock.Requests()) >= 2 },
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
	var rows []string
	for _, tc := range []struct {
		name    string
		command string
		allow   bool
		// args supplies the row's own permission flags, with the runner's
		// --allow-all-tools default still omitted — the shape a row needs when
		// the flag under test is NOT the default one.
		args []string
		want copilotfixture.PermissionOutcome
	}{
		// The deadlock claim itself: an unsafe command, no permission flags.
		{name: "unsafe-command/no-flags", command: unsafeShellCommand,
			want: copilotfixture.PermissionBlocked},
		// The positive control. Same rig, same command, one flag added — so a
		// blocked verdict above cannot be a broken fixture.
		{name: "unsafe-command/allow-all-tools", command: unsafeShellCommand, allow: true,
			want: copilotfixture.PermissionAllowed},
		// TCL-1010: the same unsafe command under the flag tclaude's `yolo`
		// approval token renders. It is measured rather than assumed from the
		// help text's alias claim, because the token's mode help tells an
		// operator yolo closes tool approval — and a `yolo` that rendered a
		// flag which did NOT close this gate would be a posture strictly worse
		// than the unattended default it sits beside in the dropdown. The
		// command choice carries the row: `safe-command/no-flags` below records
		// that Copilot auto-approves what it considers trivially safe, so only
		// an unsafe command can distinguish a closed gate from an absent one.
		{name: "unsafe-command/yolo", command: unsafeShellCommand,
			args: []string{"--yolo"}, want: copilotfixture.PermissionAllowed},
		// The scope limit, recorded so nobody re-derives it as a surprise.
		{name: "safe-command/no-flags", command: safeShellCommand,
			want: copilotfixture.PermissionAllowed},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmokeParallel(t)
			verdict, mock, res := permissionRun(t, bashTurns(tc.command), true,
				copilotfixture.RunOptions{
					OmitAllowAllTools: !tc.allow,
					ExtraArgs:         tc.args,
				})
			assert.Equal(t, tc.want, verdict.Outcome)
			if tc.want == copilotfixture.PermissionBlocked {
				// Without this the row would read "blocked" for a release that
				// silently dropped or errored the tool call instead of asking,
				// which is a different finding entirely. The two sibling
				// blocked scenarios pin their dialog the same way.
				assert.True(t, res.Contains(tc.command),
					"a blocked launch must show an approval dialog naming the command")
			}
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
			requireSmokeParallel(t)
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
			requireSmokeParallel(t)
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
				Deadline:     permissionDeadline,
				BlockedAfter: blockedQuiet,
				SettledWhen:  func() bool { return len(mock.Requests()) >= 2 },
			})

			promoted := len(mock.Requests()) >= 2
			t.Logf("permission verdict: ambient-promotion=%v (provider requests: %d)",
				promoted, len(mock.Requests()))
			assert.Equal(t, tc.promotes, promoted,
				"whether COPILOT_ALLOW_ALL=%q silently granted trust AND tool approval", tc.value)
			assert.Equal(t, !tc.promotes, res.Contains(copilotfixture.TrustPromptMarker))
			if promoted {
				// The row that reaches the provider is also the only row in the
				// matrix that injects an extra environment variable, so it is
				// where a credential arriving through the environment would be
				// least expected and least noticed. Every other scenario that
				// reaches the provider checks this.
				assertCredentialFree(t, mock)
			}
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.AmbientAllowAll, rows)
}

// TestCopilotPermissionDenyToolGrammar pins which `--deny-tool` patterns 1.0.77
// PARSES. It is the cheapest scenario in the suite -- no PTY, no network, ~1.5s
// a row -- because a parse error is visible from the launch alone.
//
// It does need the mock, though, and the reason is a false green this test
// produced before it had one. Run with no provider at all, on the theory that
// argument validation must come first, EVERY row reported "accepted",
// including `url()`: with COPILOT_OFFLINE=true and no base URL the CLI fails
// on "Offline mode requires a local model provider" before it ever inspects
// --deny-tool. The launch has to be otherwise viable for a parse error to be
// the thing that stops it. Each row now asserts two independent signals, the
// error text and whether the provider was reached, so a future failure that
// preempts parsing again shows up as a contradiction rather than a pass.
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
			requireSmokeParallel(t)
			// The mock is what makes the two outcomes separable. An earlier
			// version of this scenario ran with no provider at all, on the
			// theory that argument validation happens before anything else --
			// and every row came back "accepted", because COPILOT_OFFLINE=true
			// with no base URL fails on "Offline mode requires a local model
			// provider" BEFORE the CLI ever looks at --deny-tool. The run has
			// to be otherwise launchable for a parse error to be the thing
			// that stops it.
			mock := copilotfixture.NewMockProvider(t,
				[]copilotfixture.Turn{{Text: "MOCK DENY GRAMMAR PROBE"}})
			dirs := copilotfixture.NewSandboxDirs(t)
			copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
			result := copilotfixture.Run(t, copilotfixture.RunOptions{
				Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
				BaseURL:   mock.BaseURL(),
				Prompt:    "Answer in one word.",
				ExtraArgs: []string{"--deny-tool", tc.pattern},
				Timeout:   headlessPermissionDeadline,
			})

			out := result.Stdout + result.Stderr
			rejected := contains(out, "Invalid --deny-tool value")
			// Reaching the provider is the positive signal, and it is a
			// stronger one than any message: a pattern the CLI accepted cannot
			// have stopped the launch.
			reached := len(mock.Requests()) > 0

			t.Logf("permission verdict: pattern %q parses=%v (provider reached: %v)",
				tc.pattern, !rejected, reached)
			assert.Equal(t, !tc.valid, rejected, "whether the pattern was rejected at parse")
			assert.Equal(t, tc.valid, reached,
				"an accepted pattern must let the launch proceed to the provider; "+
					"a rejected one must stop before it")
			if tc.valid {
				assert.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
				assertCredentialFree(t, mock)
			} else {
				assert.NotEqual(t, 0, result.ExitCode,
					"a rejected pattern must fail the launch, which is what would have "+
						"killed every pane had the proposed default shipped")
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
	requireSmokeParallel(t)

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
			Deadline:     permissionDeadline,
			BlockedAfter: blockedQuiet,
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
	requireSmokeParallel(t)

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
		Timeout:           headlessPermissionDeadline,
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

	// The Phase 0 measurements, plus the web-fetch entry that closes the gap
	// Phase 0 recorded as structurally unmeasurable, the three TCL-992
	// assumption fixtures, and TCL-1010's yolo surface. Pinned as a set so a
	// measurement cannot be quietly dropped from the table.
	wantIDs := []string{
		"default-interactive-blocking",
		"no-ask-user",
		"url-access",
		"web-fetch-url-access",
		"out-of-cwd-paths",
		"path-dialog-under-allow-all-tools",
		"add-dir-write-grant",
		"flag-name-exactness",
		"folder-trust",
		"resume-submits-prompt",
		"in-pane-allow-all-override",
		"ambient-allow-all-env",
		"yolo-permission-surface",
		// TCL-1011's native-sandbox measurements. They share this table
		// because they share its discipline — a status is a claim about
		// scenarios that exist — even though their subject is Copilot's own
		// sandbox rather than its permission prompts.
		"native-sandbox-session-flags",
		"native-sandbox-generated-linux-policy",
		"native-sandbox-allow-bypass-demotes-enforcement",
	}
	var gotIDs []string
	for _, e := range contract.Entries {
		gotIDs = append(gotIDs, e.ID)
	}
	assert.ElementsMatch(t, wantIDs, gotIDs,
		"the contract must cover exactly the Phase 0 measurements plus the web-fetch "+
			"follow-up")

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

// stageOutsideEveryGrantedRoot creates a directory that is outside BOTH of
// Copilot's default path grants, and fails the test rather than degrading if
// it cannot.
//
// This helper is the whole reason the path measurement was left unverified in
// the first pass, and the trap is worth stating because the obvious shape has
// it. Copilot's default posture grants the cwd subtree PLUS the system temp
// dir. copilotfixture.NewSandboxDirs roots everything under t.TempDir, which
// IS the system temp tree — so a sibling of WorkDir, the natural choice, is
// outside the cwd grant and squarely inside the temp one. A scenario built
// that way measures an ALLOWED path and reports it as a denial.
//
// Two defences, because either alone can be defeated. The child's temp dir is
// PINNED to a directory this test chose, so "the system temp dir" is an
// experiment input rather than a property of whatever host is running; and the
// staged path is asserted to be outside every granted root, so a future change
// in any of them fails loudly instead of quietly measuring the wrong thing.
func stageOutsideEveryGrantedRoot(t *testing.T, dirs copilotfixture.Dirs, childTemp string) string {
	t.Helper()
	// Based beside the package rather than under any temp root: every temp
	// candidate is exactly what has to be excluded here.
	base, err := os.MkdirTemp(".", "copilotfixture-outside-")
	if err != nil {
		t.Fatalf("staging a directory outside every granted root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	abs, err := filepath.Abs(base)
	if err != nil {
		t.Fatalf("resolving the staged directory: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}

	// Checked against the UNRESOLVED spellings too: on macOS TMPDIR is
	// /var/folders/… while the resolved form is /private/var/folders/…, and a
	// containment check that saw only one of them would pass while the path sat
	// inside the other.
	for _, granted := range []struct{ name, path string }{
		{"the working directory", dirs.WorkDir},
		{"the unresolved working directory", dirs.UnresolvedWorkDir},
		{"the pinned child temp dir", childTemp},
		{"the fixture root", dirs.Root},
		{"the unresolved fixture root", dirs.UnresolvedRoot},
		{"the host temp dir", os.TempDir()},
	} {
		if granted.path == "" {
			continue
		}
		if under(abs, granted.path) {
			t.Fatalf(
				"the staged path %s is inside %s (%s), so this scenario would measure an "+
					"ALLOWED path and report it as a denial. Refusing to run rather than "+
					"produce that result", abs, granted.name, granted.path)
		}
	}
	return abs
}

// under reports whether path is at or beneath root, comparing whole segments so
// a sibling like /tmp/foo-other is not read as inside /tmp/foo.
//
// Both sides are symlink-resolved first, and on macOS that is the difference
// between a working guard and a decorative one: os.TempDir() there reports
// /var/folders/… while the same directory resolves to /private/var/folders/…,
// so comparing a resolved path against an unresolved root would answer "not
// contained" for a path sitting squarely inside it. The guard would then pass
// on exactly the platform whose path spellings it exists to defend against.
func under(path, root string) bool {
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestCopilotPermissionPathGrants measures Copilot's default path posture and
// the two flags that move it.
//
// Paths are a genuinely SEPARATE deadlock source, with their own dialog
// ("Allow directory access"), distinct from both tool approval and folder
// trust. On a pane, a path outside every granted root blocks — it does not
// auto-deny.
//
// That last point is a correction the PTY discipline earned, and it is the
// third time in this ticket that a headless measurement pointed the wrong way.
// Measured headlessly, an out-of-grant path AUTO-DENIES: the CLI answers the
// model "Permission denied and could not request permission from user" and the
// turn continues. Read from there, paths look like the one prompt surface a
// detached agent need not worry about, since it resolves itself. On a real
// terminal the same launch stops and asks, and waits forever. The auto-deny is
// a no-TTY fallback, not the behaviour of the launches tclaude performs.
//
// Note this is the opposite asymmetry from tool approval, where the no-TTY
// fallback auto-ALLOWS. Two different fallbacks in the same binary, neither of
// which describes a real pane — which is precisely why nothing in this file
// measures permissions without a terminal.
//
// The rows that DO allow are still classified through the tool result rather
// than the request count, because a denial produces a follow-up request just
// as an execution does. See ClassifyPermission.
func TestCopilotPermissionPathGrants(t *testing.T) {
	var rows []string
	for _, tc := range []struct {
		name string
		args func(outside, childTemp string) []string
		// inTemp targets the child's pinned temp dir instead of the staged
		// outside-everything directory.
		inTemp bool
		want   copilotfixture.PermissionOutcome
	}{
		// The default posture: outside every grant, nothing added.
		{name: "outside-all/no-path-flags", want: copilotfixture.PermissionBlocked},
		// --add-dir is the flag TCL-973 intends to feed from the sandbox
		// profile's granted dirs, so it needs to work precisely.
		{name: "outside-all/add-dir", want: copilotfixture.PermissionAllowed,
			args: func(outside, _ string) []string { return []string{"--add-dir", outside} }},
		// Measured to characterize the mechanism, never proposed as a default:
		// Copilot's built-in edits are not OS-confined, so the path check is
		// the only file-write boundary a non-tclaude-layer launch has.
		{name: "outside-all/allow-all-paths", want: copilotfixture.PermissionAllowed,
			args: func(string, string) []string { return []string{"--allow-all-paths"} }},
		// The load-bearing checkpoint: the same out-of-grant path, now with
		// --allow-all-tools explicitly supplied. The directory dialog is a
		// separate permission source and remains blocking.
		{name: "outside-all/allow-all-tools", want: copilotfixture.PermissionBlocked,
			args: func(string, string) []string { return []string{"--allow-all-tools"} }},
		// TCL-1010's evidence: the flag tclaude's `yolo` approval token
		// renders, measured on the axis that token exists to close. The row
		// above is its control — the SAME out-of-grant read, blocked, with the
		// unattended default's flag instead — so "yolo closes the directory
		// dialog" is a contrast between two launches rather than a claim about
		// one. Measured under its own spelling rather than inherited from the
		// --allow-all-paths row, since a token that renders --yolo may not
		// legitimately cite a measurement of a different flag.
		{name: "outside-all/yolo", want: copilotfixture.PermissionAllowed,
			args: func(string, string) []string { return []string{"--yolo"} }},
		// The automatic temp grant, and the flag that removes it.
		{name: "in-temp/default", inTemp: true, want: copilotfixture.PermissionAllowed},
		{name: "in-temp/disallow-temp-dir", inTemp: true, want: copilotfixture.PermissionBlocked,
			args: func(string, string) []string { return []string{"--disallow-temp-dir"} }},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmokeParallel(t)
			dirs := copilotfixture.NewSandboxDirs(t)
			// Pinned, so "the system temp dir" is something this scenario
			// chose rather than a property of the host running it.
			childTemp := filepath.Join(dirs.Root, "child-temp")
			require.NoError(t, os.MkdirAll(childTemp, 0o755))
			outside := stageOutsideEveryGrantedRoot(t, dirs, childTemp)

			targetDir := outside
			if tc.inTemp {
				targetDir = childTemp
			}
			target := filepath.Join(targetDir, "copilotfixture-path-probe.txt")
			require.NoError(t, os.WriteFile(target, []byte("copilotfixture path probe\n"), 0o600))

			var args []string
			if tc.args != nil {
				args = tc.args(outside, childTemp)
			}
			verdict, mock, res := permissionRun(t,
				// `cat` of a staged file: the permission layer is what is under
				// test, so the command itself must be as boring as possible.
				bashTurns("cat "+target), true,
				copilotfixture.RunOptions{
					ExtraArgs: args,
					// This table measures each path posture with only the
					// path flags named by its row. The runner's normal
					// --allow-all-tools default is deliberately omitted here;
					// the dedicated row above supplies it explicitly.
					OmitAllowAllTools: true,
					// TMPDIR is what Node's os.tmpdir() reads, which is how the
					// CLI resolves the temp grant.
					ExtraEnv: []string{"TMPDIR=" + childTemp},
				})
			assert.Equal(t, tc.want, verdict.Outcome)
			if tc.want == copilotfixture.PermissionBlocked {
				// Its own dialog, distinct from tool approval and from folder
				// trust. Pinned so a row cannot read "blocked" because the tool
				// call failed for some unrelated reason.
				assert.True(t, res.Contains(copilotfixture.PathPromptMarker),
					"a path-blocked launch must show the directory-access dialog")
				assert.True(t, res.Contains(target),
					"the dialog must name the path it is asking about")
				require.Len(t, mock.Requests(), 1,
					"a path prompt must have reached the provider once, but must not post a tool result")
				assert.False(t, res.Exited, "a path prompt parks the interactive process")
				assert.True(t, res.Quiesced, "a path prompt leaves settled output")
			}
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.PathGrants, rows)
}

// TestCopilotPermissionAddDirWrites measures the read/write scope of Copilot's
// single directory grant. The existing path matrix proved --add-dir against a
// read; this uses a shell redirection against a fresh file so a passing row
// proves the grant permits writes rather than merely allowing the directory to
// be inspected.
func TestCopilotPermissionAddDirWrites(t *testing.T) {
	var rows []string
	for _, tc := range []struct {
		name string
		args func(outside string) []string
		want copilotfixture.PermissionOutcome
	}{
		{name: "outside-all/no-path-flags", want: copilotfixture.PermissionBlocked,
			args: func(string) []string { return []string{"--allow-all-tools"} }},
		{name: "outside-all/add-dir", want: copilotfixture.PermissionAllowed,
			args: func(outside string) []string { return []string{"--allow-all-tools", "--add-dir", outside} }},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmokeParallel(t)
			dirs := copilotfixture.NewSandboxDirs(t)
			childTemp := filepath.Join(dirs.Root, "child-temp")
			require.NoError(t, os.MkdirAll(childTemp, 0o755))
			outside := stageOutsideEveryGrantedRoot(t, dirs, childTemp)
			target := filepath.Join(outside, "copilotfixture-write-probe.txt")

			verdict, mock, res := permissionRun(t,
				bashTurns("echo copilotfixture-write-probe > "+target), true,
				copilotfixture.RunOptions{
					ExtraArgs:         tc.args(outside),
					OmitAllowAllTools: true,
					ExtraEnv:          []string{"TMPDIR=" + childTemp},
				})
			assert.Equal(t, tc.want, verdict.Outcome)
			if tc.want == copilotfixture.PermissionBlocked {
				assert.True(t, res.Contains(copilotfixture.PathPromptMarker),
					"a write outside every grant must show the directory-access dialog")
				assert.True(t, res.Contains(target),
					"the write prompt must name the target path")
				require.Len(t, mock.Requests(), 1,
					"the blocked write must reach the provider once without a tool-result follow-up")
				assert.NoFileExists(t, target,
					"a blocked write must not create the target as a side effect")
			} else {
				data, err := os.ReadFile(target)
				require.NoError(t, err,
					"--add-dir must let the shell create and then read the write probe")
				assert.Equal(t, "copilotfixture-write-probe\n", string(data),
					"the allowed row must positively establish the written content")
			}
			assertCredentialFree(t, mock)
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.AddDirWrites, rows)
}

// TestCopilotPermissionFlagNameExactness probes the pinned parser for the
// spellings the pass-through audit deliberately does not normalize. Each row
// is launched on a PTY so acceptance cannot be inferred from a noninteractive
// fallback; a rejected spelling must exit with an unknown-option diagnostic
// before it reaches the provider.
func TestCopilotPermissionFlagNameExactness(t *testing.T) {
	var rows []string
	for _, tc := range []struct {
		name string
		arg  string
	}{
		{name: "prefix-abbreviation", arg: "--allow-all-tool"},
		{name: "camel-case", arg: "--allowAllTools"},
		{name: "no-negation", arg: "--no-allow-all-tools"},
	} {
		rows = append(rows, tc.name)
		t.Run(tc.name, func(t *testing.T) {
			requireSmokeParallel(t)
			mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK PARSER PROBE"}})
			dirs := copilotfixture.NewSandboxDirs(t)
			copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
			res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
				RunOptions: copilotfixture.RunOptions{
					Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
					BaseURL: mock.BaseURL(), Prompt: "Answer with the provider text.",
					OmitAllowAllTools: true, ExtraArgs: []string{tc.arg},
				},
				Deadline:     permissionDeadline,
				BlockedAfter: blockedQuiet,
				SettledWhen:  func() bool { return len(mock.Requests()) >= 1 },
			})
			t.Logf("permission verdict: parser rejected %q (exited=%v exit_code=%d provider_requests=%d)",
				tc.arg, res.Exited, res.ExitCode, len(mock.Requests()))
			assert.True(t, res.Exited, "an unknown option must terminate the launch")
			assert.NotEqual(t, 0, res.ExitCode, "an unknown option must fail the launch")
			assert.True(t, res.Contains("unknown option"),
				"the PTY diagnostic must positively identify the spelling as unknown")
			assert.Empty(t, mock.Requests(),
				"a parser rejection must occur before the provider is contacted")
		})
	}
	assertScenarioRowsMatchRegistry(t, permissionScenarios.FlagNameExactness, rows)
}

// TestCopilotPermissionResumeSubmitsPrompt closes the standing UNVERIFIED in
// copilot_spawner.go: whether `-i` on a `--resume` launch submits into the
// RESUMED conversation or silently starts somewhere else.
//
// Every relaunch briefing tclaude sends depends on the answer. If the prompt
// did not land, a reincarnated or resumed agent would come back with no
// instructions and no error — the failure would be invisible.
//
// The instrument is the message roles in the provider request, which is exact:
// a fresh conversation sends [system user], while one that resumed and then
// received a new prompt sends [system user assistant user] — the earlier
// exchange followed by the new turn.
func TestCopilotPermissionResumeSubmitsPrompt(t *testing.T) {
	requireSmokeParallel(t)

	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
	sessionID := "5f7c2a91-3d84-4e6b-9c15-8a2f0b7d4e63"

	// Phase 1 seeds the conversation headlessly. The permission posture is not
	// under test here, so the cheap non-PTY path is the right one; what matters
	// is that a real conversation exists under the pinned id.
	seed := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL:   mockFor(t, "MOCK FIRST ANSWER").BaseURL(),
		Prompt:    "First question.",
		SessionID: sessionID,
		Timeout:   headlessPermissionDeadline,
	})
	require.Equal(t, 0, seed.ExitCode, "stderr: %s", seed.Stderr)
	require.DirExists(t, filepath.Join(dirs.Home, "session-state", sessionID),
		"the seed run must have created the conversation under the pinned id")

	// Phase 2 resumes it interactively, which is the shape tclaude relaunches
	// with. A PTY because that is the launch under discussion, not because the
	// assertion needs one.
	resumed := copilotfixture.NewMockProvider(t,
		[]copilotfixture.Turn{{Text: "MOCK SECOND ANSWER"}})
	copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: copilotfixture.RunOptions{
			Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
			BaseURL:  resumed.BaseURL(),
			Prompt:   "Second question.",
			ResumeID: sessionID,
		},
		Deadline:     permissionDeadline,
		BlockedAfter: blockedQuiet,
		SettledWhen:  func() bool { return len(resumed.Requests()) >= 1 },
	})

	requests := resumed.Requests()
	require.NotEmpty(t, requests, "the resumed launch must reach the provider")
	roles := newSanitizer(dirs).Request(requests[0]).MessageRoles
	assert.Equal(t, []string{"system", "user", "assistant", "user"}, roles,
		"the resumed request must carry the earlier exchange AND the new prompt. "+
			"[system user] would mean the prompt started a FRESH conversation and every "+
			"relaunch briefing is silently lost")
	t.Logf("permission verdict: resume submits the prompt into the resumed conversation "+
		"(roles: %v)", roles)

	// The conversation kept its identity rather than forking into a new one.
	assert.DirExists(t, filepath.Join(dirs.Home, "session-state", sessionID))
	assertCredentialFree(t, resumed)
}

// mockFor is a one-turn provider, for phases whose traffic is setup rather
// than evidence.
func mockFor(t *testing.T, text string) *copilotfixture.MockProvider {
	t.Helper()
	return copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: text}})
}

// TestCopilotPermissionInPaneAllowAllCannotOverrideDeny answers the question
// that decides how tclaude is allowed to DESCRIBE its permission flags: is a
// launch-time deny a durable boundary, or merely a starting posture that
// anything typed into the pane can widen?
//
// Copilot has several in-pane mutators (/allow-all, /add-dir,
// /reset-allowed-tools, /settings), and unlike the sandbox posture there is no
// way to re-verify the permission posture from outside — it lives inside the
// pane. So if /allow-all could lift a launch-time deny, tclaude could not
// honestly advertise any permission boundary at all and only the OS wall would
// remain.
//
// The measured answer is the good one: the deny survives.
//
// The chosen command matters. `echo` is auto-approved when no rule mentions
// it, so a refusal here can ONLY come from the deny rule — there is no
// risk-classification confound to explain it away.
func TestCopilotPermissionInPaneAllowAllCannotOverrideDeny(t *testing.T) {
	// Sequential. The only scenario in the suite that TYPES into a live TUI at
	// a settled prompt and then types again, so it is the only one whose
	// procedure depends on the CLI keeping up rather than on what the CLI
	// eventually decides. RunPTY now refuses to type into a screen that has
	// drawn nothing, which fixed the way this failed under contention, but a
	// three-phase conversation with a real terminal has no business racing
	// three other CLIs for two cores when the whole scenario costs twelve
	// seconds to run alone.
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		// The launch prompt, answered with plain text so the session settles at
		// its input prompt with no tool call yet.
		{Text: "MOCK READY"},
		// Answers the tool request typed after /allow-all.
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_inpane",
			Name: "bash",
			Args: `{"command":"` + safeShellCommand + `","description":"in-pane probe"}`,
		}},
		{Text: "MOCK DONE"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)

	inPaneOpts := copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "Say you are ready.",
		// Paired deliberately: the deny must beat a blanket allow, not merely
		// fill a gap the allow left open.
		ExtraArgs: []string{"--deny-tool", "shell(echo)"},
	}
	// This scenario reads tool results directly rather than through
	// permissionRun, so it needs the same wire guard.
	requireCompletionsWire(t, inPaneOpts)

	res := copilotfixture.RunPTY(t, copilotfixture.PTYOptions{
		RunOptions: inPaneOpts,
		// Typed into a settled screen, in order: widen the posture, then ask for
		// the denied tool.
		Input: []string{"/allow-all", "Now use the bash tool."},
		// Three turns and two waits for a settled screen, so an absolute value
		// rather than a multiple of permissionDeadline.
		//
		// The most generous bound in the file, and measured rather than picked:
		// this scenario waits for quiescence TWICE before it has even asked its
		// question, so it accumulates the concurrency tax three times over
		// where a single-turn scenario pays it once. At 45s it failed on a
		// two-core box under load — not blocked, just starved — which is the
		// error arm doing its job and the reason the number moved.
		//
		// No BlockedAfter either. This is the one scenario that WANTS the
		// screen to go quiet: it types into each settled prompt in turn, so
		// silence is a step in the procedure rather than a verdict.
		Deadline:    2 * time.Minute,
		SettledWhen: func() bool { return len(mock.Requests()) >= 3 },
	})

	// The pane really did accept the widening command; without this the test
	// could pass because /allow-all was never delivered.
	assert.True(t, res.Contains("All permissions are now enabled"),
		"the in-pane widening command must have been accepted, or this scenario "+
			"proves nothing about whether it can override a deny")

	requests := mock.Requests()
	require.GreaterOrEqual(t, len(requests), 3,
		"the typed prompt must have produced a tool call and its result")
	marker := copilotfixture.DenialMarker(
		copilotfixture.ToolResults(requests[len(requests)-1]))
	assert.NotEmpty(t, marker,
		"the launch-time deny must still refuse the tool AFTER /allow-all widened the "+
			"posture; if this ever passes, launch permission flags are startup posture "+
			"only and docs must stop describing them as a boundary")
	t.Logf("permission verdict: launch-time deny survives in-pane /allow-all (%q)", marker)
	assertCredentialFree(t, mock)
}
