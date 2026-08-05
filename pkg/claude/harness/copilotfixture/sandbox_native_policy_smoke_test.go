package copilotfixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// TCL-1011: what Copilot's sandbox policy actually CONTAINS on Linux.
//
// Every other scenario in this suite observes the sandbox from the outside —
// a write landed or it did not. That is the right instrument for "is a boundary
// in force", and the wrong one for "what boundary", which is the question a
// harness-native posture would have to answer before tclaude could describe it
// to an operator. A policy is not knowable from its refusals: two very
// different policies refuse the same write.
//
// So this scenario reads the policy itself, by putting a `bwrap` shim first on
// PATH that records the argument vector Copilot's MXC backend generates and
// then exits without starting anything. The recorded argv IS the policy on
// Linux — bubblewrap has no configuration file, so everything the backend asks
// for is in it.
//
// Two properties make this measurement worth its weight:
//
//   - It works on BOTH host categories the suite already distinguishes. The
//     policy is generated before the namespace is created, so a host that
//     cannot create one still yields a complete policy. No arm skips.
//   - It is the only scenario here that can see the parts of the boundary that
//     never produce a refusal: the environment handed to the sandboxed process,
//     and the MECHANISM behind a denied path.
//
// Scope, stated because the omission is deliberate: macOS is not covered. The
// darwin backend generates a Seatbelt profile through `sandbox-exec` rather
// than a bubblewrap argv, so capturing it needs a different shim and a
// different reader, and writing that reader blind — on a machine that cannot
// run it — would put an unmeasured assertion into CI. The darwin profile's
// contents are therefore an open question rather than a covered one.

// nativeSandboxInvocationMarker separates recorded invocations in the shim's
// log. It is a field like any other, so it is written and matched in one place.
const nativeSandboxInvocationMarker = "=== INVOCATION ==="

// nativeSandboxPolicyCapture is one recorded backend invocation.
type nativeSandboxPolicyCapture struct {
	// Argv is the whole vector, as the backend spelled it.
	Argv []string
	// Env is the sandboxed process's environment, from the --setenv pairs.
	Env map[string]string
}

// has reports whether a bare flag (one taking no operands) is present.
func (c nativeSandboxPolicyCapture) has(flag string) bool {
	for _, argument := range c.Argv {
		if argument == flag {
			return true
		}
	}
	return false
}

// operands returns the operand tuples of a flag that takes n of them, so a
// caller can ask "is this path bound read-write" rather than scanning by index.
func (c nativeSandboxPolicyCapture) operands(flag string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(c.Argv); i++ {
		if c.Argv[i] != flag || i+n >= len(c.Argv) {
			continue
		}
		out = append(out, c.Argv[i+1:i+1+n])
		i += n
	}
	return out
}

// bindsPath reports whether the flag binds the given path (as its first
// operand), which is how every filesystem grant in this policy is spelled.
func (c nativeSandboxPolicyCapture) bindsPath(flag string, n int, path string) bool {
	for _, tuple := range c.operands(flag, n) {
		if tuple[0] == path {
			return true
		}
	}
	return false
}

