package copilotfixture_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// This file is the fixture-backed evidence behind TCL-977's answer that
// GitHub Copilot CLI's own "command sandboxing" does not satisfy tclaude's
// SupportsBuiltinOSSandbox contract. The reasoning lives beside the descriptor
// in harness/copilot_sandbox_native.go; what lives here is the measurement,
// because a capability refusal argued only from a vendor's documentation is a
// reading, not a finding.
//
// Every scenario passes --allow-all-paths deliberately. Copilot has TWO
// independent filters over a file path: the ordinary permission layer, which
// restricts tools to the working directory, and the sandbox policy. Leaving the
// permission layer on would block the out-of-policy paths for the wrong reason
// and every scenario below would pass while measuring nothing. With it off, a
// refusal can only have come from the sandbox — and
// TestCopilotNativeSandboxDisabledAppliesNoPolicy is the control that proves
// the refusals are not simply the CLI's default behavior.

// nativeSandboxBlockedMarker is the refusal text Copilot renders as the tool
// RESULT when its sandbox stopped an action.
//
// It says only "the sandbox refused this", by SOME mechanism. It deliberately
// does NOT distinguish an in-process policy check from an OS denial, and an
// earlier version of this comment claimed it did. That was wrong: the shipped
// runtime carries a denial-fingerprint classifier that maps errno-shaped
// failures (EACCES, EPERM, "operation not permitted", …) onto this same
// sentence, so an OS refusal while the sandbox is active reads identically.
//
// The only sound discriminator this suite has for "where does enforcement
// live" is a host where the OS backend cannot start at all — see the
// backend-down arm of TestCopilotNativeSandboxBuiltinEditsAreInProcessOnly,
// which is why CI provisions that host category deliberately rather than
// relying on this string.
const nativeSandboxBlockedMarker = "sandbox is active and blocked"

// nativeSandboxDirs builds a scenario's directory set: the granted workspace,
// an explicitly denied directory inside HOME, and a directory under the system
// temp root.
//
// SystemTemp is named for what it IS rather than for the role an earlier
// version of this suite assigned it ("outside every granted path"). CI refuted
// that on both platforms: `copilot help sandbox` documents the system temp
// directory as part of the DEFAULT granted surface, and a hermetic fixture
// necessarily builds everything under it. See
// TestCopilotNativeSandboxShellBasePolicySurface.
type nativeSandboxDirs struct {
	copilotfixture.Dirs
	Denied     string
	SystemTemp string
}

func newNativeSandboxDirs(t *testing.T) nativeSandboxDirs {
	t.Helper()
	dirs := copilotfixture.NewSandboxDirs(t)
	denied := filepath.Join(dirs.Root, "denied")
	require.NoError(t, os.MkdirAll(denied, 0o755))
	return nativeSandboxDirs{Dirs: dirs, Denied: denied, SystemTemp: t.TempDir()}
}

// classifyBackend derives the backend verdict from the run under test and logs
// it in the form CI greps. A run that cannot be classified fails the test
// rather than silently choosing an arm.
func classifyBackend(
	t *testing.T, grantedWriteLanded bool, shellResult string,
) copilotfixture.NativeSandboxBackendVerdict {
	t.Helper()
	verdict, err := copilotfixture.ClassifyNativeSandboxBackend(
		grantedWriteLanded, shellResult)
	require.NoError(t, err)
	t.Logf("OS sandbox backend available: %v (%s)", verdict.Up, verdict.Evidence)
	return verdict
}

