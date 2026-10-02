package geminifixture

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const fixtureModel = "gemini-2.5-flash"

func gemini() *harness.Harness { return harness.MustGet(harness.GeminiName) }

// ask runs one headless turn through tclaude's own Gemini asker argv.
func (w *World) ask(spec harness.AskSpec, extraEnv ...string) Result {
	w.T.Helper()
	spec.Print = true
	if spec.Model == "" {
		spec.Model = fixtureModel
	}
	return w.Run(gemini().Ask.BuildAskArgv(spec), extraEnv...)
}

func (w *World) listedConv(convID string) (found bool, count int) {
	w.T.Helper()
	convs, err := gemini().Convs.ListConvs(w.Project)
	require.NoError(w.T, err)
	for _, conv := range convs {
		if conv.SessionID == convID {
			found = true
		}
	}
	return found, len(convs)
}

// A pinned headless turn lands where tclaude's reader looks, under the id
// tclaude chose, and its usage folds into the context, output and what-if
// cost tclaude reports.
func TestGeminiPinnedAskIsListedAndPriced(t *testing.T) {
	w := NewWorld(t)
	convID := uuid.NewString()

	res := w.ask(harness.AskSpec{SessionID: convID, Prompt: "first question"}, TrustWorkspaceEnv)
	res.RequireOK(t)
	assert.Contains(t, res.Stdout, w.Mock.Reply)
	require.NotEmpty(t, w.Mock.ModelCalls(), "the CLI never reached the mock model")

	path, found, err := harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	require.True(t, found, "no session file holds the pinned id")

	listed, _ := w.listedConv(convID)
	assert.True(t, listed, "the conversation is not listed for its project")
	ref, err := gemini().Convs.Resolve(convID[:8], w.Project, false)
	require.NoError(t, err)
	require.NotNil(t, ref)

	var follower harness.GeminiUsageFollower
	usage, found, err := follower.Read(path)
	require.NoError(t, err)
	require.True(t, found)
	assert.GreaterOrEqual(t, usage.Calls, 1)
	assert.EqualValues(t, MockPromptTokens, usage.ContextTokens)
	assert.Equal(t, fixtureModel, usage.Model)
	// Every recorded call is priced at the gemini-2.5-flash rate card.
	perCall := (float64(MockPromptTokens-MockCachedTokens)*0.30 +
		float64(MockCachedTokens)*0.03 +
		float64(MockOutputTokens+MockThoughtsTokens)*2.50) / 1_000_000
	assert.InDelta(t, perCall*float64(usage.Calls), usage.CostUSD, 1e-9)
}

// `--resume <id>` continues the pinned conversation in the same file, and the
// model sees the earlier turn.
func TestGeminiResumeContinuesThePinnedConversation(t *testing.T) {
	w := NewWorld(t)
	convID := uuid.NewString()
	w.ask(harness.AskSpec{SessionID: convID, Prompt: "first question"}, TrustWorkspaceEnv).RequireOK(t)
	first, _, err := harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)

	w.ask(harness.AskSpec{ResumeID: convID, Prompt: "second question"}, TrustWorkspaceEnv).RequireOK(t)
	calls := w.Mock.ModelCalls()
	assert.Contains(t, calls[len(calls)-1].Body, "first question")

	second, found, err := harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, first, second)
	_, count := w.listedConv(convID)
	assert.Equal(t, 1, count, "a resume must not create a second conversation")
}

// A séance turn forks a copy: the model sees the predecessor's history, the
// predecessor's file is byte-for-byte untouched, and cleanup leaves no extra
// conversation behind.
func TestGeminiSeanceForkLeavesThePredecessorUntouched(t *testing.T) {
	w := NewWorld(t)
	convID := uuid.NewString()
	w.ask(harness.AskSpec{SessionID: convID, Prompt: "the predecessor's question"}, TrustWorkspaceEnv).RequireOK(t)
	path, _, err := harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	resumer, ok := gemini().Ask.(harness.EphemeralResumer)
	require.True(t, ok)
	copyPath, cleanup, err := resumer.PrepareEphemeralResume(convID)
	require.NoError(t, err)
	res := w.ask(harness.AskSpec{
		ResumeID: convID, ResumeFile: copyPath, Ephemeral: true, Prompt: "séance question",
		LaunchPosture: &harness.SpawnSpec{HarnessBuiltinMode: harness.GeminiSandboxOff, Cwd: w.Project},
	}, TrustWorkspaceEnv)
	res.RequireOK(t)
	calls := w.Mock.ModelCalls()
	assert.Contains(t, calls[len(calls)-1].Body, "the predecessor's question")
	_, beforeCleanup := w.listedConv(convID)
	require.Equal(t, 2, beforeCleanup, "the import did not persist a fork, so cleanup proves nothing")
	cleanup()

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the predecessor's session file changed")
	listed, count := w.listedConv(convID)
	assert.True(t, listed)
	assert.Equal(t, 1, count, "the séance fork was left behind")
	_, err = os.Stat(filepath.Dir(copyPath))
	assert.True(t, os.IsNotExist(err), "the copy was not removed")
}

