package copilotfixture_test

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// smokeEnv gates every test in this file. Plain `go test ./...` therefore runs
// the pure unit tests in this package but never launches the installed binary:
// a real-CLI run needs a pinned npm install that most developer machines and
// most CI jobs do not have, and silently skipping is better than failing for
// an absent dependency. The dedicated CI job sets it and then greps for an
// explicit PASS so that a skip cannot masquerade as coverage.
const smokeEnv = "TCLAUDE_COPILOT_FIXTURE_SMOKE"

var update = flag.Bool("update", false, "re-record sanitized Copilot fixtures")

func requireSmoke(t *testing.T) {
	t.Helper()
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("set %s=1 with %s installed to run the Copilot fixture smoke",
			smokeEnv, copilotfixture.PinnedCLISpec)
	}
	// The pin is asserted before any scenario runs, so goldens can never be
	// compared against — or re-recorded from — an unintended release.
	out, err := exec.Command("copilot", "--version").CombinedOutput()
	require.NoError(t, err, "running `copilot --version`")
	require.Contains(t, string(out), copilotfixture.VersionBanner,
		"pinned Copilot CLI version drift: fixtures describe %s only. "+
			"Install the pin, or bump PinnedCLIVersion and re-record with -update "+
			"so the contract diff gets reviewed.", copilotfixture.PinnedCLIVersion)
}

// TestCopilotVersionPin is the cheapest drift signal: it fails the moment the
// installed CLI stops being the version the goldens describe.
func TestCopilotVersionPin(t *testing.T) {
	requireSmoke(t)
}

// TestCopilotCredentialFreeTextTurn is the baseline: a complete streaming text
// turn with no GitHub credential anywhere, proving BYOK activation alone is
// enough to reach a green turn.
func TestCopilotCredentialFreeTextTurn(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK STREAMED ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "Reply with the text the provider gives you.",
	})

	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	assertCredentialFree(t, mock)
	compareGolden(t, "text_turn", dirs, mock, result)
}

// TestCopilotToolCallRoundTrip proves the CLI really executes a tool the mock
// asks for and posts the result back as a second provider request. The
// follow-up request is what a harness integration depends on, so its shape —
// roles system/user/assistant/tool, and x-initiator flipping user→agent — is
// the contract under test.
func TestCopilotToolCallRoundTrip(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_1",
			Name: "bash",
			// Deterministic, side-effect-free, and its output is asserted
			// only through the mock's own final text, never scraped.
			Args: `{"command":"echo copilotfixture-tool-ran","description":"fixture probe"}`,
		}},
		{Text: "MOCK TOOL FOLLOW UP"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "Use the bash tool as instructed.",
	})

	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	requests := mock.Requests()
	require.Len(t, requests, 2, "a tool call must produce a follow-up request")

	sanitizer := newSanitizer(dirs)
	obs := sanitizer.Requests(requests)
	assert.Equal(t, []string{"system", "user"}, obs[0].MessageRoles)
	assert.Equal(t, []string{"system", "user", "assistant", "tool"}, obs[1].MessageRoles,
		"the tool result must come back as a tool-role message")
	assert.Equal(t, "user", obs[0].Initiator)
	assert.Equal(t, "agent", obs[1].Initiator,
		"x-initiator is the discriminator between a user turn and a tool follow-up")

	assertCredentialFree(t, mock)
	compareGolden(t, "tool_call", dirs, mock, result)
}

// TestCopilotProviderFailure pins the negative path.
//
// HTTP 400 specifically: Copilot retries on its own schedule, and the observed
// costs are 400 → no retry, 401 → ~3 requests, 500 → 6 requests over ~30s,
// 429 → 6 requests over ~100s. A fixture built on 500 or 429 would spend
// essentially all its runtime in backoff for no extra evidence.
func TestCopilotProviderFailure(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{FailStatus: 400},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
		Prompt:  "This turn cannot succeed.",
	})

	assert.Equal(t, 1, result.ExitCode, "a provider failure must exit non-zero")
	assert.Len(t, mock.Requests(), 1, "HTTP 400 must not be retried")

	event, ok := result.Result()
	require.True(t, ok, "a failed turn must still emit a terminal result event")
	require.NotNil(t, event.ExitCode)
	assert.Equal(t, 1, *event.ExitCode,
		"the result event's exitCode must agree with the process exit code")

	compareGolden(t, "provider_failure", dirs, mock, result)
}