// enableNativeSandbox writes the posture every scenario shares: the workspace
// granted, one directory denied outright, outbound network closed and loopback
// kept (the mock provider is reached by the CLI process, not by a sandboxed
// child, but a scenario should not depend on that being true forever).
func enableNativeSandbox(t *testing.T, dirs nativeSandboxDirs, enabled bool) {
	t.Helper()
	copilotfixture.WriteNativeSandboxSettings(t, dirs.Dirs, copilotfixture.NativeSandboxSettings{
		Enabled:                    enabled,
		AddCurrentWorkingDirectory: true,
		// No per-command escape hatch: a bypass would make every negative
		// result ambiguous about whether the policy or the agent decided it.
		AllowBypass: false,
		UserPolicy: copilotfixture.NativeSandboxUserPolicy{
			Filesystem: copilotfixture.NativeSandboxFilesystem{
				DeniedPaths: []string{dirs.Denied},
			},
			Network: copilotfixture.NativeSandboxNetwork{
				AllowOutbound: false, AllowLocalNetwork: true,
			},
		},
	})
}

func createTurn(id, path string) copilotfixture.Turn {
	args, err := json.Marshal(map[string]any{"path": path, "file_text": "tcl977\n"})
	if err != nil {
		panic(err)
	}
	return copilotfixture.Turn{ToolCall: &copilotfixture.ToolCall{
		ID: id, Name: "create", Args: string(args),
	}}
}

func bashTurn(id, command string) copilotfixture.Turn {
	args, err := json.Marshal(map[string]any{
		"command": command, "description": "tcl977 probe",
	})
	if err != nil {
		panic(err)
	}
	return copilotfixture.Turn{ToolCall: &copilotfixture.ToolCall{
		ID: id, Name: "bash", Args: string(args),
	}}
}

// toolResults returns each tool call's result text, keyed by tool_call_id. The
// CLI replays the whole message history on every follow-up request, so the same
// result appears many times; first occurrence wins.
func toolResults(requests []copilotfixture.RecordedRequest) map[string]string {
	out := map[string]string{}
	for _, req := range requests {
		messages, _ := req.Body["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] != "tool" {
				continue
			}
			id := fmt.Sprint(message["tool_call_id"])
			if _, seen := out[id]; !seen {
				out[id] = fmt.Sprint(message["content"])
			}
		}
	}
	return out
}

// runNativeSandboxScenario runs a scenario with the standard --allow-all-paths
// posture; see the file comment for why that flag is on by default here.
func runNativeSandboxScenario(
	t *testing.T, dirs nativeSandboxDirs, turns []copilotfixture.Turn, extra ...string,
) map[string]string {
	t.Helper()
	return runNativeSandboxScenarioWithArgs(t, dirs, turns,
		append([]string{"--allow-all-paths"}, extra...)...)
}

// runNativeSandboxScenarioWithArgs passes the launch arguments verbatim, so a
// scenario can measure what --allow-all-paths itself changes rather than having
// it baked in.
func runNativeSandboxScenarioWithArgs(
	t *testing.T, dirs nativeSandboxDirs, turns []copilotfixture.Turn, args ...string,
) map[string]string {
	t.Helper()
	results, _ := runNativeSandboxScenarioWithRequests(t, dirs, turns, args...)
	return results
}

