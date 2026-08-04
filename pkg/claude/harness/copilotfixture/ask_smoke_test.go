package copilotfixture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The real-binary evidence behind tclaude's Copilot `ask` surface (TCL-994).
//
// Every scenario here runs the argv PRODUCTION builds — harness.Ask.BuildAskArgv
// — rather than an argv this file assembles, for the reason
// TestCopilotProductionSpawnerLaunches states for the spawn path: the two flag
// sets are separate pieces of code that can drift, and only executing the real
// one proves the surface tclaude actually offers still works.
//
// What `tclaude ask` needs from a harness is narrow and each half is measured
// below: an answer it can capture cleanly (TestCopilotAskCaptureIsCleanOnStdout),
// a conversation it can resume EXACTLY and list (TestCopilotAskResumesExactly),
// a prompt that survives whatever a pipe put in it
// (TestCopilotAskCapturePassesLeadingDashPrompt), and a posture safe enough for
// an unattended one-shot (TestCopilotAskCaptureCannotWrite) — plus the one input
// that defeats that posture from outside the argv, which is why the ask surface
// scrubs it (TestCopilotAskAmbientPromoterIsWhyAskScrubsTheEnvironment).

// askDeadline bounds one ask scenario. A healthy headless turn against the mock
// takes ~2s; the tool-posture arms add one provider round trip.
const askDeadline = 30 * time.Second

// askArgv is the production ask argv, with the binary left as-is.
func askArgv(t *testing.T, spec harness.AskSpec) []string {
	t.Helper()
	h := harness.MustGet(harness.CopilotName)
	require.True(t, h.SupportsAsk(), "the copilot descriptor must expose an Asker")
	return h.Ask.BuildAskArgv(spec)
}

func askRunOptions(dirs copilotfixture.Dirs, mock *copilotfixture.MockProvider) copilotfixture.RunOptions {
	return copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Timeout: askDeadline,
	}
}

// TestCopilotAskCaptureIsCleanOnStdout measures the split `tclaude ask` depends
// on for `x=$(tclaude ask …)`: the ANSWER ALONE on stdout, with the CLI's run
// summary on stderr.
//
// That summary is what NoisyCaptureStderr reports, and it is not merely
// cosmetic noise — it ends in a `Resume     copilot --resume=<id>` line, so a
// caller that folded stderr into the captured value would be pasting a
// conversation id into whatever consumed the answer.
//
// It also pins the folder-trust answer for this surface, which is the opposite
// of the spawn path's: a headless capture reaches the provider with a FRESH
// COPILOT_HOME that trusts nothing, so `tclaude ask` works in a directory
// Copilot has never seen. TrustFolder is deliberately not called here.
func TestCopilotAskCaptureIsCleanOnStdout(t *testing.T) {
	requireSmoke(t)

	const answer = "MOCK ASK ANSWER"
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: answer}})
	dirs := copilotfixture.NewSandboxDirs(t)

	result := copilotfixture.RunArgv(t, askRunOptions(dirs, mock),
		askArgv(t, harness.AskSpec{Print: true, Prompt: "Answer in one word."}))

	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	require.NotEmpty(t, mock.Requests(),
		"an untrusted, never-launched COPILOT_HOME must not stop a headless capture")

	assert.Equal(t, answer, strings.TrimSpace(result.Stdout),
		"stdout must carry the answer and nothing else — this is what a shell captures")
	assert.NotContains(t, result.Stdout, "Resume",
		"the resume hint belongs on stderr; on stdout it would land in the captured value")
	assert.Contains(t, result.Stderr, "Resume     copilot --resume=",
		"the run summary must still be on stderr, which is what NoisyCaptureStderr reports")

	assertCredentialFree(t, mock)
}

