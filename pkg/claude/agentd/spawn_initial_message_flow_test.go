package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a human spawns a new agent from the dashboard's spawn modal
// and fills in BOTH the short "Descr" field and the longer "Initial msg"
// field — the two were split precisely so a long task brief doesn't have
// to be smuggled in via the description.
//
// Expected:
//   - The initial message is delivered to the new agent's INBOX as an
//     agent_messages row — not typed into its tmux pane. This preserves
//     newlines (see TestSpawn_InitialMessageMultiLinePreserved).
//   - The welcome line points the agent at that inbox message by ID,
//     instead of telling it to sit idle.
//   - The long brief never lands in the pane as typed keystrokes.
//   - The group member's stored `descr` — what the dashboard's
//     description column renders — is the SHORT descr, never the long
//     initial message.
func TestSpawn_InitialMessageDeliveredToInbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const shortDescr = "auth reviewer"
	const initialMessage = "Review the auth module for timing-safe comparison bugs and write a short report"

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"descr":           shortDescr,
		"initial_message": initialMessage,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn: status=%d body=%s", spawn.Code, spawn.Raw)
	}

	// The initial message rode in via the inbox: the handler inserts the
	// agent_messages row synchronously, so it is already present.
	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "spawned agent should have exactly one inbox message (the brief)")
	msg := rows[0]
	assert.Equal(t, initialMessage, msg.Body, "inbox message body should be the verbatim brief")
	assert.Equal(t, "Initial context", msg.Subject, "inbox message subject")
	assert.Equal(t, spawn.ConvID, msg.ToConv, "inbox message addressed to the new agent")

	target := spawn.TmuxTarget()
	// /rename lands first, then the welcome — each its own turn. The
	// welcome must point the agent at the inbox message by ID. 5s gives
	// slack for the post-init goroutine.
	f.AssertSentContains(target, "/rename worker", 5*time.Second)
	f.AssertSentContains(target, fmt.Sprintf("inbox read %d", msg.ID), 5*time.Second)

	// The brief must NOT have been typed into the pane — that was the
	// whole point of routing it through the inbox. Once the welcome has
	// landed the post-init goroutine does no further send-keys, so the
	// log is stable to scan here.
	for _, sk := range f.World.Tmux.Sent() {
		assert.NotContains(t, sk.Text, "timing-safe",
			"the initial brief must reach the agent via the inbox, never as pane keystrokes")
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
		"alias":           "worker",
		"initial_message": initialMessage,
	})
	if spawn.Code != http.StatusOK {
		t.Fatalf("spawn with multi-line initial_message: status=%d body=%s, want 200",
			spawn.Code, spawn.Raw)
	}

	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "spawned agent should have exactly one inbox message")
	assert.Equal(t, initialMessage, rows[0].Body,
		"the multi-line brief must survive verbatim — newlines and all")
	assert.Equal(t, strings.Count(initialMessage, "\n"), strings.Count(rows[0].Body, "\n"),
		"every newline in the brief must be preserved")
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
		"alias":           "worker",
		"initial_message": "line one\x00line two",
	})
	if spawn.Code != http.StatusBadRequest {
		t.Fatalf("spawn with NUL initial_message: status=%d body=%s, want 400",
			spawn.Code, spawn.Raw)
	}
	assert.True(t, strings.Contains(string(spawn.Raw), "invalid_initial_message"),
		"error body should name the invalid_initial_message code, got %s", spawn.Raw)
}