// runNativeSandboxScenarioWithRequests additionally returns the raw provider
// requests, for a scenario whose evidence is what the CLI RECEIVED rather than
// only what it did — the traversal case, whose whole point is that an
// un-normalized argument reached the binary.
//
// Launch arguments are passed VERBATIM — no default is substituted for an empty
// list, because "no arguments" is a posture the base-surface scenario measures
// deliberately and a helpful default would silently turn it into a different
// measurement.
func runNativeSandboxScenarioWithRequests(
	t *testing.T, dirs nativeSandboxDirs, turns []copilotfixture.Turn, args ...string,
) (map[string]string, []copilotfixture.RecordedRequest) {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t, append(turns, copilotfixture.Turn{Text: "MOCK DONE"}))
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir, BaseURL: mock.BaseURL(),
		Prompt: "Run the tools the provider asks for.", ExtraArgs: args,
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	requests := mock.Requests()
	return toolResults(requests), requests
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// TestCopilotNativeSandboxBuiltinEditsAreInProcessOnly is the measurement the
// whole capability decision rests on.
//
// GitHub states it in one line — "Built-in file edits aren't OS-sandboxed, but
// still follow the same policy on a best-effort basis" — and a capability this
// consequential should not be decided from a sentence. What is measured here is
// that the built-in `create` tool is governed by a check inside the CLI rather
// than by the OS: its refusal arrives as the CLI's own tool-result text, and on
// a host where the OS backend cannot start at all, a create into the granted
// workspace still writes its file while every shell command fails.
//
// That last arm is the decisive one, and it is why the scenario measures the
// host instead of demanding a particular one. A file write that succeeds while
// the sandbox cannot even be entered is a write that never entered it.
func TestCopilotNativeSandboxBuiltinEditsAreInProcessOnly(t *testing.T) {
	requireSmoke(t)

	dirs := newNativeSandboxDirs(t)
	enableNativeSandbox(t, dirs, true)

	inWorkspace := filepath.Join(dirs.WorkDir, "in_workspace")
	shellMarker := filepath.Join(dirs.WorkDir, "shell_marker")
	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		createTurn("edit_workspace", inWorkspace),
		createTurn("edit_home", filepath.Join(dirs.Root, "in_home")),
		createTurn("edit_denied", filepath.Join(dirs.Denied, "in_denied")),
		createTurn("edit_system_temp", filepath.Join(dirs.SystemTemp, "in_system_temp")),
		bashTurn("shell_workspace", "touch "+shellMarker),
	})
	backend := classifyBackend(t, exists(t, shellMarker), results["shell_workspace"])

	assert.True(t, exists(t, inWorkspace),
		"a built-in edit inside the granted workspace must succeed; without it the "+
			"refusals below would only mean the policy blocks everything")
	for id, path := range map[string]string{
		"edit_home":        filepath.Join(dirs.Root, "in_home"),
		"edit_denied":      filepath.Join(dirs.Denied, "in_denied"),
		"edit_system_temp": filepath.Join(dirs.SystemTemp, "in_system_temp"),
	} {
		assert.NotContains(t, results[id], "Created file",
			"%s must be refused by the sandbox policy", id)
		assert.Contains(t, results[id], nativeSandboxBlockedMarker,
			"%s must be refused by the sandbox; note this marker says only THAT the "+
				"sandbox refused, not by which mechanism — see the constant", id)
		assert.False(t, exists(t, path), "%s must not have been written", id)
	}

	// Note what the edits above establish on their own: NOT where enforcement
	// lives. The refusal text is the same whichever layer produced it. The
	// mechanism question is answered only by the backend-down arm below, which
	// is why CI provisions a host in that category on purpose instead of
	// treating it as an accident of the runner.
	if backend.Up {
		t.Log("backend up: this run measures that the policy is enforced, but not " +
			"WHERE. The in-process finding comes from the backend-down arm.")
		return
	}
	// The decisive arm. The OS sandbox could not be entered at all, so no shell
	// command ran — and yet the built-in edit above wrote its file into the
	// workspace. The two halves of Copilot's boundary are therefore enforced by
	// two different things, and only one of them is the OS.
	assert.False(t, exists(t, shellMarker),
		"with no usable OS backend a shell command must not run at all")
	assert.True(t, exists(t, inWorkspace),
		"the built-in edit wrote its file on a host where the OS sandbox could "+
			"not start; the edit path never enters the sandbox")
}

