package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: the launch-enrollment "inline a short briefing" optimisation
// (the default). When a freshly-spawned Claude Code agent's briefing fits
// the inline cap, the whole briefing is baked into the launch prompt right
// after the [system: ...] welcome — so the agent acts on its first turn
// without a `tclaude agent inbox read <id>` round-trip.
//
// The brief here is MULTI-LINE on purpose: the launch prompt rides in as a
// single shell-quoted argv positional (not typed into a tmux pane where a
// newline would submit early), so multi-line briefs survive verbatim — the
// very thing the legacy send-keys welcome could not carry.
//
// Expected:
//   - the launch prompt carries the brief verbatim, newlines and all;
//   - it still surfaces the welcome metadata (identity + the `tclaude agent`
//     pointer) so the agent knows how to coordinate;
//   - it notes the inbox copy by id, and that copy still exists in the inbox
//     (inlining is additive — the briefing is always also saved);
//   - nothing was injected over tmux.
func TestSpawn_ShortBriefInlinedIntoLaunchPrompt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const brief = "Task: triage the flaky deploy job.\n\nSteps:\n- read the last 3 CI runs\n- find the retry marker\n- report back"

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// The briefing is always also saved to the inbox.
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Equal(t, "Startup context", msg.Subject, "inbox briefing subject")
	assert.Contains(t, msg.Body, brief, "inbox copy carries the verbatim brief")

	// The launch prompt carries the brief INLINE — newlines and all — plus the
	// welcome metadata and the inbox-copy note. AssertSpawnInitialPrompt does a
	// substring match, so passing the multi-line brief proves the newlines
	// survived the shell-quoted argv round-trip.
	f.AssertSpawnInitialPrompt(spawn.ConvID, brief, 5*time.Second)
	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "launch prompt should be recorded")
	assert.Contains(t, prompt, "[system:", "inlined prompt still opens with the system welcome")
	assert.Contains(t, prompt, "tclaude agent", "inlined welcome keeps the coordination pointer")
	assert.Contains(t, prompt, fmt.Sprintf("message #%d", msg.ID),
		"inlined welcome notes the inbox copy by id")
	assert.Contains(t, prompt, "act on the brief", "a task brief tells the agent to act")
	// The agent does NOT need an inbox round-trip — the brief is already here.
	assert.NotContains(t, prompt, "inbox read",
		"an inlined brief must not also tell the agent to run `inbox read`")

	// Nothing injected over tmux — same invariant as the rest of the
	// launch-enrollment path.
	if sent := f.World.Tmux.Sent(); len(sent) != 0 {
		t.Fatalf("launch-enrollment spawn must not send-keys; got %+v", sent)
	}
}

// Scenario: the operator disables inlining via agent.spawn_inline_max_chars=0.
// Even a tiny brief then keeps the single-line pointer welcome and rides only
// in the inbox — the pre-inlining behaviour, preserved as an escape hatch.
func TestSpawn_InlineDisabledByConfigKeepsPointer(t *testing.T) {
	f := newFlow(t)

	off := 0
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnInlineMaxChars: &off},
	}))

	f.HaveGroup("alpha")
	const brief = "Investigate the flaky deploy job and report back"
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)

	// Pointer welcome: it names the inbox message and the brief itself is NOT in
	// the launch prompt.
	f.AssertSpawnInitialPrompt(spawn.ConvID, fmt.Sprintf("inbox read %d", msg.ID), 5*time.Second)
	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "launch prompt should be recorded")
	assert.NotContains(t, prompt, brief,
		"with inlining disabled the brief must not be baked into the launch prompt")
}

// Scenario: a group-context-only spawn (no per-spawn task brief) whose
// context is short. The context is inlined into the launch prompt and the
// agent is told to read it then WAIT for its first instruction — the
// no-task-brief wording — rather than being told to act.
func TestSpawn_GroupContextOnlyInlinedShort(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const ctx = "Project Phoenix: small commits, tests first, coordinate via #phoenix."
	if _, err := db.SetAgentGroupDefaultContext("alpha", ctx); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	spawn := f.AsHuman().Spawn("alpha", "worker")

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, ctx, "inbox copy carries the group context")

	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "launch prompt should be recorded")
	assert.Contains(t, prompt, ctx, "short group context is inlined into the launch prompt")
	assert.Contains(t, prompt, fmt.Sprintf("message #%d", msg.ID),
		"inlined welcome notes the inbox copy by id")
	assert.Contains(t, prompt, "wait for the first instruction",
		"no task brief → tell the agent to wait, not act")
	assert.NotContains(t, prompt, "act on the brief",
		"a context-only spawn must not tell the agent to act on a (non-existent) brief")
	assert.NotContains(t, strings.ToLower(prompt), "inbox read",
		"an inlined context must not also tell the agent to run `inbox read`")
}
