package copilotfixture_test

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// smokeEnv gates every test in this file. Plain `go test ./...` therefore runs
// the pure unit tests in this package but never launches the installed binary:
// a real-CLI run needs an npm install that most developer machines and most CI
// jobs do not have, and silently skipping is better than failing for an absent
// dependency. The CI jobs that DO set it then grep for an explicit PASS so
// that a skip cannot masquerade as coverage.
const smokeEnv = "TCLAUDE_COPILOT_FIXTURE_SMOKE"

// labEnv separates the two things this package is used for, which used to be
// one thing and should not have been.
//
// Unset — REGRESSION mode. The small per-PR set runs: scenarios where
// tclaude's OWN code meets the real CLI (our spawner's argv, our trust seeder,
// our hook file, our conv-store, our ask capture). These assert behaviour, so
// they are true of any Copilot release that still honours the contract, and
// they neither assert the version pin nor diff a golden. That is what lets the
// per-PR job track an npm spec the way every other harness's job does instead
// of being welded to one release.
//
// Set to 1 — LAB mode. The full discovery suite from TCL-970 and friends,
// which measures COPILOT's own behaviour (permission grammar, native sandbox
// flags, wire shape, platform cache layout) rather than ours. That evidence is
// only meaningful against a known release, so this mode asserts the pin and
// diffs the committed goldens. It is what to run when bumping the CLI: the
// resulting diff IS the compatibility evidence.
const labEnv = "TCLAUDE_COPILOT_FIXTURE_LAB"

var update = flag.Bool("update", false, "re-record sanitized Copilot fixtures")

// labMode reports whether this run is pinned discovery rather than per-PR
// regression. Re-recording is inherently a lab act, so -update implies it.
func labMode() bool { return os.Getenv(labEnv) == "1" || *update }

// installedVersion runs `copilot --version` once per test binary.
//
// The pin is asserted before EVERY lab scenario, which is the right guarantee
// and was the wrong implementation: the answer cannot change while a single
// test binary runs — the binary on PATH is fixed for the process — so
// re-launching Node once per test bought nothing and cost half a second each
// time, on a suite with seventy of them.
var installedVersion = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("copilot", "--version").CombinedOutput()
	return string(out), err
})

func requireSmoke(t *testing.T) {
	t.Helper()
	if os.Getenv(smokeEnv) != "1" {
		t.Skipf("set %s=1 with the Copilot CLI installed to run the Copilot fixture smoke",
			smokeEnv)
	}
	if !labMode() {
		// Regression mode: the assertions below this call are behavioural, so
		// pinning the release would only convert an upstream publish into a
		// red PR without telling us anything about our own code.
		return
	}
	// Lab mode: the pin is asserted before any scenario runs, so goldens can
	// never be compared against — or re-recorded from — an unintended release.
	out, err := installedVersion()
	require.NoError(t, err, "running `copilot --version`")
	require.Contains(t, out, copilotfixture.VersionBanner,
		"pinned Copilot CLI version drift: lab fixtures describe %s only. "+
			"Install %s, or bump PinnedCLIVersion and re-record with -update "+
			"so the contract diff gets reviewed.",
		copilotfixture.PinnedCLIVersion, copilotfixture.PinnedCLISpec)
}

// requireLab skips a scenario that is discovery rather than regression.
//
// These measure the CLI's own behaviour against a pinned release, so running
// them per-PR would re-prove a third-party binary that cannot have changed
// since the previous push. They stay in-tree and run from the on-demand lab
// workflow, which is also where a version bump re-reads them.
func requireLab(t *testing.T) {
	t.Helper()
	requireSmoke(t)
	if !labMode() {
		t.Skipf("discovery scenario: set %s=1 (with the pinned %s) to run the lab suite",
			labEnv, copilotfixture.PinnedCLISpec)
	}
}

// requireLabParallel is requireLab for a lab scenario safe to run alongside
// the rest of the suite. See requireSmokeParallel for what "safe" excludes.
func requireLabParallel(t *testing.T) {
	t.Helper()
	requireLab(t)
	t.Parallel()
}

