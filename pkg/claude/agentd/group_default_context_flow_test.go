package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// patchGroup is a small helper: PATCH /v1/groups/{name} as the human
// with the given partial-update body, returning the recorder.
func patchGroup(t *testing.T, f *testharness.Flow, name string, body map[string]any) int {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/"+name, body))
	return testharness.Serve(f.Mux, r).Code
}

// soleInboxMessage returns the one agent_messages row addressed to
// convID, failing the test if there isn't exactly one. The spawn
// handler inserts the "Startup context" briefing synchronously, so it
// is already present by the time the spawn response returns.
func soleInboxMessage(t *testing.T, convID string) *db.AgentMessage {
	t.Helper()
	rows, err := db.ListAgentMessagesForConv(convID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	require.Len(t, rows, 1, "expected exactly one inbox message for %s", convID)
	return rows[0]
}

// Scenario: a group carries a startup context. An agent spawned into
// that group — with no include_group_context flag in the request —
// gets the context delivered to its inbox as the "Startup context"
// message, and the welcome points it there.
//
// This is the feature's core promise: PATCH /v1/groups/{name} stores
// default_context, and the spawn folds it into the agent's inbox
// briefing. The Spawn DSL helper sends only {name} (the flag
// omitted), which exercises the opt-in default — every spawn path
// inherits the group context unless it explicitly opts out.
func TestGroupDefaultContext_InjectedOnSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const ctx = "You are part of Project Phoenix. Coordinate via the #phoenix group."
	require.Equal(t, http.StatusOK,
		patchGroup(t, f, "alpha", map[string]any{"default_context": ctx}),
		"PATCH default_context")

	// It landed on the group row.
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, ctx, g.DefaultContext, "group row default_context")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	// The group context was delivered to the agent's inbox.
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Equal(t, "Startup context", msg.Subject, "briefing subject")
	assert.Contains(t, msg.Body, "Project Phoenix", "briefing carries the group context")
	assert.Contains(t, msg.Body, "shared guidance", "group-context section header")

	// The welcome rides in as the launch prompt. This short context fits the
	// default inline cap, so the context is inlined (with the inbox copy noted
	// by id) rather than pointed at — the agent sees it on its first turn.
	f.AssertSpawnInitialPrompt(spawn.ConvID, "spawned by the human", 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID, "Project Phoenix", 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID,
		fmt.Sprintf("message #%d", msg.ID), 10*time.Second)
}

// Scenario: a spawn opts out via include_group_context:false and
// supplies no task brief. The group has a context, but this one agent
// must NOT receive it — the dashboard's "include group default
// context" checkbox, unticked. With nothing to brief, no inbox
// message is created at all.
func TestGroupDefaultContext_OptOutSkipsInjection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultContext("alpha",
		"You are part of Project Phoenix."); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":                  "worker",
		"include_group_context": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// No briefing message — the group context was opted out and there
	// was no task brief, so buildSpawnContextBody had nothing to send.
	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	assert.Empty(t, rows, "opted-out spawn with no brief must get no inbox message")

	// The welcome (launch prompt) tells the agent to wait — no inbox pointer.
	f.AssertSpawnInitialPrompt(spawn.ConvID, "Wait for the first instruction", 10*time.Second)
}

// Scenario: a group with no startup context, spawned into with no task
// brief. There is nothing to brief, so no inbox message is created and
// the welcome tells the agent to wait.
func TestGroupDefaultContext_NoContextNoInjection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	assert.Empty(t, rows, "group with no context + no brief must get no inbox message")

	f.AssertSpawnInitialPrompt(spawn.ConvID, "Wait for the first instruction", 10*time.Second)
}

