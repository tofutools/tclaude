package harness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The telemetry parser is exercised through CodexContextTelemetry — the
// exported entry the hook callback uses — driving the real CodexSim rollout
// writer, so the on-disk token_count shape under test is the one the sim
// records for every Codex flow test (testharness v2). External package for
// the same import-cycle reason as the ConvStore tests.

// A turn can emit several token_count events; the LAST one reflects the
// window after the most recent response, and the per-turn last_token_usage
// (not the cumulative total_token_usage) is the occupancy numerator. This
// pins both: a second, larger turn's last_token_usage wins, and Pct is
// computed from it against model_context_window.
func TestCodexContextTelemetry_LastTokenCountWins(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135453"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())

	// Turn 1 — small.
	require.NoError(t, cx.WriteUserInput("hello"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200},
		testharness.CodexTokenUsage{InputTokens: 1000, OutputTokens: 200, TotalTokens: 1200}))

	// Turn 2 — the window has grown. total_token_usage is cumulative
	// (deliberately huge here, > window) to prove it is NOT what occupancy
	// reads; last_token_usage is the resident-window figure.
	require.NoError(t, cx.WriteUserInput("more please"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 400000, OutputTokens: 5000, TotalTokens: 405000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000}))

	snap, ok, err := harness.CodexContextTelemetry(home, id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(49000), snap.TokensInput, "last turn's input")
	assert.Equal(t, int64(1000), snap.TokensOutput, "last turn's output")
	assert.Equal(t, int64(200000), snap.WindowSize)
	assert.InDelta(t, 25.0, snap.Pct, 0.001, "50000/200000 = 25%, from last_token_usage not cumulative")
}

func TestCodexRuntimeTelemetry_CompactionInvalidatesPriorContext(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135458"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 100000, OutputTokens: 5000, TotalTokens: 105000},
		testharness.CodexTokenUsage{InputTokens: 49000, OutputTokens: 1000, TotalTokens: 50000}))
	require.NoError(t, cx.WriteCompacted())

	snap, err := harness.CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.True(t, snap.ContextReset, "compaction is an explicit reset boundary")
	assert.False(t, snap.HasContext, "pre-compaction token_count must not survive the boundary")
	assert.Equal(t, harness.ContextTelemetry{}, snap.Context)

	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 110000, OutputTokens: 6000, TotalTokens: 116000},
		testharness.CodexTokenUsage{InputTokens: 9000, OutputTokens: 1000, TotalTokens: 10000}))
	snap, err = harness.CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.False(t, snap.ContextReset, "real post-compaction telemetry clears the reset marker")
	require.True(t, snap.HasContext)
	assert.Equal(t, int64(9000), snap.Context.TokensInput)
}

// A session that has only taken user input (no model response yet) carries
// no token_count event — ok is false, not an error, so the caller leaves
// the previous snapshot untouched.
func TestCodexContextTelemetry_NoTokenCountYet(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135454"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hello"))

	snap, ok, err := harness.CodexContextTelemetry(home, id)
	require.NoError(t, err)
	assert.False(t, ok, "no token_count ⇒ nothing to persist")
	assert.Zero(t, snap.Pct)
}

// Codex persists collaboration-child lifecycle independently of its hook
// callbacks. Replaying that stream must harvest terminal interrupted IDs while
// leaving normal started/interacted lifecycle to the hook ledger.
func TestCodexRuntimeTelemetry_ReconcilesSubagentActivity(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea41354bb"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	require.NoError(t, cx.Start())

	require.NoError(t, cx.WriteSubagentActivity("child-a", "/root/a", "started"))
	require.NoError(t, cx.WriteSubagentActivity("child-b", "/root/b", "interacted"))
	require.NoError(t, cx.WriteSubagentActivity("child-a", "/root/a", "interrupted"))

	snap, err := harness.CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Contains(t, snap.InterruptedSubagents, "child-a")
	assert.NotContains(t, snap.InterruptedSubagents, "child-b",
		"started/interacted are not terminal facts and must not replace the hook ledger")
	assert.False(t, snap.HasContext, "subagent telemetry works before any token_count exists")

	// A queue-only send_message is interaction but NOT live evidence: it must
	// not resurrect the interrupted child from a stale hook ledger.
	require.NoError(t, cx.WriteSubagentInteraction("child-a", "/root/a", "send_message"))
	snap, err = harness.CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Contains(t, snap.InterruptedSubagents, "child-a", "queue-only message does not resume child")

	// followup_task DOES trigger a turn under the same thread id. The replay is
	// a state machine: this later live evidence clears the earlier terminal fact.
	require.NoError(t, cx.WriteSubagentInteraction("child-a", "/root/a", "followup_task"))
	require.NoError(t, cx.WriteSubagentActivity("child-b", "/root/b", "interrupted"))
	snap, err = harness.CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.NotContains(t, snap.InterruptedSubagents, "child-a", "later activity resumes the same child")
	assert.Contains(t, snap.InterruptedSubagents, "child-b")
}

