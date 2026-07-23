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
// (JOH-162). The catalog must list all spawnable harnesses with the right
// menu values and capability flags — in particular the subtle pair the
// per-row controls gate on: Codex CAN rename (via its ConvStore, even with
// no in-pane /rename) but CANNOT compact. Every harness exposes its honest
// sandbox posture now: Codex's native `--sandbox` modes, Claude Code's
// inherit/on/off `--settings` override, and OpenCode's sole explicit
// no-containment mode.
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
	// Claude Code's OS sandbox is a settings.json block (no `--sandbox` flag),
	// but tclaude exposes a per-session inherit/on/off override delivered via
	// `--settings`, so the dialog DOES show a sandbox selector for claude.
	assert.True(t, claude.CanSandbox, "claude exposes a per-session sandbox override")
	assert.Equal(t, []string{"inherit", "on", "off"}, claude.SandboxModes)
	assert.Equal(t, "inherit", claude.DefaultSandbox, "inherit (= no override) is pre-selected")
	require.NotNil(t, claude.SandboxModeHelp, "claude exposes per-mode sandbox help")
	for _, m := range claude.SandboxModes {
		assert.NotEmpty(t, claude.SandboxModeHelp[m], "help text for mode %q", m)
	}
	assert.NotContains(t, claude.SandboxModeHelp["inherit"], "⚠", "the inherit default carries no caveat marker")
	assert.Contains(t, claude.SandboxModeHelp["off"], "⚠", "off flags its sandbox-disabled caveat")
	// Claude Code surfaces its --permission-mode values as the dialog's
	// "Permission mode" dropdown (the approval axis). The list is still
	// inherit-first (presentation), but `auto` is what the dialog pre-selects
	// and tags "(recommended)" — the JS matches DefaultApproval by value.
	assert.True(t, claude.CanApproval, "claude exposes a permission-mode (approval) catalog")
	assert.False(t, claude.CanAutoReview, "claude has no separate approvals reviewer control")
	assert.Equal(t, []string{"inherit", "plan", "default", "acceptEdits", "auto", "dontAsk", "bypassPermissions"}, claude.ApprovalModes)
	assert.Equal(t, "auto", claude.DefaultApproval, "auto (supervisor-classifier) is pre-selected")
	require.NotNil(t, claude.ApprovalModeHelp, "claude exposes per-mode approval help")
	for _, m := range claude.ApprovalModes {
		assert.NotEmpty(t, claude.ApprovalModeHelp[m], "help text for permission mode %q", m)
	}
	assert.NotContains(t, claude.ApprovalModeHelp["auto"], "⚠", "the auto default carries no caveat marker")
	assert.Contains(t, claude.ApprovalModeHelp["inherit"], "⚠", "inherit is no longer the default and flags its can-block caveat")
	assert.Contains(t, claude.ApprovalModeHelp["bypassPermissions"], "⚠", "bypassPermissions flags its no-guardrails caveat")
	assert.True(t, claude.CanRemoteControl, "claude has built-in Remote Access (/remote-control)")

	codex := findDashHarness(snap, "codex")
	require.NotNil(t, codex, "catalog missing codex; have %+v", snap.Harnesses)
	assert.Equal(t, "Codex CLI", codex.DisplayName)
	assert.Equal(t, []string{
		"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
		"gpt-5.4", "gpt-5.4-mini", "gpt-5.3-codex-spark",
	}, codex.Models, "codex exposes the curated dropdown suggestions")
	assert.NotEmpty(t, codex.EffortLevels, "codex offers effort/reasoning levels")
	assert.True(t, codex.CanRename, "codex renames out-of-band via ConvStore — must stay renameable")
	assert.True(t, codex.CanCompact, "codex supports /compact")
	assert.True(t, codex.CanSandbox, "codex takes a launch sandbox flag")
	assert.False(t, codex.CanRemoteControl, "codex has no built-in Remote Access — the toggle must be gated off")
	assert.Equal(t, []string{"tclaude-agent", "workspace-write", "read-only", "danger-full-access"}, codex.SandboxModes)
	assert.Equal(t, "tclaude-agent", codex.DefaultSandbox, "managed-profile default pre-selected")
	// Every selectable mode carries a one-line help string the dialog renders
	// as a live hint; the recommended profile has no caveat marker, the raw
	// modes flag their agentd-reachability / sandbox-off caveat with "⚠".
	require.NotNil(t, codex.SandboxModeHelp, "codex exposes per-mode sandbox help")
	for _, m := range codex.SandboxModes {
		assert.NotEmpty(t, codex.SandboxModeHelp[m], "help text for mode %q", m)
	}
	assert.NotContains(t, codex.SandboxModeHelp["tclaude-agent"], "⚠", "recommended profile carries no caveat marker")
	assert.Contains(t, codex.SandboxModeHelp["read-only"], "⚠", "read-only flags its no-agentd caveat")
	assert.True(t, codex.CanApproval, "codex supports approval (daemon default + CLI)")
	assert.True(t, codex.CanAutoReview, "codex exposes its separate approvals reviewer")
	assert.Equal(t, []string{"never", "untrusted", "on-failure", "on-request"}, codex.ApprovalModes)
	assert.Equal(t, "never", codex.DefaultApproval)
	for _, mode := range codex.ApprovalModes {
		assert.NotEmpty(t, codex.ApprovalModeHelp[mode], "help for %s", mode)
	}

	opencode := findDashHarness(snap, "opencode")
	require.NotNil(t, opencode, "catalog missing opencode; have %+v", snap.Harnesses)
	assert.Equal(t, "OpenCode", opencode.DisplayName)
	assert.True(t, opencode.CanRename, "OpenCode renames through its managed ConvStore API")
	assert.True(t, opencode.CanCompact, "OpenCode attached TUI supports /compact")
	assert.True(t, opencode.CanSandbox, "OpenCode surfaces soft access-control and explicit off postures")
	assert.Equal(t, []string{"access-control", "off"}, opencode.SandboxModes)
	assert.Equal(t, "access-control", opencode.DefaultSandbox)
	assert.Contains(t, opencode.SandboxModeHelp["access-control"], "not an OS sandbox")
	assert.Contains(t, opencode.SandboxModeHelp["off"], "No directory scoping or OS containment")
	assert.True(t, opencode.CanApproval)
	assert.Equal(t, []string{"deny", "ask", "allow-tools"}, opencode.ApprovalModes)
	assert.Equal(t, "deny", opencode.DefaultApproval)
	for _, mode := range opencode.ApprovalModes {
		assert.NotEmpty(t, opencode.ApprovalModeHelp[mode], "help for %s", mode)
	}
	assert.False(t, opencode.CanAutoReview)
	assert.False(t, opencode.CanRemoteControl)
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
	f.HaveAliveCodexSession(codexConv, codexLabel, "tmux-cdx1", f.TestCwd("cdx1"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          codexLabel,
		TmuxSession: "tmux-cdx1",
		ConvID:      codexConv,
		Cwd:         f.TestCwd("cdx1"),
		Status:      "running",
		Harness:     "codex",
		SandboxMode: "workspace-write",
	}), "stamp codex sandbox mode")
	f.HaveMember("mixed", codexConv)

	// A live Claude Code agent in the same group (mixed-harness group).
	f.HaveAliveSession(ccConv, ccLabel, "tmux-ccx1", f.TestCwd("ccx1"))
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
// POST body as the keys {"harness", "sandbox"} (agent-spawn-model.js). This pins
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