// captureNativeSandboxPolicy runs one scenario with a recording `bwrap` shim
// first on PATH and returns the backend invocations it generated.
//
// The shim answers `--version` itself and refuses everything else, so it
// depends on no real bubblewrap being installed: on a host that has one the
// measurement is identical, because the policy is complete before the
// namespace is attempted.
func captureNativeSandboxPolicy(
	t *testing.T, dirs nativeSandboxDirs, settings copilotfixture.NativeSandboxSettings,
	extraEnv []string,
) (captures []nativeSandboxPolicyCapture, shimDir string) {
	t.Helper()
	copilotfixture.WriteNativeSandboxSettings(t, dirs.Dirs, settings)

	shimDir = filepath.Join(dirs.Root, "bwrap-shim")
	require.NoError(t, os.MkdirAll(shimDir, 0o755))
	log := filepath.Join(dirs.Root, "bwrap-argv.log")
	// NUL-separated, so an argument containing a newline — the wrapped shell
	// command routinely does — cannot be read back as two arguments.
	shim := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo 'bubblewrap 0.11.0'; exit 0; fi\n" +
		"{ printf '" + nativeSandboxInvocationMarker +
		"\\0'; for a in \"$@\"; do printf '%s\\0' \"$a\"; done; } >> '" +
		log + "'\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(shimDir, "bwrap"), []byte(shim), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	args, err := json.Marshal(map[string]any{
		"command":     "touch " + filepath.Join(dirs.WorkDir, "policy_probe"),
		"description": "TCL-1011 policy probe",
	})
	require.NoError(t, err)
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{ID: "shell", Name: "bash", Args: string(args)}},
		{Text: "MOCK DONE"},
	})
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, XDGCache: dirs.XDGCache,
		WorkDir: dirs.WorkDir, BaseURL: mock.BaseURL(), ExtraEnv: extraEnv,
		Prompt: "Run the tools the provider asks for.", ExtraArgs: []string{"--allow-all-paths"},
	})
	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)

	data, err := os.ReadFile(log)
	require.NoError(t, err,
		"the sandbox backend was never invoked, so no policy was generated; either the "+
			"settings did not engage the sandbox or the backend is no longer `bwrap` on PATH")

	// An EMPTY field is a real argument, not padding, and dropping empties is a
	// bug that hides itself: the policy carries `--setenv GIT_ASKPASS ""`, so
	// skipping the empty value shifts every following operand by one and
	// silently re-pairs the whole environment onto the wrong names. With an even
	// number of empty values the pairs realign and the parse looks correct,
	// which is exactly how this survived a local run and failed in CI.
	//
	// Only the trailing element is dropped, and only because the shim writes a
	// NUL after every field, so a well-formed log always ends with one.
	fields := strings.Split(string(data), "\x00")
	if len(fields) > 0 && fields[len(fields)-1] == "" {
		fields = fields[:len(fields)-1]
	}
	for _, field := range fields {
		if field == nativeSandboxInvocationMarker {
			captures = append(captures, nativeSandboxPolicyCapture{Env: map[string]string{}})
			continue
		}
		if len(captures) == 0 {
			continue
		}
		current := &captures[len(captures)-1]
		current.Argv = append(current.Argv, field)
	}
	require.NotEmpty(t, captures, "no backend invocation was recorded")
	for i := range captures {
		for _, pair := range captures[i].operands("--setenv", 2) {
			// The alignment guard. A shifted parse does not look broken — it
			// looks like a complete environment attached to the wrong names,
			// and every assertion built on it becomes a confident false
			// statement about what the sandbox inherits. An environment
			// variable NAME cannot contain a slash or a space, so a key that
			// does is proof the operands moved.
			require.Regexp(t, `^[A-Za-z_][A-Za-z0-9_]*$`, pair[0],
				"parsed %q as an environment variable NAME, which it cannot be; the "+
					"--setenv operands are misaligned and every environment claim from "+
					"this capture would be attached to the wrong variable", pair[0])
			captures[i].Env[pair[0]] = pair[1]
		}
	}
	return captures, shimDir
}

// policyCapture returns the recorded invocation that carries a policy.
//
// Selected by content rather than taken as captures[0]: the shim answers
// `--version` itself, but any other probe form the backend might add would be
// recorded first and silently become "the policy".
func policyCapture(t *testing.T, captures []nativeSandboxPolicyCapture) nativeSandboxPolicyCapture {
	t.Helper()
	for _, capture := range captures {
		if capture.has("--unshare-user") {
			return capture
		}
	}
	t.Fatalf("copilotfixture: none of the %d recorded bwrap invocations carried a "+
		"policy (no --unshare-user)", len(captures))
	return nativeSandboxPolicyCapture{}
}

// redactPolicy renders an argv for a log line with every `--setenv` VALUE
// removed.
//
// This is not tidiness. The pairs are the launching process's own environment,
// which is the very finding this scenario exists to record — and the smoke job
// runs `go test -v` and tees the output into the log of a job in a PUBLIC
// repository. Printing the values verbatim would publish whatever the runner
// happens to carry, which GitHub masks only for secrets it was told about. The
// key NAMES and the count are what the finding needs; the values are what it is
// warning about.
func redactPolicy(argv []string) string {
	var out []string
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--setenv" && i+2 < len(argv) {
			out = append(out, "--setenv", argv[i+1], "<redacted>")
			i += 2
			continue
		}
		out = append(out, argv[i])
	}
	return strings.Join(out, " ")
}

