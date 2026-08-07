package copilotfixture_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-1011: the per-launch sandbox flags.
//
// sandbox_native_smoke_test.go and harness/copilot_sandbox.go both rest on the
// premise that Copilot's own command sandbox has NO launch flag, so tclaude can
// only ASSERT a posture read from a settings file and never SELECT one for a
// single launch. That premise was measured against `copilot --help`, and
// `copilot --help` is not the whole argv surface: 1.0.70's changelog entry adds
// "--sandbox and --no-sandbox flags to turn the OS-level shell sandbox on or off
// for the current session only, without changing your saved sandbox setting",
// and neither flag is listed in the 1.0.77 help output.
//
// These scenarios measure the flags rather than trusting either source. Parse
// acceptance is measured separately from effect, because the suite's own
// url-access entry is a standing reminder that the two come apart.
//
// The instrument throughout is a built-in `create` into an explicitly denied
// path, NOT a shell command. That choice is what makes these scenarios
// host-independent: built-in edits are governed by the CLI's in-process policy
// check (TCL-977), so they answer "is a sandbox policy in force for this
// launch?" identically on a host whose OS backend starts and on one where it
// cannot. A shell-based instrument would answer "the backend is down" on the
// second kind of host and measure nothing about the flag.

// nativeSandboxFlagPosture writes the shared settings posture: a denied
// directory that is always present, and an `enabled` value the flag under test
// is supposed to override for one launch.
func nativeSandboxFlagPosture(t *testing.T, dirs nativeSandboxDirs, enabled bool) {
	t.Helper()
	copilotfixture.WriteNativeSandboxSettings(t, dirs.Dirs, copilotfixture.NativeSandboxSettings{
		Enabled:                    enabled,
		AddCurrentWorkingDirectory: true,
		AllowBypass:                false,
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

// TestCopilotNativeSandboxSessionFlagsAreAccepted establishes the argv fact on
// its own, before any question of effect.
//
// The unknown-flag control is the point of the scenario: `--sandbox` exiting 0
// proves nothing unless a neighbouring spelling the parser has never heard of
// exits non-zero on the same rig. Without it, a CLI that ignored every unknown
// option would report both flags as accepted.
//
// The hidden-ness is asserted rather than merely described, because "accepted
// but undocumented" is the whole reason this surface went unnoticed: a release
// that starts listing the flags in `--help` is a release where the feature may
// have left experimental, which is one of the recorded revisit triggers.
func TestCopilotNativeSandboxSessionFlagsAreAccepted(t *testing.T) {
	requireLabParallel(t)

	t.Run("hidden-from-help", func(t *testing.T) {
		help, err := exec.Command("copilot", "--no-auto-update", "--no-color", "--help").
			CombinedOutput()
		require.NoError(t, err, "running `copilot --help`: %s", string(help))
		for _, flag := range []string{"--sandbox", "--no-sandbox"} {
			// Anchored on the leading two spaces of an option row: the help
			// text mentions the word "sandbox" as a HELP TOPIC, so a bare
			// substring search would find it and assert nothing.
			assert.NotContains(t, string(help), "  "+flag+" ",
				"%s is listed in `copilot --help`; it was hidden when these scenarios "+
					"were written, and a release that documents it is a release worth "+
					"re-reading the experimental gate on", flag)
		}
	})

	for _, testCase := range []struct {
		name       string
		flag       string
		wantAccept bool
	}{
		{"sandbox", "--sandbox", true},
		{"no-sandbox", "--no-sandbox", true},
		// Not a plausible typo — a deliberately impossible neighbour, so the
		// row cannot accidentally name a real flag a later release adds.
		{"unknown-control", "--sandbox-tcl1011-control", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
				{Text: "MOCK FLAG-PARSE ANSWER"},
			})
			result := copilotfixture.Run(t, copilotfixture.RunOptions{
				Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
				WorkDir: dirs.WorkDir, BaseURL: mock.BaseURL(),
				Prompt: "Parse question.", ExtraArgs: []string{testCase.flag},
			})
			if testCase.wantAccept {
				assert.Equal(t, 0, result.ExitCode,
					"%s must be accepted by the 1.0.77 parser even though `copilot --help` "+
						"does not list it; stderr: %s", testCase.flag, result.Stderr)
				return
			}
			assert.NotEqual(t, 0, result.ExitCode,
				"the control spelling must be REJECTED, or the accepted rows above "+
					"measure a parser that swallows anything")
			assert.Contains(t, result.Stderr, "unknown option",
				"the control must fail at argument parsing rather than somewhere else")
		})
	}
}