// A non-interactive run's pinned conversation is removed afterwards.
func TestGeminiRemoveEphemeralSessionDeletesARealRun(t *testing.T) {
	w := NewWorld(t)
	convID := uuid.NewString()
	w.ask(harness.AskSpec{SessionID: convID, Prompt: "throwaway"}, TrustWorkspaceEnv).RequireOK(t)
	_, found, err := harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	require.True(t, found, "nothing to remove")
	remover, ok := gemini().Ask.(harness.EphemeralSessionRemover)
	require.True(t, ok)
	remover.RemoveEphemeralSession(convID)
	_, found, err = harness.LocateGeminiSessionFile(convID)
	require.NoError(t, err)
	assert.False(t, found)
}

// The hooks tclaude installs are accepted by the CLI and fire with the
// payload tclaude's callback reads. The installed callback command is swapped
// for a recorder after install; everything else (events, groups, timeout,
// output sink) is exactly what `tclaude setup` writes.
func TestGeminiInstalledHooksFire(t *testing.T) {
	w := NewWorld(t)
	installer := gemini().Hooks
	require.NoError(t, installer.Install())
	installed, missing, _ := installer.Check()
	require.True(t, installed, "missing: %v", missing)

	events := filepath.Join(w.Home, "events.jsonl")
	settingsPath := filepath.Join(w.Home, ".gemini", "settings.json")
	settings, err := os.ReadFile(settingsPath)
	require.NoError(t, err)
	quoted, _ := json.Marshal(clcommon.HookCallbackCommand)
	// The redirect stays inside sh -c: the installed command's output sink
	// (>/dev/null) would otherwise override it.
	recorder, _ := json.Marshal(`sh -c 'cat >> "$0"; echo >> "$0"' '` + events + `'`)
	require.Contains(t, string(settings), strings.Trim(string(quoted), `"`))
	settings = []byte(strings.ReplaceAll(string(settings),
		strings.Trim(string(quoted), `"`), strings.Trim(string(recorder), `"`)))
	require.NoError(t, os.WriteFile(settingsPath, settings, 0o600))

	// Settings hooks run only in a trusted folder; trust it the way
	// `--trust-dir` does.
	require.NoError(t, harness.EnsureGeminiDirTrustedForLaunch(nil, "", w.Project))
	convID := uuid.NewString()
	w.ask(harness.AskSpec{SessionID: convID, Prompt: "hooked question"}).RequireOK(t)

	file, err := os.Open(events)
	require.NoError(t, err, "no hook fired")
	defer func() { _ = file.Close() }()
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var payload struct {
			SessionID     string `json:"session_id"`
			HookEventName string `json:"hook_event_name"`
			Cwd           string `json:"cwd"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &payload), "hook payload: %s", line)
		assert.Equal(t, convID, payload.SessionID, "event %s", payload.HookEventName)
		seen[payload.HookEventName] = true
	}
	require.NoError(t, scanner.Err())
	// SessionEnd is best effort (the CLI does not wait for it).
	for _, event := range []string{"SessionStart", "BeforeAgent", "AfterAgent"} {
		assert.True(t, seen[event], "%s did not fire; saw %v", event, seen)
	}
}

// Headless Gemini refuses an untrusted folder; the entry tclaude's
// `--trust-dir` records is what the CLI accepts.
func TestGeminiDirTrustSatisfiesTheHeadlessTrustCheck(t *testing.T) {
	w := NewWorld(t)
	refused := w.ask(harness.AskSpec{SessionID: uuid.NewString(), Prompt: "untrusted"})
	require.Error(t, refused.Err, "an untrusted folder ran headless; the trust premise no longer holds")
	assert.Contains(t, refused.Stdout+refused.Stderr, "trusted directory", "refused for another reason")

	require.NoError(t, harness.EnsureGeminiDirTrustedForLaunch(nil, "", w.Project))
	w.ask(harness.AskSpec{SessionID: uuid.NewString(), Prompt: "trusted"}).RequireOK(t)
}

// Every approval mode tclaude renders is accepted by the CLI, as tclaude
// renders it.
func TestGeminiApprovalModesAreAccepted(t *testing.T) {
	w := NewWorld(t)
	for _, mode := range gemini().Approval.Modes() {
		t.Run(mode, func(t *testing.T) {
			argv := gemini().Ask.BuildAskArgv(harness.AskSpec{
				Print: true, Model: fixtureModel, SessionID: uuid.NewString(), Prompt: "mode " + mode,
				LaunchPosture: &harness.SpawnSpec{
					ApprovalPolicy: mode, HarnessBuiltinMode: harness.GeminiSandboxOff, Cwd: w.Project,
				},
			})
			if mode != harness.GeminiApprovalInherit {
				require.Contains(t, argv, "--approval-mode="+mode)
			}
			w.Run(argv, TrustWorkspaceEnv).RequireOK(t)
		})
	}
}