// requireSmokeParallel is requireSmoke for a scenario that may run alongside
// the rest of the suite.
//
// Almost everything here is hermetic by construction and idle by nature: each
// scenario launches its own CLI process against its own disposable
// COPILOT_HOME, cache, working directory and localhost mock, and then spends
// most of its wall clock WAITING — for a terminal to stop redrawing, for a
// deadline to pass, for a prompt that will never be answered. Run one at a
// time that is ten minutes of a CI runner doing nothing.
//
// Two kinds of scenario must NOT use this, and both say so at their own call
// site:
//
//   - Process-global state. The conv-store and hooks scenarios drive tclaude's
//     own production code through t.Setenv and the conv-index database's reset
//     hook. `go test` enforces the t.Setenv half of that rule itself, by
//     panicking rather than by racing.
//   - Scenarios that are ABOUT timing. The soft-exit arms and the in-pane
//     injection scenario type into a live TUI on a schedule, so they encode an
//     assumption about how far the CLI has got — the one assumption contention
//     breaks.
//
// Note what is NOT on that list any more: the golden-comparing scenarios. They
// were sequential for a while because the event goldens pinned where a
// background-task poller's ticks fell relative to a tool's own events, which is
// a race rather than a contract. That is fixed in the projection now (see
// selfPacedEventTypes), so they run concurrently like everything else.
//
// Ordering matters: requireSmoke may skip, and a skipped test never returns to
// call t.Parallel.
func requireSmokeParallel(t *testing.T) {
	t.Helper()
	requireSmoke(t)
	t.Parallel()
}

// TestCopilotVersionPin is the cheapest drift signal: it fails the moment the
// installed CLI stops being the version the goldens describe.
func TestCopilotVersionPin(t *testing.T) {
	requireLabParallel(t)
}

// TestCopilotEffortVocabularyHelp compares the actual pinned CLI help with
// the committed excerpt. The harness catalog test consumes that same excerpt,
// so this is the evidence bridge from Copilot's advertised surface to the
// per-harness values tclaude accepts.
func TestCopilotEffortVocabularyHelp(t *testing.T) {
	requireLabParallel(t)

	live, err := exec.Command("copilot", "--no-auto-update", "--no-color", "--help").CombinedOutput()
	require.NoError(t, err, "running `copilot --help`")
	fixture, err := os.ReadFile(copilotfixture.PinnedEffortHelpFixture)
	require.NoError(t, err, "reading pinned Copilot help fixture")
	require.NoError(t, copilotfixture.ValidateHelpEffortLevels(live, fixture))
}

// TestCopilotModelVocabularyHelp compares the actual pinned CLI config help
// with the committed excerpt. The harness catalog test consumes that same
// excerpt, so this is the evidence bridge from Copilot's documented concrete
// model ids to the dropdown suggestions tclaude exposes.
func TestCopilotModelVocabularyHelp(t *testing.T) {
	requireLabParallel(t)

	live, err := exec.Command("copilot", "--no-auto-update", "--no-color", "help", "config").CombinedOutput()
	require.NoError(t, err, "running `copilot help config`")
	fixture, err := os.ReadFile(copilotfixture.PinnedModelHelpFixture)
	require.NoError(t, err, "reading pinned Copilot help fixture")
	require.NoError(t, copilotfixture.ValidateHelpModels(live, fixture))
}

// TestCopilotCredentialFreeTextTurn is the baseline: a complete streaming text
// turn with no GitHub credential anywhere, proving BYOK activation alone is
// enough to reach a green turn.
func TestCopilotCredentialFreeTextTurn(t *testing.T) {
	requireSmokeParallel(t)

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
	requireSmokeParallel(t)

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
	requireLabParallel(t)

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
	requireSmokeParallel(t)

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

	compareGolden(t, "session_resume", dirs, mock, resumed, sessionID)
}