// TestCopilotNativeSandboxBuiltinEditPolicyResolvesSymlinksAndTraversal is the
// honest counterweight to the test above, and it is recorded because it came
// out in Copilot's favor.
//
// The obvious failure mode of an in-process path check is that it compares
// strings: a symlink planted inside the granted workspace, or a `..` segment,
// would then walk straight out of the policy. Measured against 1.0.77, neither
// does. The capability refusal therefore rests on WHERE enforcement lives, not
// on a defect in it — which is a materially different claim, and the one this
// suite is willing to make.
func TestCopilotNativeSandboxBuiltinEditPolicyResolvesSymlinksAndTraversal(t *testing.T) {
	requireSmoke(t)

	dirs := newNativeSandboxDirs(t)
	enableNativeSandbox(t, dirs, true)
	require.NoError(t, os.Symlink(dirs.SystemTemp, filepath.Join(dirs.WorkDir, "escape")))
	require.NoError(t, os.Symlink(dirs.Denied, filepath.Join(dirs.WorkDir, "escape_denied")))

	// Concatenated, NOT filepath.Join'd. Join calls Clean, which resolves the
	// `..` before the argument is ever serialized — so a Join-built "traversal"
	// path arrives at the CLI already normalized and the scenario measures a
	// plain out-of-policy write instead of a traversal. The literal segment has
	// to survive into the tool argument for this test to mean anything, which is
	// what the request assertion below verifies rather than assumes.
	traversalArgument := dirs.WorkDir + "/../via_traversal"
	require.Contains(t, traversalArgument, "/../",
		"the traversal argument must carry a literal `..` segment")

	results, requests := runNativeSandboxScenarioWithRequests(t, dirs,
		[]copilotfixture.Turn{
			createTurn("symlink_outside", filepath.Join(dirs.WorkDir, "escape", "via_symlink")),
			createTurn("symlink_denied", filepath.Join(dirs.WorkDir, "escape_denied", "via_symlink")),
			createTurn("traversal", traversalArgument),
		}, "--allow-all-paths")

	// Proof that the un-normalized path reached the CLI: the tool call is
	// replayed verbatim in the follow-up request's message history, so the
	// literal segment is observable on the wire.
	recorded, err := json.Marshal(requests)
	require.NoError(t, err)
	assert.Contains(t, string(recorded), traversalArgument,
		"the CLI must have received the literal `..` path; if this fails the "+
			"traversal case is normalizing before the CLI sees it and proves nothing")

	for id, path := range map[string]string{
		"symlink_outside": filepath.Join(dirs.SystemTemp, "via_symlink"),
		"symlink_denied":  filepath.Join(dirs.Denied, "via_symlink"),
		"traversal":       filepath.Join(dirs.Root, "via_traversal"),
	} {
		assert.Contains(t, results[id], nativeSandboxBlockedMarker,
			"%s must be refused: the policy check resolves the path before deciding", id)
		assert.False(t, exists(t, path),
			"%s escaped the built-in edit policy; the TCL-977 write-up says it does not, "+
				"so either the CLI regressed or the finding needs restating", id)
	}
}