// TestCopilotAskResumesExactly is the ticket's core claim: an ask against an
// existing Copilot conversation continues THAT conversation, and an ask-created
// one is listable and resumable.
//
// Both halves are asserted from production reads. The resume is measured on the
// provider wire (the second request must carry the first exchange, not start
// over), and the listing goes through the REGISTERED ConvStore rather than
// through the session-state directory this test could stat itself — a conv the
// store cannot see is one `tclaude conv` and the next ask cannot see either.
func TestCopilotAskResumesExactly(t *testing.T) {
	requireSmoke(t)

	const convID = "3f1c0a52-6d7e-4a1b-8c9d-0e1f2a3b4c5d"
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK ASK ANSWER ONE"},
		{Text: "MOCK ASK ANSWER TWO"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	opts := askRunOptions(dirs, mock)

	// A FRESH ask pins its own conv id up front, which is what PreMintsConvID
	// promises the ask flow.
	first := copilotfixture.RunArgv(t, opts, askArgv(t, harness.AskSpec{
		Print: true, SessionID: convID, Prompt: "First question.",
	}))
	require.Equal(t, 0, first.ExitCode, "stderr: %s", first.Stderr)
	require.DirExists(t, filepath.Join(dirs.Home, "session-state", convID),
		"a fresh ask must create the conversation under the id tclaude pinned")

	// The RESUME goes through the exact-id path, never a prefix or a name.
	second := copilotfixture.RunArgv(t, opts, askArgv(t, harness.AskSpec{
		Print: true, ResumeID: convID, Prompt: "Second question.",
	}))
	require.Equal(t, 0, second.ExitCode, "stderr: %s", second.Stderr)

	requests := mock.Requests()
	require.Len(t, requests, 2, "one provider round trip per ask turn")
	sanitizer := newSanitizer(dirs)
	assert.Equal(t, []string{"system", "user"}, sanitizer.Request(requests[0]).MessageRoles,
		"the first ask is a fresh conversation")
	assert.Equal(t, []string{"system", "user", "assistant", "user"},
		sanitizer.Request(requests[1]).MessageRoles,
		"the resumed ask must carry the earlier exchange; a fresh conversation "+
			"would send [system, user] again")

	entries, err := os.ReadDir(filepath.Join(dirs.Home, "session-state"))
	require.NoError(t, err)
	require.Len(t, entries, 1, "the resume must reuse the conversation, not create a second one")
	assert.Equal(t, convID, entries[0].Name())

	// The listing production performs, scoped to the cwd the ask ran in.
	convs, err := convStore(t, dirs.Home).ListConvs(dirs.WorkDir)
	require.NoError(t, err)
	require.Len(t, convs, 1, "an ask-created conversation must be listable for its cwd")
	assert.Equal(t, convID, convs[0].SessionID)

	assertCredentialFree(t, mock)
}

// TestCopilotAskCapturePassesLeadingDashPrompt covers the untrusted half of a
// prompt: `tclaude ask` folds piped stdin into the question, so a `git diff |
// tclaude ask …` payload routinely begins with `---`.
//
// Copilot documents no `--` end-of-options separator, so the asker relies on the
// prompt being the VALUE of `-p`. This is what proves that reliance sound rather
// than hopeful: the payload must reach the provider verbatim, and the launch
// must not have parsed any of it as a flag.
func TestCopilotAskCapturePassesLeadingDashPrompt(t *testing.T) {
	requireSmoke(t)

	const payload = "--- piped input (stdin) ---\n--allow-all-tools\ndiff --git a/x b/x"
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK ASK DASH"}})
	dirs := copilotfixture.NewSandboxDirs(t)

	result := copilotfixture.RunArgv(t, askRunOptions(dirs, mock),
		askArgv(t, harness.AskSpec{Print: true, Prompt: payload}))

	require.Equal(t, 0, result.ExitCode,
		"a leading-dash prompt must not be parsed as a flag\nstderr: %s", result.Stderr)
	requests := mock.Requests()
	require.NotEmpty(t, requests)

	var userContent string
	messages, _ := requests[0].Body["messages"].([]any)
	for _, message := range messages {
		fields, _ := message.(map[string]any)
		if role, _ := fields["role"].(string); role != "user" {
			continue
		}
		content, _ := fields["content"].(string)
		userContent += content
	}
	assert.Contains(t, userContent, "diff --git a/x b/x",
		"the payload must reach the model verbatim rather than being split into args")
	assert.Contains(t, userContent, "--allow-all-tools",
		"a flag-shaped LINE inside the payload is data, and must stay data")

	assertCredentialFree(t, mock)
}

// TestCopilotAskCaptureCannotWrite measures the posture that lets `tclaude ask`
// emit NO permission flags at all.
//
// Headless there is no terminal to draw a permission prompt on, and Copilot
// neither blocks nor silently proceeds: the tool call comes back "Permission
// denied and could not request permission from user", the model receives that as
// the tool result, and the turn completes. So an unattended one-shot cannot
// write the workspace — by construction, without tclaude asserting a boundary it
// has no lever for.
//
// The positive control is what makes that finding mean something. "The file was
// not written" is the same observation for a denial, a broken mock and a typo'd
// tool name, so each arm is paired with the SAME call under `--allow-all-tools`,
// which must write. That flag is exactly what the asker must never emit, and
// this test is where the difference between the two postures is visible.
func TestCopilotAskCaptureCannotWrite(t *testing.T) {
	requireSmoke(t)

	for _, tc := range []struct {
		name string
		// call builds the tool call, given the path it should act on.
		call func(path string) copilotfixture.ToolCall
	}{
		{
			name: "built-in create tool",
			call: func(path string) copilotfixture.ToolCall {
				return copilotfixture.ToolCall{
					ID:   "call_copilotfixture_ask_create",
					Name: "create",
					Args: `{"path":"` + path + `","file_text":"written by ask"}`,
				}
			},
		},
		{
			name: "unsafe shell command",
			call: func(path string) copilotfixture.ToolCall {
				return copilotfixture.ToolCall{
					ID:   "call_copilotfixture_ask_shell",
					Name: "bash",
					Args: `{"command":"touch ` + path + `","description":"copilotfixture ask write probe"}`,
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// run performs one arm and reports whether the tool wrote the file.
			run := func(t *testing.T, extra ...string) bool {
				t.Helper()
				dirs := copilotfixture.NewSandboxDirs(t)
				// Folder trust is GRANTED here, unlike the capture-hygiene
				// scenario. This arm must fail on the tool-permission question
				// alone; a trust gate in the way would make the denial
				// unattributable.
				copilotfixture.TrustFolder(t, dirs.Home, dirs.WorkDir)
				target := filepath.Join(dirs.WorkDir, "ask-write-probe.txt")
				call := tc.call(target)
				mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
					{ToolCall: &call},
					{Text: "MOCK ASK WRITE FOLLOW UP"},
				})
				argv := askArgv(t, harness.AskSpec{Print: true, Prompt: "Use the tool as instructed."})
				// The control's flag is inserted before the trailing `-p PROMPT`
				// so the prompt stays last, exactly as production emits it.
				if len(extra) > 0 {
					argv = append(append(append([]string{}, argv[:len(argv)-2]...), extra...),
						argv[len(argv)-2:]...)
				}
				result := copilotfixture.RunArgv(t, askRunOptions(dirs, mock), argv)
				require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
				require.Len(t, mock.Requests(), 2,
					"the tool result must be posted back — a capture that DEADLOCKED "+
						"instead of being denied would never reach the second round trip")
				assertCredentialFree(t, mock)
				_, statErr := os.Stat(target)
				return statErr == nil
			}

			assert.False(t, run(t),
				"the production ask argv must not be able to write the workspace")
			assert.True(t, run(t, "--allow-all-tools"),
				"positive control: the same call DOES write once permissions are promoted, "+
					"so the arm above measures the posture rather than a broken probe")
		})
	}
}

