package copilotfixture_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

// nativeSandboxBlockedMarker is the CLI's own in-process refusal text, emitted
// as the tool RESULT rather than as an OS error. Matching on it is how a
// scenario tells "the CLI's policy check refused this" apart from "the OS
// refused this", which is the exact distinction TCL-977 turns on.
const nativeSandboxBlockedMarker = "sandbox is active and blocked"

// nativeSandboxDirs builds a scenario's directory set: the granted workspace,
// an explicitly denied directory inside HOME, and a directory outside HOME
// entirely that the policy never mentions.
type nativeSandboxDirs struct {
	copilotfixture.Dirs
	Denied  string
	Outside string
}

func newNativeSandboxDirs(t *testing.T) nativeSandboxDirs {
	t.Helper()
	dirs := copilotfixture.NewSandboxDirs(t)
	denied := filepath.Join(dirs.Root, "denied")
	require.NoError(t, os.MkdirAll(denied, 0o755))
	return nativeSandboxDirs{Dirs: dirs, Denied: denied, Outside: t.TempDir()}
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

func runNativeSandboxScenario(
	t *testing.T, dirs nativeSandboxDirs, turns []copilotfixture.Turn, extra ...string,
) map[string]string {
	t.Helper()
	mock := copilotfixture.NewMockProvider(t, append(turns, copilotfixture.Turn{Text: "MOCK DONE"}))
	args := append([]string{"--allow-all-paths"}, extra...)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir, BaseURL: mock.BaseURL(),
		Prompt: "Run the tools the provider asks for.", ExtraArgs: args,
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	return toolResults(mock.Requests())
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
	backendUp, evidence := copilotfixture.NativeSandboxBackendAvailable(t)
	t.Logf("OS sandbox backend available: %v (%s)", backendUp, evidence)

	inWorkspace := filepath.Join(dirs.WorkDir, "in_workspace")
	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		createTurn("edit_workspace", inWorkspace),
		createTurn("edit_home", filepath.Join(dirs.Root, "in_home")),
		createTurn("edit_denied", filepath.Join(dirs.Denied, "in_denied")),
		createTurn("edit_outside", filepath.Join(dirs.Outside, "outside_home")),
		bashTurn("shell_workspace", "touch "+filepath.Join(dirs.WorkDir, "shell_marker")),
	})

	assert.True(t, exists(t, inWorkspace),
		"a built-in edit inside the granted workspace must succeed; without it the "+
			"refusals below would only mean the policy blocks everything")
	for id, path := range map[string]string{
		"edit_home":    filepath.Join(dirs.Root, "in_home"),
		"edit_denied":  filepath.Join(dirs.Denied, "in_denied"),
		"edit_outside": filepath.Join(dirs.Outside, "outside_home"),
	} {
		assert.NotContains(t, results[id], "Created file",
			"%s must be refused by the sandbox policy", id)
		assert.Contains(t, results[id], nativeSandboxBlockedMarker,
			"%s must be refused by the CLI's OWN policy check — an OS refusal would "+
				"surface as an errno, not as this message", id)
		assert.False(t, exists(t, path), "%s must not have been written", id)
	}

	if !backendUp {
		// The decisive arm. The OS sandbox could not be entered at all, so a
		// shell command cannot run — and yet the built-in edit above wrote its
		// file. The two halves of Copilot's boundary are therefore enforced by
		// two different things, and only one of them is the OS.
		assert.False(t, exists(t, filepath.Join(dirs.WorkDir, "shell_marker")),
			"with no usable OS backend a shell command must not run at all")
		assert.True(t, exists(t, inWorkspace),
			"the built-in edit wrote its file on a host where the OS sandbox could "+
				"not start; the edit path never enters the sandbox")
	}
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
	require.NoError(t, os.Symlink(dirs.Outside, filepath.Join(dirs.WorkDir, "escape")))
	require.NoError(t, os.Symlink(dirs.Denied, filepath.Join(dirs.WorkDir, "escape_denied")))

	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		createTurn("symlink_outside", filepath.Join(dirs.WorkDir, "escape", "via_symlink")),
		createTurn("symlink_denied", filepath.Join(dirs.WorkDir, "escape_denied", "via_symlink")),
		createTurn("traversal", filepath.Join(dirs.WorkDir, "..", "via_traversal")),
	})

	for id, path := range map[string]string{
		"symlink_outside": filepath.Join(dirs.Outside, "via_symlink"),
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
	backendUp, evidence := copilotfixture.NativeSandboxBackendAvailable(t)
	t.Logf("OS sandbox backend available: %v (%s)", backendUp, evidence)

	workspaceMarker := filepath.Join(dirs.WorkDir, "shell_workspace")
	deniedMarker := filepath.Join(dirs.Denied, "shell_denied")
	outsideMarker := filepath.Join(dirs.Outside, "shell_outside")
	results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
		bashTurn("shell_workspace", "touch "+workspaceMarker),
		bashTurn("shell_denied", "touch "+deniedMarker),
		bashTurn("shell_outside", "touch "+outsideMarker),
	})

	assert.False(t, exists(t, deniedMarker),
		"a shell write into an explicitly denied path must not land")
	assert.False(t, exists(t, outsideMarker),
		"a shell write outside every granted path must not land")

	if backendUp {
		assert.True(t, exists(t, workspaceMarker),
			"with a working backend a shell write inside the granted workspace must "+
				"succeed; if it does not, the two refusals above prove nothing about "+
				"the policy because the sandbox is refusing everything")
		return
	}
	// Fail-closed arm: the backend could not start, and the CLI's answer is to
	// fail the command rather than run it unconfined. That is the right
	// behavior, and pinning it here is what would make a future silent
	// downgrade to an unsandboxed shell a test failure rather than a surprise.
	assert.False(t, exists(t, workspaceMarker),
		"with no usable backend the shell must fail closed, not run unconfined")
	for _, id := range []string{"shell_workspace", "shell_denied", "shell_outside"} {
		assert.NotEmpty(t, results[id], "%s must still report a result", id)
	}
}

// TestCopilotNativeSandboxNeedsNoExperimentalFlag records a launch-surface fact
// that is easy to get backwards from the documentation.
//
// `copilot help sandbox` says the feature is experimental and that the
// `/sandbox` command is registered only when experimental features are on. That
// gate is on the interactive COMMAND, not on the feature: a config.json that
// enables sandboxing takes effect with no `--experimental` anywhere. It matters
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
				"config.json enables the sandbox regardless of --experimental")
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
		"edit_outside": filepath.Join(dirs.Outside, "outside_home"),
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
}

// TestCopilotHarnessRefusesBuiltinOSSandbox ties the measurements above to the
// descriptor they justify, in the same file, so the capability and its evidence
// cannot drift apart. It needs no CLI, hence no smoke gate.
func TestCopilotHarnessRefusesBuiltinOSSandbox(t *testing.T) {
	copilot, ok := harness.Get(harness.CopilotName)
	require.True(t, ok)
	require.False(t, copilot.SupportsBuiltinOSSandbox())

	err := harness.ValidateHarnessBuiltinOSSandbox(copilot)
	require.Error(t, err)
	require.True(t, harness.IsBuiltinOSSandboxInvalid(err))
	assert.Contains(t, err.Error(), "built-in file edits are checked by an in-process policy",
		"the refusal must name the property Copilot is missing; a flat "+
			"\"no built-in OS sandbox\" reads as a gap in tclaude to an operator "+
			"who can see the feature in their own CLI")
}