// Scenario: clearing the context (PATCH default_context:"") removes
// it. A later spawn (no task brief) no longer picks up the old value
// — and gets no inbox message at all.
func TestGroupDefaultContext_PatchClears(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultContext("alpha",
		"You are part of Project Phoenix."); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	require.Equal(t, http.StatusOK,
		patchGroup(t, f, "alpha", map[string]any{"default_context": ""}),
		"PATCH clears default_context")

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultContext, "default_context should be cleared")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	rows, err := db.ListAgentMessagesForConv(spawn.ConvID, 100)
	require.NoError(t, err, "ListAgentMessagesForConv")
	assert.Empty(t, rows, "cleared context must not produce an inbox message")
}

// Scenario: a group created with a startup context in one shot —
// POST /v1/groups carries default_context, applied as a post-create
// update. This is the create-time path the CLI's `groups create
// --context` and the dashboard's create modal both ride.
func TestGroupDefaultContext_CreateWithContext(t *testing.T) {
	f := newFlow(t)

	const ctx = "Onboarding for the squad: read docs/architecture.md first."
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups",
		map[string]any{"name": "beta", "default_context": ctx}))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, ctx, g.DefaultContext, "created group carries the context")
}

// Scenario: a multi-line startup context survives the spawn-time
// inbox delivery intact. The inbox stores the body as plain text, so a
// later line of the context must reach the message verbatim.
func TestGroupDefaultContext_MultilinePreserved(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	ctx := strings.Join([]string{
		"Project Phoenix onboarding.",
		"Repo: ~/phoenix",
		"Follow docs/conventions.md before editing.",
	}, "\n")
	require.Equal(t, http.StatusOK,
		patchGroup(t, f, "alpha", map[string]any{"default_context": ctx}),
		"PATCH multi-line default_context")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	// Contains is an exact-substring match (newlines included), so this
	// proves the whole multi-line block survived the round-trip.
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, ctx, "multi-line group context preserved verbatim")
}

// Scenario: a spawn with BOTH a group startup context AND a per-spawn
// initial message. The two must be merged into a SINGLE inbox briefing
// — group guidance first, task brief second — so the agent has one
// `inbox read` to run, not two.
func TestGroupDefaultContext_MergedWithInitialMessage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const groupCtx = "Project Phoenix conventions: small commits, tests first."
	const brief = "Refactor the auth module and write a short report."
	if _, err := db.SetAgentGroupDefaultContext("alpha", groupCtx); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"initial_message": brief,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// One message, carrying both sections in order.
	msg := soleInboxMessage(t, spawn.ConvID)
	assert.Contains(t, msg.Body, groupCtx, "briefing carries the group context")
	assert.Contains(t, msg.Body, brief, "briefing carries the task brief")
	assert.Less(t, strings.Index(msg.Body, groupCtx), strings.Index(msg.Body, brief),
		"group context should come before the task brief")
	assert.Contains(t, msg.Body, "Your task brief:", "task-brief section header")

	// Both sections are short enough to inline: the launch prompt carries the
	// merged briefing (group context then task brief), notes the inbox copy by
	// id, and tells the agent to act.
	f.AssertSpawnInitialPrompt(spawn.ConvID, groupCtx, 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID, brief, 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID,
		fmt.Sprintf("message #%d", msg.ID), 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID, "act on the brief", 10*time.Second)
}

// Scenario: a context over the size cap is rejected, and nothing is
// persisted. 16 KiB is comfortably more than any reasonable block of
// startup guidance.
func TestGroupDefaultContext_PatchTooLongRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	huge := strings.Repeat("x", 16*1024+1)
	assert.Equal(t, http.StatusBadRequest,
		patchGroup(t, f, "alpha", map[string]any{"default_context": huge}),
		"oversized default_context should 400")

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultContext, "rejected context must not be stored")
}

// Scenario: CRLF / lone-CR line endings are folded to LF on store, so
// the briefing renders consistently regardless of where the human
// authored it.
func TestGroupDefaultContext_PatchNormalizesCRLF(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equal(t, http.StatusOK,
		patchGroup(t, f, "alpha", map[string]any{"default_context": "line1\r\nline2\rline3"}),
		"PATCH CRLF default_context")

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "line1\nline2\nline3", g.DefaultContext, "CRLF folded to LF")
}