// TestCopilotReasoningEffortOnResponsesWire pins effort pass-through, on a
// complete successful turn over the OpenAI Responses wire.
//
// The RESPONSES wire is deliberate: on the default completions wire the
// request body carries no effort key at all, so the same assertion built there
// would be vacuously green while looking identical.
//
// The two wires are genuinely different contracts, not a flag toggle. The
// request posts to /responses with input[] plus a separate instructions
// string, and the response is a named-event SSE sequence that terminates at
// response.completed with no [DONE] sentinel. Both halves are exercised here.
func TestCopilotReasoningEffortOnResponsesWire(t *testing.T) {
	requireLabParallel(t)

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK RESPONSES ANSWER"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)
	result := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Wire: copilotfixture.WireResponses,
		Prompt: "Effort probe.", Effort: "xhigh",
	})

	require.Equal(t, 0, result.ExitCode,
		"a responses-wire turn must complete\nstderr: %s", result.Stderr)

	requests := mock.Requests()
	require.NotEmpty(t, requests, "the responses wire must reach the provider")
	obs := newSanitizer(dirs).Request(requests[0])

	assert.Equal(t, "xhigh", obs.ReasoningEffort,
		"--effort must reach the provider verbatim, with no per-model remapping")
	assert.Equal(t, "/v1/responses", obs.Path,
		"the responses wire posts to its own route, not /chat/completions")

	// The CLI accepted the framing, so the answer really came back through it.
	assert.Contains(t, result.Stdout, "MOCK RESPONSES ANSWER")

	assertCredentialFree(t, mock)
	compareGolden(t, "responses_wire_effort", dirs, mock, result)
}

// TestCopilotModelSelection pins --model precedence over COPILOT_MODEL. Unlike
// effort this is observable on the default wire, and it needs no subscription:
// the wire model is simply whatever the CLI was told to ask for.
func TestCopilotModelSelection(t *testing.T) {
	requireLabParallel(t)

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
	requireSmokeParallel(t)

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
	requests := mock.Requests()
	// Zero requests would make the loop below vacuously green, which is the
	// last assertion in this suite that may ever pass by accident.
	require.NotEmpty(t, requests, "no provider request to check for credentials")
	for i, obs := range newSanitizerForAuth().Requests(requests) {
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
	pinnedSessionIDs ...string,
) {
	t.Helper()
	if !labMode() {
		// Regression mode. A golden is a byte-level record of ONE release's
		// wire shape — it is the reason this package needed a version pin at
		// all — so diffing it per-PR would make an upstream publish look like
		// our regression. Every caller of this helper carries its own named
		// behavioural assertions (exit code, credential-free traffic, message
		// roles, x-initiator, the session id being honoured), and those are
		// what the per-PR set is actually for. The byte diff is lab evidence
		// and runs there.
		return
	}
	sanitizer := newSanitizer(dirs)
	// A scenario-chosen id gets its own placeholder, so the golden records
	// that enrollment was honoured instead of flattening it to <uuid>.
	for _, id := range pinnedSessionIDs {
		sanitizer = sanitizer.WithPinnedSessionID(id)
	}
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
	// require, not assert: this runs BEFORE the -update write, so a non-fatal
	// check would flag the leak and still persist the contaminated golden to
	// testdata, where a later `git add` could pick it up.
	for _, forbidden := range []string{dirs.Root, dirs.Home, dirs.Cache, dirs.WorkDir} {
		require.NotContains(t, body, forbidden, "fixture leaked a private path")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		require.NotContains(t, body, home, "fixture leaked the operator's home directory")
	}
	// The fixture must remain small: the system prompt alone is ~26 kB, so a
	// sudden size jump means bulk content stopped being reduced to a digest.
	require.Less(t, len(encoded), 16*1024,
		"fixture grew past the size that indicates raw prompt/tool content crept in")
	require.False(t, strings.Contains(body, "<current_datetime>"),
		"fixture captured injected prompt scaffolding")
	require.False(t, strings.Contains(body, "<environment_context>"),
		"fixture captured host-probed environment scaffolding")

	var probe any
	require.NoError(t, json.Unmarshal(encoded, &probe), "fixture must be valid JSON")
}