// TestCopilotNativeSandboxSessionFlagsNeedExperimental is the finding, and it
// is not the one the changelog entry suggests.
//
// The flags DO select the posture for a single launch, in both directions,
// overriding a settings file that says the opposite — but only when
// experimental features are on. Without `--experimental` they are accepted by
// the parser (the scenario above) and then ignored, in both directions.
//
// That distinction is the whole answer to whether tclaude could offer a
// copilot-native posture on this surface, because `--experimental` is exactly
// what CopilotTclaudeLayerExtraArgRefusal refuses and what
// ValidateCopilotTclaudeLayerInnerSandbox refuses over: it registers the in-pane
// `/sandbox` command, so a posture selected on the command line can be revoked
// from inside the pane. A per-launch override that is only available together
// with an in-pane override of itself is not the durable, recorded posture the
// approval catalog asks for.
//
// The two `experimental_only` rows are the control that makes the honored rows
// mean something: `--experimental` on its own changes NOTHING about which
// posture applies, so the flagged rows cannot be credited to it. Without them,
// "disabled + --sandbox + --experimental enforced" would be consistent with
// experimental features enabling the sandbox by themselves.
func TestCopilotNativeSandboxSessionFlagsNeedExperimental(t *testing.T) {
	requireLabParallel(t)

	for _, testCase := range []struct {
		name string
		// settingsEnabled is what `sandbox.enabled` says on disk.
		settingsEnabled bool
		args            []string
		// wantEnforced is whether the launch is expected to refuse a create
		// into the settings file's own deniedPaths entry.
		wantEnforced bool
	}{
		// Flagless controls: the settings file alone decides.
		{"enabled_no_flag", true, nil, true},
		{"disabled_no_flag", false, nil, false},

		// `--experimental` alone: still the settings file alone.
		{"enabled_experimental_only", true, []string{"--experimental"}, true},
		{"disabled_experimental_only", false, []string{"--experimental"}, false},

		// The flags WITHOUT experimental features: parsed, then ignored. Both
		// directions, because a flag honored in only one of them would be a
		// different and much more dangerous finding.
		{"enabled_no_sandbox_flag", true, []string{"--no-sandbox"}, true},
		{"disabled_sandbox_flag", false, []string{"--sandbox"}, false},

		// The flags WITH experimental features: honored, overriding the file.
		{"enabled_no_sandbox_flag_experimental", true,
			[]string{"--no-sandbox", "--experimental"}, false},
		{"disabled_sandbox_flag_experimental", false,
			[]string{"--sandbox", "--experimental"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			nativeSandboxFlagPosture(t, dirs, testCase.settingsEnabled)

			target := filepath.Join(dirs.Denied, "in_denied")
			results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
				createTurn("edit_denied", target),
			}, testCase.args...)

			if testCase.wantEnforced {
				assert.Contains(t, results["edit_denied"], nativeSandboxBlockedMarker,
					"a sandbox policy must be in force for this launch")
				assert.False(t, exists(t, target),
					"the denied write must not have landed")
				return
			}
			assert.NotContains(t, results["edit_denied"], nativeSandboxBlockedMarker,
				"no sandbox policy may be in force for this launch")
			assert.True(t, exists(t, target),
				"the denied write must land: the settings file's deniedPaths entry "+
					"governs nothing when the sandbox is not engaged")
		})
	}
}