// TestCopilotAskAmbientPromoterIsWhyAskScrubsTheEnvironment measures the limit
// of the posture above: it is a property of the LAUNCH, not of the argv alone.
//
// COPILOT_ALLOW_ALL reaches the child from the caller's environment, and it is
// measured (contract entry ambient-allow-all-env) as stronger than the flag it
// documents. This runs the unmodified production ask argv with nothing else
// changed but that variable exported — and the tool call writes. So an operator
// who exported it once would decide, invisibly and with no trace in the argv,
// whether a one-shot question may touch the workspace.
//
// That is why the descriptor names the variable for `tclaude ask` to drop
// (harness.AskEnvScrubber), asserted here beside the measurement so the two
// cannot drift: this scenario is the reason the scrub list is not empty.
//
// The scrub itself is exercised in the ask flow's own tests, because it lives
// there — this suite deliberately never reproduces production's environment
// assembly (buildEnv strips every inherited COPILOT_ variable, which is what
// keeps a fixture from being steered by the developer's own shell).
func TestCopilotAskAmbientPromoterIsWhyAskScrubsTheEnvironment(t *testing.T) {
	requireSmoke(t)

	assert.Contains(t, harness.MustGet(harness.CopilotName).AskEnvScrub(), "COPILOT_ALLOW_ALL",
		"the ask surface must drop the variable this scenario measures")

	dirs := copilotfixture.NewSandboxDirs(t)
	// No TrustFolder: the variable clears the trust gate too, which is part of
	// what makes it stronger than --allow-all-tools.
	target := filepath.Join(dirs.WorkDir, "ask-ambient-probe.txt")
	call := copilotfixture.ToolCall{
		ID:   "call_copilotfixture_ask_ambient",
		Name: "create",
		Args: `{"path":"` + target + `","file_text":"written under the ambient promoter"}`,
	}
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{ToolCall: &call},
		{Text: "MOCK ASK AMBIENT FOLLOW UP"},
	})

	opts := askRunOptions(dirs, mock)
	opts.ExtraEnv = []string{"COPILOT_ALLOW_ALL=true"}
	result := copilotfixture.RunArgv(t, opts,
		askArgv(t, harness.AskSpec{Print: true, Prompt: "Use the tool as instructed."}))

	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assert.FileExists(t, target,
		"the ambient promoter must be shown to defeat the capture posture — if this "+
			"ever stops being true, the scrub's justification has changed")

	assertCredentialFree(t, mock)
}