// TestCopilotNativeSandboxShellEnforcementIsHostConditional pins the shell half
// of the boundary on BOTH kinds of host, with no arm that skips.
//
// Copilot's Linux backend needs bubblewrap AND permission to create an
// unprivileged user namespace; a hardened kernel, a container or an AppArmor
// profile can withhold the latter from a host that has `bwrap` installed. That
// makes "is the sandbox enforcing?" a property of the machine, which is the
// third reason TCL-977 declines the capability: tclaude cannot verify it at
// launch. So the scenario measures the host and asserts a different real
// property per arm — enforcement where the backend runs, fail-closed
// degradation where it does not. Neither arm is a skip, and a host that changes
// category changes which assertions run, not whether any do.
func TestCopilotNativeSandboxShellEnforcementIsHostConditional(t *testing.T) {
	requireSmoke(t)

	dirs := newNativeSandboxDirs(t)
	enableNativeSandbox(t, dirs, true)

	workspaceMarker := filepath.Join(dirs.WorkDir, "shell_workspace")
	deniedMarker := filepath.Join(dirs.Denied, "shell_denied")
	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		bashTurn("shell_workspace", "touch "+workspaceMarker),
		bashTurn("shell_denied", "touch "+deniedMarker),
	})
	backend := classifyBackend(t, exists(t, workspaceMarker), results["shell_workspace"])

	// The denied path is the enforcement claim, and it is a strong one HERE
	// because of where the fixture's directories live: everything a hermetic
	// scenario can write to is under the system temp directory, which Copilot's
	// base policy grants by default. So this target is inside a base grant and
	// still refused — an authored deny beats the auto-discovered base rather
	// than being merged into it.
	//
	// What this test deliberately does NOT assert is a write "outside every
	// granted path". That phrasing was wrong by construction: see
	// TestCopilotNativeSandboxShellBasePolicySurface, which measures the base
	// surface instead of assuming it.
	assert.False(t, exists(t, deniedMarker),
		"a shell write into an explicitly denied path must not land")

	if backend.Up {
		// The granted write landing IS what classified the backend as up, so
		// there is nothing further to assert on this arm: the deny above is the
		// enforcement claim, and it means something precisely because the
		// sandbox is demonstrably not refusing everything.
		return
	}
	// Fail-closed arm: the backend could not start, and the CLI's answer is to
	// fail the command rather than run it unconfined. That is the right
	// behavior, and pinning it here is what would make a future silent
	// downgrade to an unsandboxed shell a test failure rather than a surprise.
	//
	// The assertion is on the FAILURE SIGNATURE, not merely on a non-empty
	// result: a successful `touch` also returns a non-empty result (an exit-0
	// line), so "reported something" would pass whether the command failed
	// closed or ran unconfined.
	assert.False(t, exists(t, workspaceMarker),
		"with no usable backend the shell must fail closed, not run unconfined")
	for _, id := range []string{"shell_workspace", "shell_denied"} {
		_, err := copilotfixture.ClassifyNativeSandboxBackend(false, results[id])
		assert.NoError(t, err,
			"%s must report a backend-start failure rather than an ambiguous "+
				"result; got %q", id, results[id])
	}
}