// Codex writes the effective reasoning effort into turn_context before any
// token_count exists. The dashboard should still be able to show it for a
// just-started session.
func TestCodexEffortLevel_NoTokenCountYet(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea41354aa"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.Effort = "xhigh"
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hello"))

	effort, ok, err := harness.CodexEffortLevel(home, id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "xhigh", effort)
}

// No rollout for the id ⇒ (zero, false, nil): a just-spawned session whose
// rollout has not materialised is the normal "nothing yet" state.
func TestCodexContextTelemetry_NoRollout(t *testing.T) {
	home := codexTestHome(t)
	snap, ok, err := harness.CodexContextTelemetry(home, "019ec004-4250-79b1-9ade-ebaea4135499")
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, harness.ContextTelemetry{}, snap)
}

// When the event omits model_context_window, occupancy is unknowable so Pct
// stays 0, but the absolute token counts still flow through — the dashboard
// can show "X / ? tokens" rather than nothing.
func TestCodexContextTelemetry_MissingWindow(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135455"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.ContextWindow = 0 // ⇒ model_context_window: 0 on the event
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hi"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		testharness.CodexTokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}))

	snap, ok, err := harness.CodexContextTelemetry(home, id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, snap.Pct, "no window ⇒ no percentage")
	assert.Equal(t, int64(100), snap.TokensInput)
	assert.Equal(t, int64(50), snap.TokensOutput)
	assert.Zero(t, snap.WindowSize)
}

// A token_count with a real window but all-zero last_token_usage (no usage
// recorded yet) carries no occupancy signal: it must report ok=false, NOT a
// window-only snapshot. Otherwise db.UpdateContextSnapshot's all-zero guard
// (which requires window==0 too) would let it overwrite a good snapshot.
func TestCodexContextTelemetry_ZeroUsageWithWindowIgnored(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135457"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.ContextWindow = 200000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hi"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{},
		testharness.CodexTokenUsage{}))

	snap, ok, err := harness.CodexContextTelemetry(home, id)
	require.NoError(t, err)
	assert.False(t, ok, "all-zero last_token_usage ⇒ nothing to persist, even with a window")
	assert.Equal(t, harness.ContextTelemetry{}, snap)
}

// IsCodexRolloutPath accepts only well-formed rollout filenames (the guard
// that lets the hook callback trust a transcript_path before reading it),
// rejecting CC transcripts, bare ids, and truncated names.
func TestIsCodexRolloutPath(t *testing.T) {
	const uuid = "019ec004-4250-79b1-9ade-ebaea4135453"
	cases := []struct {
		path string
		want bool
	}{
		{"/h/.codex/sessions/2026/06/13/rollout-2026-06-13T10-06-05-" + uuid + ".jsonl", true},
		{"/h/.codex/sessions/2026/06/13/rollout-2026-06-13T10-06-05-" + uuid + ".jsonl.zst", true},
		{"rollout-2026-06-13T10-06-05-" + uuid + ".jsonl", true},
		{"/h/.claude/projects/enc/" + uuid + ".jsonl", false}, // CC transcript, not a rollout
		{"/h/.codex/sessions/2026/06/13/rollout-2026-06-13T10-06-05-not-a-uuid.jsonl", false},
		{uuid + ".jsonl", false}, // bare id, no rollout- prefix
		{"", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, harness.IsCodexRolloutPath(c.path), "IsCodexRolloutPath(%q)", c.path)
	}
}

// When last_token_usage.total_tokens is absent (0) but the input/output
// split is present, occupancy reconstructs as input+output (input already
// includes the cached prefix, so this never double-counts).
func TestCodexContextTelemetry_TotalTokensFallback(t *testing.T) {
	home := codexTestHome(t)
	const id = "019ec004-4250-79b1-9ade-ebaea4135456"
	cx := testharness.NewCodexSimWithID(t, home, id, "/home/u/proj")
	cx.ContextWindow = 100000
	require.NoError(t, cx.Start())
	require.NoError(t, cx.WriteUserInput("hi"))
	require.NoError(t, cx.WriteTokenCount(
		testharness.CodexTokenUsage{InputTokens: 24000, OutputTokens: 1000, TotalTokens: 0},
		testharness.CodexTokenUsage{InputTokens: 24000, OutputTokens: 1000, TotalTokens: 0}))

	snap, ok, err := harness.CodexContextTelemetry(home, id)
	require.NoError(t, err)
	require.True(t, ok)
	assert.InDelta(t, 25.0, snap.Pct, 0.001, "(24000+1000)/100000 = 25%")
}