// TestCopilotSessionEnrollmentAndResume proves the two claims tclaude's
// descriptor makes about identity: LaunchEnrollment (a caller-chosen
// --session-id becomes the real session id, so the daemon can enroll an agent
// before the pane starts) and exact resume (--resume=<id> continues that same
// conversation with its history intact).
func TestCopilotSessionEnrollmentAndResume(t *testing.T) {
	requireSmoke(t)

	// A fixed UUID: the whole point is that the CALLER chooses it.
	const sessionID = "11111111-2222-4333-8444-555555555555"

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK FIRST TURN"},
		{Text: "MOCK RESUMED TURN"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)

	fresh := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "First question.", SessionID: sessionID,
	})
	require.Equal(t, 0, fresh.ExitCode, "stderr: %s", fresh.Stderr)
	freshResult, ok := fresh.Result()
	require.True(t, ok)
	assert.Equal(t, sessionID, freshResult.SessionID,
		"a caller-chosen --session-id must become the session's real id")

	// The session's state directory is named by that same id, which is what
	// makes pre-launch enrollment observable on disk.
	assert.DirExists(t, filepath.Join(dirs.Home, "session-state", sessionID))

	resumed := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Second question.", ResumeID: sessionID,
	})
	require.Equal(t, 0, resumed.ExitCode, "stderr: %s", resumed.Stderr)
	resumedResult, ok := resumed.Result()
	require.True(t, ok)
	assert.Equal(t, sessionID, resumedResult.SessionID,
		"--resume=<id> must continue the same conversation, not fork a new one")

	requests := mock.Requests()
	require.Len(t, requests, 2)
	sanitizer := newSanitizer(dirs)
	obs := sanitizer.Requests(requests)
	assert.Equal(t, []string{"system", "user"}, obs[0].MessageRoles)
	assert.Equal(t, []string{"system", "user", "assistant", "user"}, obs[1].MessageRoles,
		"a resumed turn must carry the prior exchange as history")

	compareGolden(t, "session_resume", dirs, mock, resumed)
}

// TestCopilotReasoningEffortOnResponsesWire pins effort pass-through.
//
// It runs on the RESPONSES wire deliberately: on the default completions wire
// the request body carries no effort key at all, so the same assertion built
// there would be vacuously green. The turn is answered with a fast-failing 400
// because only the REQUEST is under test here — the responses-wire response
// framing is not yet characterized, and guessing at it would couple this test
// to an unverified contract.
func TestCopilotReasoningEffortOnResponsesWire(t *testing.T) {
	requireSmoke(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{FailStatus: 400},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Wire: copilotfixture.WireResponses,
		Prompt: "Effort probe.", Effort: "xhigh",
	})

	requests := mock.Requests()
	require.NotEmpty(t, requests, "the responses wire must still reach the provider")
	obs := newSanitizer(dirs).Request(requests[0])
	assert.Equal(t, "xhigh", obs.ReasoningEffort,
		"--effort must reach the provider verbatim, with no per-model remapping")
}

// TestCopilotModelSelection pins --model precedence over COPILOT_MODEL. Unlike
// effort this is observable on the default wire, and it needs no subscription:
// the wire model is simply whatever the CLI was told to ask for.
func TestCopilotModelSelection(t *testing.T) {
	requireSmoke(t)

	const override = "copilotfixture-override-model"
	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{{Text: "MOCK OK"}})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Prompt: "Model probe.", Model: override,
	})

	require.Equal(t, 0, result.ExitCode, "stderr: %s", result.Stderr)
	requests := mock.Requests()
	require.NotEmpty(t, requests)
	assert.Equal(t, override, newSanitizer(dirs).Request(requests[0]).Model,
		"--model must win over COPILOT_MODEL on the wire")
}

// TestCopilotProductionSpawnerLaunches is the harness-evidence arm: it runs
// the string tclaude's REAL spawner produces, rather than the argv the fixture
// runner assembles for its own purposes.
//
// That distinction is the whole value of this test. copilotfixture.Run uses
// the headless `-p` form because it wants JSONL; production uses the
// interactive `-i` form with `--session-id` / `--name` / `--model` because it
// drives a tmux pane. Those two flag sets can drift apart, and only executing
// the spawner's own output proves the launch tclaude actually performs still
// reaches a provider and completes.
func TestCopilotProductionSpawnerLaunches(t *testing.T) {
	requireSmoke(t)

	const sessionID = "22222222-3333-4444-8555-666666666666"

	h, ok := harness.Get(harness.CopilotName)
	require.True(t, ok, "the copilot harness must be registered")
	require.NotNil(t, h.Spawn)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK SPAWNER ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)

	// Built by production code, not by this test.
	commandLine := h.Spawn.BuildCommand(harness.SpawnSpec{
		SessionID:     sessionID,
		Name:          "copilotfixture-session",
		Model:         copilotfixture.MockModel,
		InitialPrompt: "Reply with the text the provider gives you.",
	})
	require.Contains(t, commandLine, "--session-id",
		"sanity: the spawner must still pin the session id for a fresh launch")

	opts := copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(),
	}
	result := copilotfixture.RunShell(t, opts, commandLine)
	require.Equal(t, 0, result.ExitCode,
		"the production spawner command must run headlessly to completion\nstderr: %s",
		result.Stderr)

	// The launch reached the provider, so the flags parsed and BYOK activated.
	require.NotEmpty(t, mock.Requests(), "the spawner launch must reach the provider")

	// --session-id from the production string really pinned the conversation.
	assert.DirExists(t, filepath.Join(dirs.Home, "session-state", sessionID),
		"the spawner's --session-id must create the session under that exact id")

	assertCredentialFree(t, mock)
}

