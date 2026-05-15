package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Scenario: a human spawns a new agent from the dashboard's spawn modal
// and fills in BOTH the short "Descr" field and the longer "Initial msg"
// field — the two were split precisely so a long task brief doesn't have
// to be smuggled in via the description.
//
// Expected:
//   - The initial message is delivered to the new pane as its own turn
//     (a separate send-keys submit, after the welcome).
//   - The welcome line tells the agent its instructions follow, instead
//     of telling it to sit idle.
//   - The group member's stored `descr` — what the dashboard's
//     description column renders — is the SHORT descr, never the long
//     initial message.
func TestSpawn_InitialMessageDeliveredSeparateFromDescr(t *testing.T) {
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

	target := spawn.TmuxTarget()
	// /rename lands first, then the welcome, then the initial message —
	// each its own turn. 5s gives slack for the post-init goroutine.
	f.AssertSentContains(target, "/rename worker", 5*time.Second)
	f.AssertSentContains(target, "follow in the next message", 5*time.Second)
	f.AssertSentContains(target, initialMessage, 5*time.Second)

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

// Scenario: a caller (CLI / agent API) posts an initial_message that
// contains a newline.
//
// Expected: the daemon rejects it with 400 invalid_initial_message —
// each newline would land as a premature submit through tmux send-keys,
// fragmenting the prompt into multiple turns. The dashboard collapses
// newlines client-side; this guards the raw wire.
func TestSpawn_InitialMessageRejectsControlChars(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"alias":           "worker",
		"initial_message": "line one\nline two",
	})
	if spawn.Code != http.StatusBadRequest {
		t.Fatalf("spawn with newline initial_message: status=%d body=%s, want 400",
			spawn.Code, spawn.Raw)
	}
	assert.True(t, strings.Contains(string(spawn.Raw), "invalid_initial_message"),
		"error body should name the invalid_initial_message code, got %s", spawn.Raw)
}