// TestCopilotNativeSandboxShellBasePolicySurface measures the base policy the
// OS sandbox generates, rather than assuming it.
//
// This test exists because an earlier version of the scenario above asserted
// that a shell write "outside every granted path" would not land, and CI
// refuted it on Linux AND macOS. The assumption was wrong by construction:
// `copilot help sandbox` documents the default surface as the working
// directory, PATH directories, **the system temp directory**, and the user
// profile — and a hermetic fixture necessarily builds all of its directories
// inside the system temp directory. The "outside" target was inside a base
// grant the whole time.
//
// Two candidate explanations had to be told apart, and neither could be assumed
// from a host where the backend cannot start: the base temp grant, and
// `--allow-all-paths` widening the generated OS policy rather than only
// suppressing the CLI's own path-permission layer. The matrix below crosses
// that flag with `--disallow-temp-dir`, Copilot's documented opt-out for the
// temp grant, so the two are separable in one run.
//
// Only the invariants are asserted; the surface itself is logged, because the
// point of this test is to RECORD what the base policy covers on each platform
// rather than to legislate it. The invariant that does hold everywhere is the
// one that matters: an authored deny is honored no matter which flags are
// passed and no matter that the denied directory sits inside a base grant.
//
// WHAT CI MEASURED, identically on Linux and macOS:
//
//	config                            workspace  home   temp   denied
//	allow_all_paths                   yes        yes    yes    no
//	no_allow_all_paths                yes        yes    yes    no
//	allow_all_paths+disallow_temp     yes        yes    yes    no
//	no_allow_all_paths+disallow_temp  yes        no     no     no
//
// Three readings, and the third is the one to be careful about:
//
//   - `--allow-all-paths` on its own changes NOTHING (rows 1 vs 2). The
//     hypothesis that it widens the generated OS policy is not supported.
//   - `--disallow-temp-dir` on its own changes NOTHING either (rows 1 vs 3).
//     It only bites once the path-permission layer is left on, so
//     `--allow-all-paths` suppresses it.
//   - Row 4 is NOT evidence about the OS sandbox's base policy. Both the OS
//     sandbox and the CLI's path-permission layer grant the temp directory, a
//     hermetic fixture puts HOME inside it, and this matrix cannot separate
//     which layer allowed a write that both permit. What it does establish is
//     the thing the earlier assertion got wrong: these targets are inside a
//     granted surface, so a write landing there is not an escape.
func TestCopilotNativeSandboxShellBasePolicySurface(t *testing.T) {
	requireSmoke(t)

	configurations := []struct {
		name string
		args []string
	}{
		{"allow_all_paths", []string{"--allow-all-paths"}},
		{"no_allow_all_paths", nil},
		{"allow_all_paths+disallow_temp", []string{"--allow-all-paths", "--disallow-temp-dir"}},
		{"no_allow_all_paths+disallow_temp", []string{"--disallow-temp-dir"}},
	}
	for _, configuration := range configurations {
		t.Run(configuration.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			enableNativeSandbox(t, dirs, true)

			targets := map[string]string{
				"workspace":   filepath.Join(dirs.WorkDir, "probe"),
				"home":        filepath.Join(dirs.Root, "probe"),
				"system_temp": filepath.Join(dirs.SystemTemp, "probe"),
				"denied":      filepath.Join(dirs.Denied, "probe"),
			}
			turns := make([]copilotfixture.Turn, 0, len(targets))
			for _, name := range []string{"workspace", "home", "system_temp", "denied"} {
				turns = append(turns, bashTurn("shell_"+name, "touch "+targets[name]))
			}
			results := runNativeSandboxScenarioWithArgs(
				t, dirs, turns, configuration.args...)
			// Called for its log line and for its refusal to classify an
			// ambiguous run; this scenario asserts the same invariant on either
			// arm, so the verdict itself is not branched on.
			classifyBackend(
				t, exists(t, targets["workspace"]), results["shell_workspace"])

			for _, name := range []string{"workspace", "home", "system_temp", "denied"} {
				t.Logf("BASE SURFACE %-32s %-12s reachable=%v",
					configuration.name, name, exists(t, targets[name]))
			}
			assert.False(t, exists(t, targets["denied"]),
				"an authored deniedPaths entry must be honored under %s, even though "+
					"the denied directory lies inside the base temp grant",
				configuration.name)

		})
	}
}

// TestCopilotNativeSandboxNeedsNoExperimentalFlag records a launch-surface fact
// that is easy to get backwards from the documentation.
//
// `copilot help sandbox` says the feature is experimental and that the
// `/sandbox` command is registered only when experimental features are on. That
// gate is on the interactive COMMAND, not on the feature: settings that enable
// sandboxing take effect with no `--experimental` anywhere. (This scenario
// writes the CANONICAL settings.json; the legacy config.json is covered by
// TestCopilotNativeSandboxSettingsSourcesAndPrecedence.) It matters
// because it cuts both ways for tclaude — the posture cannot be turned on by a
// launch argument, and it also cannot be assumed off just because tclaude
// passed no experimental flag.
func TestCopilotNativeSandboxNeedsNoExperimentalFlag(t *testing.T) {
	requireSmoke(t)

	for _, experimental := range []bool{false, true} {
		t.Run(fmt.Sprintf("experimental=%v", experimental), func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			enableNativeSandbox(t, dirs, true)
			var extra []string
			if experimental {
				extra = append(extra, "--experimental")
			}
			results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
				createTurn("edit_denied", filepath.Join(dirs.Denied, "in_denied")),
			}, extra...)
			assert.Contains(t, results["edit_denied"], nativeSandboxBlockedMarker,
				"the settings file enables the sandbox regardless of --experimental")
			assert.False(t, exists(t, filepath.Join(dirs.Denied, "in_denied")))
		})
	}
}