// assertCredentialFree checks the signature of an unauthenticated BYOK run:
// the OpenAI SDK always sends an Authorization header, but with no credential
// behind it. A non-empty bearer means a token leaked in from the environment,
// which would invalidate the whole point of the suite.
func assertCredentialFree(t *testing.T, mock *copilotfixture.MockProvider) {
	t.Helper()
	for i, obs := range newSanitizerForAuth().Requests(mock.Requests()) {
		assert.True(t, obs.AuthorizationEmpty,
			"request %d carried a credential; the fixture path must stay credential-free", i)
	}
}

func newSanitizerForAuth() *copilotfixture.Sanitizer {
	return copilotfixture.NewSanitizer("", "", "")
}

func newSanitizer(d copilotfixture.Dirs) *copilotfixture.Sanitizer {
	return copilotfixture.NewSanitizer(d.Home, d.Cache, d.WorkDir)
}

// scenarioFixture is the committed shape of one scenario: sanitized provider
// traffic plus the sanitized event stream. Deliberately free of raw prompts,
// tool schemas, absolute paths, UUIDs and timestamps.
type scenarioFixture struct {
	CLIVersion string                              `json:"cliVersion"`
	Requests   []copilotfixture.RequestObservation `json:"requests"`
	Events     copilotfixture.EventObservation     `json:"events"`
}

func compareGolden(
	t *testing.T,
	name string,
	dirs copilotfixture.Dirs,
	mock *copilotfixture.MockProvider,
	result copilotfixture.RunResult,
) {
	t.Helper()
	sanitizer := newSanitizer(dirs)
	got := scenarioFixture{
		CLIVersion: copilotfixture.PinnedCLIVersion,
		Requests:   sanitizer.Requests(mock.Requests()),
		Events:     sanitizer.Events(result),
	}
	encoded, err := copilotfixture.Marshal(got)
	require.NoError(t, err)

	// Guardrail against the failure mode this suite most needs to avoid:
	// a future field quietly reintroducing prompt text or a private path.
	assertNoLeakedSecrets(t, encoded, dirs)

	path := filepath.Join("testdata", copilotfixture.PinnedCLIVersion, name+".json")
	if *update {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, encoded, 0o644))
		t.Logf("re-recorded %s", path)
		return
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err,
		"missing golden %s; re-record with `go test -run %s -update`", path, t.Name())
	assert.JSONEq(t, string(want), string(encoded),
		"Copilot contract drift in %s. Review the diff as compatibility evidence, "+
			"then re-record with -update if the change is intended.", path)
}

// assertNoLeakedSecrets is a committed-content check, not a behavior check: it
// fails if a fixture is about to carry a machine-specific path or an
// unnormalized identifier.
func assertNoLeakedSecrets(t *testing.T, encoded []byte, dirs copilotfixture.Dirs) {
	t.Helper()
	body := string(encoded)
	for _, forbidden := range []string{dirs.Root, dirs.Home, dirs.Cache, dirs.WorkDir} {
		assert.NotContains(t, body, forbidden, "fixture leaked a private path")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		assert.NotContains(t, body, home, "fixture leaked the operator's home directory")
	}
	// The fixture must remain small: the system prompt alone is ~26 kB, so a
	// sudden size jump means bulk content stopped being reduced to a digest.
	assert.Less(t, len(encoded), 16*1024,
		"fixture grew past the size that indicates raw prompt/tool content crept in")
	assert.False(t, strings.Contains(body, "<current_datetime>"),
		"fixture captured injected prompt scaffolding")

	var probe any
	require.NoError(t, json.Unmarshal(encoded, &probe), "fixture must be valid JSON")
}