// Scenario: the Claude Code spawn dialog forwards the chosen sandbox + permission
// mode in the POST body as {"sandbox", "approval"} (agent-spawn-model.js). This pins
// that body contract end-to-end for the new Claude launch-containment fields: a
// default-harness (Claude) spawn with those keys threads the explicit sandbox
// mode AND the permission mode through to the spawner (which renders them as a
// `--settings` override + `--permission-mode`). Guards against a key-name drift
// between the dialog and the daemon's spawn request.
func TestDashboardSpawn_ClaudeSandboxAndApprovalBodyContract(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("squad")

	// No "harness" key → the default (Claude Code), exactly as a plain CC spawn
	// from the dialog omits it.
	spawn := f.SpawnWith("squad", map[string]any{
		"name":     "cc-locked",
		"sandbox":  "on",
		"approval": "plan",
	})
	require.Equal(t, 200, spawn.Code, "spawn body=%s", string(spawn.Raw))
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv id")

	gotSandbox, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "spawner recorded a sandbox for the new conv")
	assert.Equal(t, "on", gotSandbox,
		"the dialog's {sandbox} body must thread the Claude sandbox mode to the spawner")

	gotApproval, ok := f.World.SpawnApproval(spawn.ConvID)
	require.True(t, ok, "spawner recorded an approval/permission mode for the new conv")
	assert.Equal(t, "plan", gotApproval,
		"the dialog's {approval} body must thread the Claude permission mode to the spawner")
}
