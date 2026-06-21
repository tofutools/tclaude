package agentd_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a Codex spawn with a SHORT task brief. Codex must self-submit a
// first-turn prompt to materialise its conv-id (JOH-205), and that prompt now
// IS the [system: ...] welcome — with the brief inlined — so the agent gets a
// SINGLE greeting turn that looks like the Claude Code launch prompt. No more
// "[tclaude] …" placeholder seed followed by a separate post-connect welcome.
//
// Expected:
//   - the launch seed carries the welcome metadata + the brief inline;
//   - it has NO inbox-message id (Codex has no conv-id at launch) and tells the
//     agent to act, not to run `inbox read`;
//   - the briefing is ALSO saved to the inbox (additive, like Claude Code);
//   - NOTHING is injected over tmux post-connect — the welcome already landed
//     as the seed, and Codex's rename is out-of-band (threads.title).
func TestCodexSpawn_ShortBriefInlinedIntoSeed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	const brief = "Audit the auth module and report back"
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":            "codex-worker",
		"harness":         "codex",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// The briefing is always also saved to the inbox.
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Equal(t, "Startup context", msg.Subject, "inbox briefing subject")
	assert.Contains(t, msg.Body, brief, "inbox copy carries the verbatim brief")

	// The seed (Codex's first-turn launch prompt) carries the inline welcome.
	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "codex launch seed should be recorded")
	assert.Contains(t, prompt, "[system:", "seed opens with the system welcome")
	assert.Contains(t, prompt, brief, "the short brief is inlined into the seed")
	assert.Contains(t, prompt, "tclaude agent", "seed keeps the coordination pointer")
	assert.Contains(t, prompt, "act on the brief", "a task brief tells the agent to act")
	assert.NotContains(t, prompt, "[tclaude]", "the old inert placeholder seed is gone")
	// No conv-id at launch → no inbox-message id, and no `inbox read` round-trip.
	assert.NotContains(t, prompt, "inbox read", "an inlined seed needs no inbox round-trip")
	assert.NotContains(t, prompt, "message #", "Codex has no inbox-message id at launch")

	// Post-connect: the welcome was already delivered via the seed, and Codex's
	// rename is out-of-band — so NOTHING is typed into the pane. Drain the
	// post-init background goroutine first so this isn't racing it.
	agentd.WaitForBackgroundForTest()
	target := spawn.TmuxTarget()
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target {
			t.Fatalf("an inlined-seed Codex spawn must not send-keys post-connect; got %q", sk.Text)
		}
	}

	// The name still resolves — Codex renames via the native title store.
	f.AssertGroupMember("crew", spawn.ConvID, "codex-worker", 3*time.Second)
}

// Scenario: a Codex spawn with NO briefing (no group context, no task brief).
// The seed is still a clean [system: ...] welcome that tells the agent to wait
// — replacing the old "[tclaude] …" placeholder — and nothing is injected
// post-connect.
func TestCodexSpawn_NoBriefingSeedsCleanWaitWelcome(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnHarness("crew", "codex-worker", "codex")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "codex launch seed should be recorded")
	assert.Contains(t, prompt, "[system:", "seed is a clean system welcome")
	assert.Contains(t, prompt, "Wait for the first instruction", "no briefing → tell the agent to wait")
	assert.NotContains(t, prompt, "[tclaude]", "the old inert placeholder seed is gone")

	// No briefing → no inbox message at all.
	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	assert.Empty(t, rows, "a no-briefing spawn gets no inbox message")

	// No post-connect send-keys.
	agentd.WaitForBackgroundForTest()
	target := spawn.TmuxTarget()
	for _, sk := range f.World.Tmux.Sent() {
		if sk.Target == target {
			t.Fatalf("a no-briefing Codex spawn must not send-keys post-connect; got %q", sk.Text)
		}
	}
}

// Scenario: a Codex spawn whose briefing is OVER the inline cap (set tiny). The
// seed can't reference the not-yet-created inbox message, so it's a stand-by
// welcome; the real inbox-pointer welcome is injected post-connect, once the
// inbox row + its id exist — race-safe. (CC presets its conv-id so it inlines
// or points with the id at launch; Codex can't, so the long case degrades to
// this two-step. The common short case is single-turn — see the test above.)
func TestCodexSpawn_LongBriefStandbySeedThenPointerWelcome(t *testing.T) {
	f := newFlow(t)

	// Force the over-cap path with a tiny inline cap.
	tiny := 10
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnInlineMaxChars: &tiny},
	}))

	f.HaveGroup("crew")
	const brief = "Audit the auth module for timing-safe comparison bugs and write a report"
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":            "codex-worker",
		"harness":         "codex",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, brief, "inbox copy carries the verbatim brief")

	// Seed is a stand-by welcome — NOT the brief inline.
	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok, "codex launch seed should be recorded")
	assert.Contains(t, prompt, "[system:", "stand-by seed is a system welcome")
	assert.Contains(t, prompt, "stand by", "stand-by seed tells the agent to wait for the inbox briefing")
	assert.NotContains(t, prompt, "timing-safe", "the long brief must NOT be inlined into the seed")
	assert.NotContains(t, prompt, "[tclaude]", "the old inert placeholder seed is gone")

	// Post-connect: the real inbox-pointer welcome is injected over tmux,
	// pointing the agent at the briefing it can now read.
	f.AssertSentContains(spawn.TmuxTarget(), fmt.Sprintf("inbox read %d", msg.ID), 3*time.Second)
}
