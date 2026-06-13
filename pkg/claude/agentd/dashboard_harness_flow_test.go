package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// findDashHarness returns the catalog entry for a harness name, or nil.
func findDashHarness(snap dashSnapshot, name string) *dashHarness {
	for i := range snap.Harnesses {
		if snap.Harnesses[i].Name == name {
			return &snap.Harnesses[i]
		}
	}
	return nil
}

// Scenario: the spawn dialog drives its harness selector + per-harness
// model/effort/sandbox menus off /api/snapshot's `harnesses` catalog
// (JOH-162). The catalog must list both spawnable harnesses with the right
// menu values and capability flags — in particular the subtle pair the
// per-row controls gate on: Codex CAN rename (via its ConvStore, even with
// no in-pane /rename) but CANNOT compact, and it alone takes a launch
// sandbox.
func TestDashboardSnapshot_HarnessCatalog(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	_ = f // catalog is registry-derived; no agents needed

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	claude := findDashHarness(snap, "claude")
	require.NotNil(t, claude, "catalog missing claude; have %+v", snap.Harnesses)
	assert.Equal(t, "Claude Code", claude.DisplayName)
	assert.NotEmpty(t, claude.Models, "claude offers a curated model list")
	assert.NotEmpty(t, claude.EffortLevels, "claude offers effort levels")
	assert.True(t, claude.CanRename, "claude renames in-pane")
	assert.True(t, claude.CanCompact, "claude compacts in-pane")
	assert.False(t, claude.CanSandbox, "claude has no launch sandbox flag")
	assert.Empty(t, claude.SandboxModes, "claude exposes no sandbox modes")

	codex := findDashHarness(snap, "codex")
	require.NotNil(t, codex, "catalog missing codex; have %+v", snap.Harnesses)
	assert.Equal(t, "Codex CLI", codex.DisplayName)
	// Codex curates no model list (its set changes per release; validated
	// server-side), so the dialog falls back to a free-text model entry.
	assert.Empty(t, codex.Models, "codex curates no model list")
	assert.NotNil(t, codex.Models, "codex models is [] not null for JS .map safety")
	assert.NotEmpty(t, codex.EffortLevels, "codex offers effort/reasoning levels")
	assert.True(t, codex.CanRename, "codex renames out-of-band via ConvStore — must stay renameable")
	assert.False(t, codex.CanCompact, "codex has no compaction command")
	assert.True(t, codex.CanSandbox, "codex takes a launch sandbox flag")
	assert.Equal(t, []string{"read-only", "workspace-write", "danger-full-access"}, codex.SandboxModes)
	assert.Equal(t, "workspace-write", codex.DefaultSandbox, "secure default pre-selected")
}

// Scenario: a per-agent harness + sandbox badge needs the snapshot to
// surface each agent's harness and (Codex) launch sandbox mode off its
// sessions row. A Codex agent shows harness=codex + its sandbox; a Claude
// Code agent shows harness=claude + no sandbox. Both appear on the Agents[]
// roster and the group Members[] row (the two places the badges draw).
func TestDashboardSnapshot_PerAgentHarnessAndSandbox(t *testing.T) {
	const codexConv = "cdx1-1111-2222-3333-4444"
	const codexLabel = "spwn-cdx1"
	const ccConv = "ccx1-1111-2222-3333-4444"
	const ccLabel = "spwn-ccx1"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("mixed")

	// A live Codex agent. HaveAliveCodexSession tags the row harness=codex;
	// overwrite it (same row id → UPSERT, tmux unchanged so it stays alive)
	// to also stamp the launch sandbox the daemon spawn would have recorded.
	f.HaveAliveCodexSession(codexConv, codexLabel, "tmux-cdx1", "/tmp/cdx1")
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          codexLabel,
		TmuxSession: "tmux-cdx1",
		ConvID:      codexConv,
		Cwd:         "/tmp/cdx1",
		Status:      "running",
		Harness:     "codex",
		SandboxMode: "workspace-write",
	}), "stamp codex sandbox mode")
	f.HaveMember("mixed", codexConv)

	// A live Claude Code agent in the same group (mixed-harness group).
	f.HaveAliveSession(ccConv, ccLabel, "tmux-ccx1", "/tmp/ccx1")
	f.HaveMember("mixed", ccConv)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	// Codex agent: harness + sandbox surface on both surfaces.
	codexAgent := findDashAgent(snap, codexConv)
	require.NotNil(t, codexAgent, "codex agent missing from Agents[]")
	assert.Equal(t, "codex", codexAgent.State.Harness, "Agents[] codex harness")
	assert.Equal(t, "workspace-write", codexAgent.State.SandboxMode, "Agents[] codex sandbox")

	codexMember := findDashMember(snap, "mixed", codexConv)
	require.NotNil(t, codexMember, "codex agent missing from group members")
	assert.Equal(t, "codex", codexMember.State.Harness, "Members[] codex harness")
	assert.Equal(t, "workspace-write", codexMember.State.SandboxMode, "Members[] codex sandbox")

	// Claude Code agent: harness=claude (the row default), no sandbox.
	ccAgent := findDashAgent(snap, ccConv)
	require.NotNil(t, ccAgent, "claude agent missing from Agents[]")
	assert.Equal(t, "claude", ccAgent.State.Harness, "Agents[] claude harness")
	assert.Equal(t, "", ccAgent.State.SandboxMode, "claude agent has no sandbox badge")
}

// Scenario: the spawn dialog forwards the chosen harness + sandbox in the
// POST body as the keys {"harness", "sandbox"} (modal-spawn.js). This pins
// that body contract end-to-end: a spawn with those exact keys resolves
// against the named harness and threads the chosen sandbox to the spawner —
// guarding against a key-name drift between the dialog and the daemon's
// agent.SpawnRequest. The default-sandbox path is covered by JOH-192's
// codex_sandbox_spawn_flow_test; this exercises an EXPLICIT non-default
// mode, the dialog's other case.
func TestDashboardSpawn_HarnessAndSandboxBodyContract(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")

	spawn := f.SpawnWith("squad", map[string]any{
		"name":    "cdx-ro",
		"harness": "codex",
		"sandbox": "read-only",
	})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv id")

	got, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "spawner recorded a sandbox for the new conv")
	assert.Equal(t, "read-only", got,
		"the dialog's {harness, sandbox} body must thread the explicit sandbox to the spawner")
}
