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

// Scenario: a human spawns a new agent from the dashboard's spawn modal
// and fills in BOTH the short "Descr" field and the longer "Initial msg"
// field — the two were split precisely so a long task brief doesn't have
// to be smuggled in via the description — AND the brief is too long to
// inline into the first turn (over the inline cap, set tiny here so a
// modest brief deterministically exercises the over-threshold path).
//
// Expected:
//   - The initial message is delivered to the new agent's INBOX as the
//     "Startup context" agent_messages row — not typed into its tmux
//     pane. The pane stays free of CC's input-size limit; newlines
//     survive (see TestSpawn_InitialMessageMultiLinePreserved).
//   - Because it's over the inline cap, the welcome line POINTS the agent
//     at that inbox message by id rather than carrying the brief inline.
//   - The long brief never lands in the pane as typed keystrokes, nor in
//     the launch prompt.
//   - The group member's stored `descr` — what the dashboard's
//     description column renders — is the SHORT descr, never the long
//     initial message.
func TestSpawn_InitialMessageDeliveredToInbox(t *testing.T) {
	f := newFlow(t)

	// Force the over-threshold (pointer) path with a tiny inline cap, so a
	// modest brief routes to the inbox rather than inlining. config.Load reads
	// it fresh on the spawn path and newFlow points HOME at this test's temp
	// dir, so this governs this spawn only.
	tiny := 10
	require.NoError(t, config.Save(&config.Config{
		Agent: &config.AgentConfig{SpawnInlineMaxChars: &tiny},
	}))

	f.HaveGroup("alpha")

	const shortDescr = "auth reviewer"
	const initialMessage = "Review the auth module for timing-safe comparison bugs and write a short report"

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"descr":           shortDescr,
		"initial_message": initialMessage,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn: status=%d body=%s", spawn.Code, spawn.Raw)
	}

	// The initial message rode in via the inbox: the handler inserts the
	// "Startup context" agent_messages row synchronously, so it is
	// already present by the time the spawn response returns.
	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "spawned agent should have exactly one inbox message (the briefing)")
	msg := rows[0]
	assert.Equal(t, "Startup context", msg.Subject, "inbox message subject")
	assert.Equal(t, spawn.ConvID, msg.ToConv, "inbox message addressed to the new agent")
	assert.Contains(t, msg.Body, initialMessage, "the briefing must carry the verbatim task brief")
	assert.Contains(t, msg.Body, "Your task brief:", "the task-brief section header")

	// The agent is named + greeted via launch args (`claude --name <prompt>`),
	// not tmux injection. Over the inline cap, the welcome points the agent at
	// the inbox message by id — identical content, delivered more efficiently.
	f.AssertSpawnName(spawn.ConvID, "worker", 5*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID, fmt.Sprintf("inbox read %d", msg.ID), 5*time.Second)

	// The brief must NOT be typed into the pane NOR baked into the launch
	// prompt — that was the whole point of routing it through the inbox. The
	// launch-enrollment path injects nothing over tmux at all.
	if sent := f.World.Tmux.Sent(); len(sent) != 0 {
		t.Fatalf("launch-enrollment spawn must not send-keys; got %+v", sent)
	}
	if prompt, _ := f.World.SpawnInitialPrompt(spawn.ConvID); strings.Contains(prompt, "timing-safe") {
		t.Fatalf("the briefing must reach the agent via the inbox, never in the launch prompt; got %q", prompt)
	}

	// The contact surface a human reads: `tclaude agent groups members`.
	// The descr column must show the short label only — the whole point
	// of the split is that the long brief never lands here.
	members := f.ListGroupMembers("alpha")
	var found bool
	for _, m := range members {
		if m.ConvID != spawn.ConvID {
			continue
		}
		found = true
		assert.Equal(t, shortDescr, m.Descr,
			"member descr should be the short label, not the initial message")
		assert.NotContains(t, m.Descr, "timing-safe",
			"the initial message must never leak into the dashboard descr column")
	}
	assert.True(t, found, "spawned conv %s should be a member of alpha", spawn.ConvID)
}

// Scenario: a caller (dashboard / CLI / agent API) posts an
// initial_message that spans multiple lines — a real task brief with
// paragraphs, a bullet list, whatever.
//
// Expected: the daemon accepts it (newlines are no longer rejected,
// because the brief is delivered to the inbox rather than typed into
// the pane) and the stored message body preserves the newlines exactly.
func TestSpawn_InitialMessageMultiLinePreserved(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const initialMessage = "Task: refactor the auth module.\n\nSteps:\n- audit timing-safe comparisons\n- write a short report\n\nReport back when done."

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": initialMessage,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn with multi-line initial_message: status=%d body=%s, want 200",
			spawn.Code, spawn.Raw)
	}

	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "spawned agent should have exactly one inbox message")
	// Contains is an exact-substring match — newlines included — so this
	// alone proves the multi-line brief survived the round-trip verbatim.
	assert.Contains(t, rows[0].Body, initialMessage,
		"the multi-line brief must survive verbatim — newlines and all")
}

// Scenario: a caller posts an initial_message carrying a genuinely
// dangerous control character (a NUL byte) that would corrupt an
// `inbox read` terminal render.
//
// Expected: 400 invalid_initial_message. Newlines and tabs are fine
// now, but NUL / escape / carriage-return are still rejected.
func TestSpawn_InitialMessageRejectsControlChars(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": "line one\x00line two",
	})
	if spawn.Code != http.StatusBadRequest {
		t.Fatalf("spawn with NUL initial_message: status=%d body=%s, want 400",
			spawn.Code, spawn.Raw)
	}
	assert.True(t, strings.Contains(string(spawn.Raw), "invalid_initial_message"),
		"error body should name the invalid_initial_message code, got %s", spawn.Raw)
}

// Scenario: a caller posts a genuinely large task brief — well over the
// retired 4096-byte cap but under the current 16384-byte cap. Detailed
// multi-paragraph briefs are exactly what the bump was for.
//
// Expected: the daemon accepts it (the brief rides in the inbox, a
// SQLite row, never a tmux pane — so the old 4096 cap no longer binds)
// and the stored body carries it verbatim.
func TestSpawn_InitialMessageLargeBriefAccepted(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// 8000 bytes: over the retired 4096 cap, under the 16384 cap.
	initialMessage := strings.Repeat("a", 8000)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": initialMessage,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn with 8000-char initial_message: status=%d body=%s, want 200",
			spawn.Code, spawn.Raw)
	}

	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "spawned agent should have one inbox message")
	assert.Contains(t, rows[0].Body, initialMessage,
		"the large brief must survive verbatim into the inbox row")
}

// Scenario: a caller posts an initial_message that exceeds the
// 16384-byte cap.
//
// Expected: 400 invalid_initial_message — the cap is generous but still
// bounded.
func TestSpawn_InitialMessageOverCapRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": strings.Repeat("a", 17000),
	})
	if spawn.Code != http.StatusBadRequest {
		t.Fatalf("spawn with 17000-char initial_message: status=%d body=%s, want 400",
			spawn.Code, spawn.Raw)
	}
	assert.True(t, strings.Contains(string(spawn.Raw), "invalid_initial_message"),
		"error body should name the invalid_initial_message code, got %s", spawn.Raw)
}
