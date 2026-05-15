package agentd_test

import (
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

// Scenario: a group carries a startup context. An agent spawned into
// that group — with no include_group_context flag in the request —
// has the context injected into its pane on startup, right after the
// spawn welcome.
//
// This is the feature's core promise: PATCH /v1/groups/{name} stores
// default_context, and runSpawnPostInit pastes it into the new pane.
// The Spawn DSL helper sends only {alias} (the flag omitted), which
// exercises the opt-in default — every spawn path inherits the group
// context unless it explicitly opts out.
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

	// The welcome always lands first — wait for it so the assertion
	// below isn't racing an un-started post-init goroutine.
	f.AssertSentContains(spawn.TmuxTarget(), "spawned by the human", 10*time.Second)
	// The group startup context is pasted in right after.
	f.AssertSentContains(spawn.TmuxTarget(), "Project Phoenix", 10*time.Second)
}

// Scenario: a spawn opts out via include_group_context:false. The
// group has a context, but this one agent must NOT receive it — the
// dashboard's "include group default context" checkbox, unticked.
func TestGroupDefaultContext_OptOutSkipsInjection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultContext("alpha",
		"You are part of Project Phoenix."); err != nil {
		t.Fatalf("SetAgentGroupDefaultContext: %v", err)
	}

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"alias":                 "worker",
		"include_group_context": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// Post-init still runs — the welcome lands.
	f.AssertSentContains(spawn.TmuxTarget(), "spawned by the human", 10*time.Second)
	// But the group context must not be injected. By the time the
	// welcome has landed, a context paste (if it were going to happen)
	// follows within ~2s; a 5s negative window is a comfortable margin.
	if f.World.Tmux.WaitForSendKeys(spawn.TmuxTarget(), "Project Phoenix", 5*time.Second) {
		t.Fatalf("opted-out spawn must not receive the group context; sent=%+v",
			f.World.Tmux.Sent())
	}
}

// Scenario: a group with no startup context. Spawning into it injects
// only the welcome — the post-init goroutine skips the context step
// entirely (and doesn't crash on the empty string).
func TestGroupDefaultContext_NoContextNoInjection(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	f.AssertSentContains(spawn.TmuxTarget(), "spawned by the human", 10*time.Second)
	// No "startup context" header should ever be pasted.
	if f.World.Tmux.WaitForSendKeys(spawn.TmuxTarget(), "startup context", 5*time.Second) {
		t.Fatalf("group with no context must not inject one; sent=%+v",
			f.World.Tmux.Sent())
	}
}

// Scenario: clearing the context (PATCH default_context:"") removes
// it. A later spawn no longer picks up the old value.
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
	f.AssertSentContains(spawn.TmuxTarget(), "spawned by the human", 10*time.Second)
	if f.World.Tmux.WaitForSendKeys(spawn.TmuxTarget(), "Project Phoenix", 5*time.Second) {
		t.Fatalf("cleared context must not be injected; sent=%+v", f.World.Tmux.Sent())
	}
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
// injection intact. The injector uses bracketed paste precisely so
// embedded newlines don't each submit as a separate turn — a later
// line of the context must reach the pane verbatim.
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
	f.AssertSentContains(spawn.TmuxTarget(), "spawned by the human", 10*time.Second)
	// The third line — only reachable if the newlines survived as
	// literal newlines rather than splitting the paste into turns.
	f.AssertSentContains(spawn.TmuxTarget(), "Follow docs/conventions.md", 10*time.Second)
}

// Scenario: a context over the size cap is rejected, and nothing is
// persisted. The context is bracketed-pasted into a pane; an unbounded
// blob has no business there.
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
// the pasted block renders consistently regardless of where the human
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
