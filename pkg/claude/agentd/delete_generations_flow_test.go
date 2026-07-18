package agentd_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario (JOH-26 PR3d): a multi-generation actor is permanently deleted.
//
// Under the stable-agent-identity model an actor accumulates conversation
// generations — each Claude Code /clear rotates the conv-id while keeping the
// SAME process alive, advancing the actor's live pointer and leaving the prior
// conv around as a past generation with its OWN .jsonl on disk. Before PR3d,
// deleting the live (head) generation tore the actor's identity down but
// ORPHANED every predecessor generation's conv-scoped DB rows AND .jsonl files.
// An orphaned .jsonl is re-indexed by `conv ls` (it resurrects as a plain
// conversation), so the leftover is not benign.
//
// Setup: an agent (group member, permission override) at genesis conv A, then
// /clear'd twice → three generations A → B → C (C is the live head), each with
// a real .jsonl. The predecessors are given conv_index rows, mimicking a
// `conv ls` scan having indexed them, so the DB-row sweep is observable too.
//
// Action: the human force-deletes the live agent (head conv C).
//
// Expected:
//   - EVERY generation's .jsonl is gone — A, B and C — not just the head's.
//   - A `conv ls` re-scan re-discovers none of them (the orphan-reindex guard,
//     applied to all generations).
//   - Every generation's conv_index + sessions DB rows are gone, and the actor
//     row is torn down — nothing resolves to the actor any more.
//   - The group itself survives; only its member (the actor) is gone.
//   - The delete response surfaces the full generation set it reaped.
func TestDelete_MultiGenerationActorSweepsAllGenerations(t *testing.T) {
	f := newFlow(t)

	const (
		group = "alpha"
		convA = "aaaa1111-2222-3333-4444-555555555555" // genesis generation
		label = "spwn-mg-001"
		tmux  = "tclaude-spwn-mg-001"
	)
	cwd := f.TestCwd("mgwork")

	g := f.HaveGroup(group)
	f.HaveAliveSession(convA, label, tmux, cwd)
	// Make it a real agent so the /clear hook migrates identity across the
	// rotation (a plain conversation's /clear is a no-op for identity).
	f.HaveMember(group, convA)
	require.NoError(t, db.GrantAgentPermission(convA, "self.compact", "test"), "grant")

	// Rotate twice: A → B → C. Each /clear keeps the same actor and advances
	// its live pointer; the old conv's .jsonl stays on disk.
	c1 := f.Clear(label)
	require.Equal(t, convA, c1.OldConv, "first /clear rotates off the genesis conv")
	convB := c1.NewConv
	c2 := f.Clear(label)
	require.Equal(t, convB, c2.OldConv, "second /clear rotates off the B generation")
	convC := c2.NewConv

	gens := []string{convA, convB, convC}

	// One actor spans all three generations.
	actor, err := db.AgentIDForConv(convC)
	require.NoError(t, err)
	require.NotEmpty(t, actor, "the live head must resolve to an actor")
	for _, cv := range []string{convA, convB} {
		a, err := db.AgentIDForConv(cv)
		require.NoError(t, err)
		assert.Equal(t, actor, a, "%s should resolve to the same actor as the head", cv)
	}
	linked, err := db.ConvsForAgent(actor)
	require.NoError(t, err)
	assert.ElementsMatch(t, gens, linked, "the actor should span all three generations")

	// Precondition: every generation's .jsonl exists on disk.
	projectDir := convops.GetClaudeProjectPath(cwd)
	jsonl := func(conv string) string { return filepath.Join(projectDir, conv+".jsonl") }
	for _, cv := range gens {
		_, statErr := os.Stat(jsonl(cv))
		require.NoError(t, statErr, "precondition: %s.jsonl should exist before delete", cv)
	}

	// Seed conv_index rows for the predecessors — mimics a `conv ls` scan
	// having indexed their .jsonl — so we can prove the DB rows are swept,
	// not just the files. (The head already has its scan row.)
	for _, cv := range []string{convA, convB} {
		require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: cv, IndexedAt: time.Now()}),
			"seed conv_index for %s", cv)
	}

	// --- Delete the live agent (head conv, force-killing the pane). ---
	resp := f.AsHuman().Delete(convC, true /* force */)
	assert.Equal(t, "deleted", resp.Action, "action")

	// Every generation's .jsonl is gone — no orphan left to be re-indexed.
	for _, cv := range gens {
		_, statErr := os.Stat(jsonl(cv))
		assert.Truef(t, os.IsNotExist(statErr),
			"%s.jsonl should be removed after the actor delete; stat err=%v", cv, statErr)
	}

	// A `conv ls` re-scan finds none of the generations (orphan-reindex guard,
	// applied across the whole generation set — this is the bug class PR3d
	// closes: a predecessor .jsonl lingering and resurfacing as a plain conv).
	for _, cv := range gens {
		f.AssertConvNotListed(cv, cwd)
	}

	// DB: every generation's conv_index + sessions rows are gone, and no
	// generation resolves to any actor any more.
	for _, cv := range gens {
		row, _ := db.GetConvIndex(cv)
		assert.Nilf(t, row, "conv_index row for %s should be gone after the sweep", cv)
		sessRows, _ := db.FindSessionsByConvID(cv)
		assert.Emptyf(t, sessRows, "sessions rows for %s should be gone after the sweep", cv)
		a, err := db.AgentIDForConv(cv)
		require.NoError(t, err)
		assert.Emptyf(t, a, "%s should no longer resolve to any actor", cv)
	}
	gone, err := db.GetAgent(actor)
	require.NoError(t, err)
	assert.Nil(t, gone, "the actor row should be gone after deleting its live generation")

	// The membership is gone (actor torn down); the group survives.
	f.AssertNotGroupMember(group, convC)
	stillThere, err := db.GetAgentGroupByName(group)
	if assert.NoError(t, err) && assert.NotNil(t, stillThere) {
		assert.Equal(t, g.ID, stillThere.ID, "group alpha should still exist")
	}

	// The response surfaced the full generation set it reaped (predecessors
	// included) — not just the named head conv.
	raw := string(resp.Raw)
	assert.Contains(t, raw, convA, "delete response should list predecessor generation A")
	assert.Contains(t, raw, convB, "delete response should list predecessor generation B")
}