// TestCopilotNativeSandboxDisabledAppliesNoPolicy is the control every negative
// assertion in this file depends on.
//
// With `sandbox.enabled` false the SAME settings file — including its
// deniedPaths — governs nothing: built-in edits land in the denied directory,
// in HOME, and outside HOME alike. So the refusals measured elsewhere are the
// sandbox's doing rather than the CLI's ordinary behavior, and the sandbox's
// default-off posture is itself pinned: an operator who never enabled it has no
// containment at all, which is the state tclaude confines Copilot from the
// outside for.
func TestCopilotNativeSandboxDisabledAppliesNoPolicy(t *testing.T) {
	requireSmoke(t)

	dirs := newNativeSandboxDirs(t)
	enableNativeSandbox(t, dirs, false)

	paths := map[string]string{
		"edit_home":    filepath.Join(dirs.Root, "in_home"),
		"edit_denied":  filepath.Join(dirs.Denied, "in_denied"),
		"edit_outside": filepath.Join(dirs.SystemTemp, "outside_home"),
	}
	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		createTurn("edit_home", paths["edit_home"]),
		createTurn("edit_denied", paths["edit_denied"]),
		createTurn("edit_outside", paths["edit_outside"]),
	})
	for id, path := range paths {
		assert.NotContains(t, results[id], nativeSandboxBlockedMarker,
			"%s must not be refused while the sandbox is disabled", id)
		assert.True(t, exists(t, path),
			"%s must land: a disabled sandbox applies none of its own deniedPaths", id)
	}
}