// TestCopilotNativeSandboxGeneratedLinuxPolicy records the policy and asserts
// the four properties that would decide how tclaude could describe it.
func TestCopilotNativeSandboxGeneratedLinuxPolicy(t *testing.T) {
	requireSmoke(t)
	if runtime.GOOS != "linux" {
		t.Skipf("the bubblewrap backend is Linux-only; the darwin Seatbelt profile "+
			"needs a `sandbox-exec` shim this scenario does not implement (GOOS=%s)",
			runtime.GOOS)
	}

	// The credential probe, and the choice of variable NAMES is the whole
	// measurement. `copilot help sandbox` says the sandbox "still inherits your
	// shell environment apart from a fixed blocklist", so a probe named
	// TCL1011_SOMETHING would be guaranteed to pass and would establish nothing
	// about the blocklist — it would measure that an unlisted name is unlisted.
	//
	// These two names are chosen so the pair says something an operator can act
	// on: GITHUB_TOKEN is the credential most likely to be live in a shell that
	// launches Copilot, and OPENAI_API_KEY is the CONTROL that proves the
	// blocklist exists and is doing work. If both were inherited the finding
	// would be "no filtering at all"; if both were dropped it would be "the
	// sandbox scrubs credentials". Measured, it is neither, and the asymmetry is
	// the point.
	//
	// Both values are obviously fake, and the runner scrubs the real ones from
	// the child environment before ExtraEnv is appended, so no genuine
	// credential can reach this run.
	const inheritedVar = "GITHUB_TOKEN"
	const inheritedValue = "tcl1011-not-a-real-token"
	const blockedVar = "OPENAI_API_KEY"
	const blockedValue = "tcl1011-not-a-real-key"

	for _, testCase := range []struct {
		name          string
		allowOutbound bool
	}{
		{"outbound_denied", false},
		{"outbound_allowed", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dirs := newNativeSandboxDirs(t)
			captures, shimDir := captureNativeSandboxPolicy(t, dirs,
				copilotfixture.NativeSandboxSettings{
					Enabled: true, AddCurrentWorkingDirectory: true, AllowBypass: false,
					UserPolicy: copilotfixture.NativeSandboxUserPolicy{
						Filesystem: copilotfixture.NativeSandboxFilesystem{
							DeniedPaths: []string{dirs.Denied},
						},
						Network: copilotfixture.NativeSandboxNetwork{
							AllowOutbound: testCase.allowOutbound, AllowLocalNetwork: true,
						},
					},
				}, []string{
					inheritedVar + "=" + inheritedValue,
					blockedVar + "=" + blockedValue,
				})

			policy := policyCapture(t, captures)
			t.Logf("GENERATED POLICY (%s), %d args, %d --setenv pairs:\n%s",
				testCase.name, len(policy.Argv), len(policy.Env),
				redactPolicy(policy.Argv))

			// 1. The isolation primitives. Recorded rather than merely counted,
			// because "an OS sandbox" is a claim about exactly this list.
			for _, flag := range []string{
				"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
			} {
				assert.True(t, policy.has(flag),
					"the generated policy must unshare %s", flag)
			}

			// 2. Network. This is the one dimension of the policy that is
			// genuinely kernel-enforced end to end on Linux — an unshared
			// network namespace with no interfaces cannot be talked around —
			// and it is measured in BOTH directions so the row means something.
			assert.Equal(t, !testCase.allowOutbound, policy.has("--unshare-net"),
				"network.allowOutbound=%v must decide whether the network namespace is "+
					"unshared", testCase.allowOutbound)

			// 3. Grants and denials, and the mechanism behind each. The
			// workspace is a read-write bind; the DENIED path is not a deny
			// rule at all but an empty tmpfs mounted over it.
			//
			// That distinction is what makes this row worth recording: masking
			// and denial are indistinguishable from outside — every existing
			// scenario asserts only that the host file is absent, which both
			// satisfy — and they are not the same contract. What a masked write
			// looks like to the MODEL is a separate question this scenario does
			// not answer; it needs a host whose namespace can actually start.
			assert.True(t, policy.bindsPath("--bind", 2, dirs.WorkDir),
				"the workspace must be bound read-write")
			assert.True(t, policy.bindsPath("--tmpfs", 1, dirs.Denied),
				"a deniedPaths entry must be masked by an empty tmpfs; if this changes to "+
					"a real deny the model-visible behaviour of a denied write changes with it")

			// The read-only bind set is derived from the LAUNCHING PROCESS'S
			// PATH, which matters to tclaude specifically: whatever PATH it
			// hands a Copilot pane decides part of the sandbox's executable
			// surface. The shim directory is the proof available here — this
			// scenario put it on PATH itself, and the policy bound it.
			assert.True(t, policy.bindsPath("--ro-bind", 2, shimDir),
				"the shim directory is on PATH only because this scenario put it there, "+
					"so a read-only bind of it is what shows the bind set follows PATH")

			// 4. The environment. --clearenv followed by explicit --setenv
			// pairs reads like a scrub and is not one — but it is not a
			// free-for-all either, and the pair of assertions is what pins
			// WHERE the line falls.
			assert.True(t, policy.has("--clearenv"),
				"the policy must clear the environment before repopulating it")
			assert.Equal(t, inheritedValue, policy.Env[inheritedVar],
				"%s must be re-exported into the sandbox. It is the credential most "+
					"likely to be live in a shell that launches Copilot, and it is NOT on "+
					"the blocklist — so the sandbox is a filesystem and network boundary, "+
					"not a credential one, and anything describing it to an operator has "+
					"to say so", inheritedVar)
			assert.NotContains(t, policy.Env, blockedVar,
				"%s must be DROPPED. This is the control: without it the assertion above "+
					"would be consistent with no filtering at all, and the finding is the "+
					"asymmetry — a real blocklist that does not cover %s",
				blockedVar, inheritedVar)
		})
	}
}