// TestCopilotNativeSandboxAllowBypassDemotesEnforcementToAPrompt measures the
// `sandbox.allowBypass` setting, and it changes what the word "enforcement"
// means for every scenario in this suite that leaves the key false.
//
// The investigation reached it from a 1.0.77 changelog entry — "Unconditional
// autopilot approval now disables sandbox for the current session when bypass
// is allowed" — which reads as though autopilot were the actor. Measured, the
// actor is `allowBypass` alone: with it on, a policy violation stops being a
// refusal and becomes a PERMISSION REQUEST. The tool result changes from the
// sandbox's own "sandbox is active and blocked …" to the CLI's ordinary
// "Permission denied and could not request permission from user", which is the
// text a prompt nobody could answer produces.
//
// So a Copilot sandbox with `allowBypass: true` is not a boundary; it is a
// question, and the answer decides the boundary. That distinction is invisible
// to any scenario that only checks whether the file landed — in every arm here
// it did not — and it is decisive for a posture tclaude would have to describe
// to an operator.
//
// WHAT THIS DOES NOT ESTABLISH, stated because the headless instrument cannot
// reach it: whether the prompt AUTO-APPROVES on a real terminal. These runs
// carry the runner's default `--allow-all-tools`, and it did not approve the
// bypass — but a no-TTY launch auto-denies path prompts (see the permission
// matrix's out-of-cwd-paths entry) while auto-allowing tool prompts, so a
// headless denial is not evidence about a pane. The changelog's autopilot claim
// lives on exactly that path, and neither `--autopilot` nor `--yolo` reproduces
// it at launch here.
func TestCopilotNativeSandboxAllowBypassDemotesEnforcementToAPrompt(t *testing.T) {
	requireLabParallel(t)

	// The result text of a bypass request that reached nobody. Matched rather
	// than the sandbox marker because the whole finding is that the two are
	// different strings produced by different layers.
	const bypassPromptMarker = "Permission denied and could not request permission from user"

	for _, testCase := range []struct {
		name        string
		allowBypass bool
		args        []string
		// wantPrompt is whether the refusal is expected to arrive as an
		// unanswerable permission request rather than as a sandbox refusal.
		wantPrompt bool
	}{
		{"bypass_off", false, nil, false},
		{"bypass_on", true, nil, true},
		// Autopilot and yolo are crossed with both settings because the
		// changelog entry names autopilot as the trigger. Neither changes the
		// outcome at launch: `allowBypass` decides it on its own.
		{"bypass_off_autopilot", false, []string{"--autopilot"}, false},
		{"bypass_on_autopilot", true, []string{"--autopilot"}, true},
		{"bypass_on_yolo", true, []string{"--yolo"}, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			copilotfixture.WriteNativeSandboxSettings(t, dirs.Dirs,
				copilotfixture.NativeSandboxSettings{
					Enabled: true, AddCurrentWorkingDirectory: true,
					AllowBypass: testCase.allowBypass,
					UserPolicy: copilotfixture.NativeSandboxUserPolicy{
						Filesystem: copilotfixture.NativeSandboxFilesystem{
							DeniedPaths: []string{dirs.Denied},
						},
						// Matches enableNativeSandbox's posture rather than
						// defaulting to false: the mock provider is reached by
						// the CLI process rather than by a sandboxed child
						// today, and a scenario should not quietly depend on
						// that staying true.
						Network: copilotfixture.NativeSandboxNetwork{
							AllowOutbound: false, AllowLocalNetwork: true,
						},
					},
				})

			target := filepath.Join(dirs.Denied, "in_denied")
			results := runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
				createTurn("edit_denied", target),
			}, testCase.args...)

			// Common to every arm, and the reason the finding is about the
			// MECHANISM rather than the outcome: nothing reached the host.
			assert.False(t, exists(t, target),
				"a headless launch must fail closed whichever layer refused")

			if testCase.wantPrompt {
				assert.Contains(t, results["edit_denied"], bypassPromptMarker,
					"`allowBypass: true` must turn the violation into a permission "+
						"request; if this starts reading as a sandbox refusal again the "+
						"setting has stopped being a prompt and the suite's other "+
						"scenarios may enable it safely")
				assert.NotContains(t, results["edit_denied"], nativeSandboxBlockedMarker)
				return
			}
			assert.Contains(t, results["edit_denied"], nativeSandboxBlockedMarker,
				"with `allowBypass: false` the sandbox must refuse outright")
			assert.NotContains(t, results["edit_denied"], bypassPromptMarker)
		})
	}
}

// TestCopilotNativeSandboxSessionFlagsDoNotPersist measures the property that
// decides whether tclaude could ever USE these flags.
//
// The changelog says the flags apply "for the current session only, without
// changing your saved sandbox setting". If that is wrong — if `--no-sandbox`
// writes `enabled: false` into the operator's settings the way Copilot's
// preference-style flags do — then a tclaude launch passing it would silently
// edit the human's configuration and leave their next hand-run Copilot session
// unsandboxed. That is a materially different product from a per-launch
// override, so it is measured rather than read.
//
// Both directions are checked, because a flag that only persisted in the
// widening direction would still be the dangerous one.
//
// Every arm passes `--experimental`. That is load-bearing rather than
// incidental: without it the flags are ignored entirely (see the scenario
// above), so a non-experimental run would establish only that a no-op writes
// nothing — which is true of every unknown flag and says nothing about these.
func TestCopilotNativeSandboxSessionFlagsDoNotPersist(t *testing.T) {
	requireLabParallel(t)

	for _, testCase := range []struct {
		name            string
		settingsEnabled bool
		flag            string
	}{
		{"no_sandbox_over_enabled", true, "--no-sandbox"},
		{"sandbox_over_disabled", false, "--sandbox"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			nativeSandboxFlagPosture(t, dirs, testCase.settingsEnabled)

			runNativeSandboxScenario(t, dirs, []copilotfixture.Turn{
				createTurn("edit_workspace", filepath.Join(dirs.WorkDir, "probe")),
			}, testCase.flag, "--experimental")

			// Read through the SAME two-file resolver tclaude's pre-launch gate
			// uses, rather than parsing the canonical file directly: the CLI's
			// startup migration is what decides the effective value, and a raw
			// read of settings.json would report a posture the next launch
			// replaces. This also makes the scenario exercise a production read
			// path instead of a test-local one.
			runEnv := map[string]string{harness.CopilotHomeEnvVar: dirs.Home}
			state, err := harness.ResolveCopilotInnerSandbox(
				func(k string) string { return runEnv[k] }, dirs.Root)
			require.NoError(t, err,
				"the settings the run left behind must still be readable")
			assert.Equal(t, testCase.settingsEnabled, state.Enabled,
				"%s must not rewrite the operator's saved `sandbox.enabled`; if this "+
					"fails the flag is a preference mutation rather than a per-launch "+
					"override, and tclaude must not pass it", testCase.flag)
		})
	}
}