// TestCopilotNativeSandboxSettingsSourcesAndPrecedence pins WHERE the posture
// comes from, which is a security question rather than a trivia one.
//
// COPILOT_HOME carries two settings files and both are live. `settings.json` is
// the canonical one; `config.json` is a legacy source the CLI MIGRATES from at
// startup — it wins for that launch and then overwrites settings.json with its
// contents, leaving config.json as a managed stub. So anything that inspects a
// Copilot sandbox posture and reads only one of the two names is wrong in a
// direction that matters: reading only settings.json can be bypassed by
// dropping a config.json that disables the sandbox, and reading only
// config.json misses the canonical file entirely.
//
// Five arms, each a real launch: either file alone enables the sandbox, neither
// file leaves it off, and when the two disagree config.json decides.
func TestCopilotNativeSandboxSettingsSourcesAndPrecedence(t *testing.T) {
	requireSmoke(t)

	cases := []struct {
		name string
		// nil means "do not write this file at all".
		legacy, canonical *bool
		wantEnforced      bool
	}{
		{"canonical_only", nil, new(true), true},
		{"legacy_only", new(true), nil, true},
		{"neither", nil, nil, false},
		{"legacy_on_beats_canonical_off", new(true), new(false), true},
		{"legacy_off_beats_canonical_on", new(false), new(true), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			settings := func(enabled bool) copilotfixture.NativeSandboxSettings {
				return copilotfixture.NativeSandboxSettings{
					Enabled: enabled, AddCurrentWorkingDirectory: true, AllowBypass: false,
					UserPolicy: copilotfixture.NativeSandboxUserPolicy{
						Filesystem: copilotfixture.NativeSandboxFilesystem{
							DeniedPaths: []string{dirs.Denied},
						},
					},
				}
			}
			if testCase.legacy != nil {
				copilotfixture.WriteNativeSandboxSettingsTo(t, dirs.Dirs,
					copilotfixture.NativeLegacySettingsFile, settings(*testCase.legacy))
			}
			if testCase.canonical != nil {
				copilotfixture.WriteNativeSandboxSettingsTo(t, dirs.Dirs,
					copilotfixture.NativeSettingsFile, settings(*testCase.canonical))
			}

			target := filepath.Join(dirs.Denied, "in_denied")
			results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
				createTurn("edit_denied", target),
			})

			if testCase.wantEnforced {
				assert.Contains(t, results["edit_denied"], nativeSandboxBlockedMarker)
				assert.False(t, exists(t, target),
					"the posture these settings describe was not applied")
				return
			}
			assert.NotContains(t, results["edit_denied"], nativeSandboxBlockedMarker)
			assert.True(t, exists(t, target),
				"a sandbox was applied that these settings did not ask for")
		})
	}

	// The migration itself, pinned separately from the verdicts above: a gate
	// that saw config.json as inert legacy would be reading a file the CLI is
	// about to promote over the canonical one.
	t.Run("legacy_is_migrated_into_canonical", func(t *testing.T) {
		dirs := newNativeSandboxDirs(t)
		copilotfixture.WriteNativeSandboxSettingsTo(t, dirs.Dirs,
			copilotfixture.NativeLegacySettingsFile, copilotfixture.NativeSandboxSettings{
				Enabled: true, AddCurrentWorkingDirectory: true,
				UserPolicy: copilotfixture.NativeSandboxUserPolicy{
					Filesystem: copilotfixture.NativeSandboxFilesystem{
						DeniedPaths: []string{dirs.Denied},
					},
				},
			})
		runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
			createTurn("edit_denied", filepath.Join(dirs.Denied, "in_denied")),
		})

		canonical, err := os.ReadFile(
			filepath.Join(dirs.Home, copilotfixture.NativeSettingsFile))
		require.NoError(t, err,
			"the CLI must have written the canonical settings file")
		assert.Contains(t, string(canonical), dirs.Denied,
			"the legacy file's sandbox policy must have been migrated into the "+
				"canonical one; a reader of settings.json alone would otherwise "+
				"see a posture the next launch replaces")
	})

	// The SHAPE of the merge, which is what anything modelling this precedence
	// gets wrong first. The merge is shallow: a top-level key the legacy file
	// never mentions survives, but one it does mention has its whole VALUE
	// replaced. So a legacy `sandbox` block that sets some unrelated field
	// discards the canonical file's `sandbox.enabled` rather than merging with
	// it — a key-by-key model would compute a posture the CLI never produces.
	t.Run("the_merge_is_shallow", func(t *testing.T) {
		dirs := newNativeSandboxDirs(t)
		// Canonical: sandbox ENABLED, plus an unrelated top-level key.
		copilotfixture.WriteNativeSandboxSettingsTo(t, dirs.Dirs,
			copilotfixture.NativeSettingsFile, copilotfixture.NativeSandboxSettings{
				Enabled: true, AddCurrentWorkingDirectory: true,
				UserPolicy: copilotfixture.NativeSandboxUserPolicy{
					Filesystem: copilotfixture.NativeSandboxFilesystem{
						DeniedPaths: []string{dirs.Denied},
					},
				},
			})
		// Legacy: a sandbox block that never mentions `enabled`.
		require.NoError(t, os.WriteFile(
			filepath.Join(dirs.Home, copilotfixture.NativeLegacySettingsFile),
			[]byte(`{"sandbox":{"addCurrentWorkingDirectory":true}}`), 0o600))

		target := filepath.Join(dirs.Denied, "in_denied")
		results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
			createTurn("edit_denied", target),
		})

		assert.NotContains(t, results["edit_denied"], nativeSandboxBlockedMarker,
			"the legacy sandbox block replaced the canonical one WHOLESALE, so the "+
				"canonical file's enabled:true did not survive and no sandbox applies")
		assert.True(t, exists(t, target))

		canonical, err := os.ReadFile(
			filepath.Join(dirs.Home, copilotfixture.NativeSettingsFile))
		require.NoError(t, err)
		assert.NotContains(t, string(canonical), dirs.Denied,
			"the canonical file's whole sandbox object must have been replaced, "+
				"including the deniedPaths it carried")
	})
}

// The descriptor refusal this suite justifies is asserted in
// harness.TestCopilotHarnessRefusesBuiltinOSSandbox — beside the code it covers,
// per the repository's testing guidance. It needs no CLI and no fixture, so it
// runs in the ordinary `go test ./...` job rather than in the pinned-binary one.
