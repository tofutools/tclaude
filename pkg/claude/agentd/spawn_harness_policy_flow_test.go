package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestSpawnHarnessPolicyReturnsConfiguredReasonToAgent(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const lead = "lead-cross-harness-policy-111111111111"
	f.HaveMember("alpha", lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: db.SpawnHarnessDeny, Reason: "Codex credits are reserved for release reviews",
	}}))

	resp := f.AsAgent(lead).SpawnWith(g.Name, map[string]any{
		"alias": "worker", "harness": "codex",
	})
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "cross_harness_spawn_denied")
	assert.Contains(t, string(resp.Raw), "claude → codex")
	assert.Contains(t, string(resp.Raw), "Codex credits are reserved for release reviews")
	assert.Empty(t, f.ListGroupMembers(g.Name)[1:], "denial must not add a child")
}

func TestSpawnHarnessPolicyGroupDenyOverridesGlobalAllow(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const lead = "lead-group-harness-policy-222222222222"
	f.HaveMember(g.Name, lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex", Decision: db.SpawnHarnessAllow,
	}}))
	require.NoError(t, db.ReplaceSpawnHarnessRules(g.ID, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: db.SpawnHarnessDeny, Reason: "this group stays on Claude",
	}}))

	resp := f.AsAgent(lead).SpawnWith(g.Name, map[string]any{
		"alias": "worker", "harness": "codex",
	})
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), `group \"alpha\" policy`)
	assert.Contains(t, string(resp.Raw), "this group stays on Claude")
}

func TestSpawnHarnessPolicyChecksHarnessResolvedByDefaultProfile(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const lead = "lead-profile-harness-policy-333333333333"
	f.HaveMember(g.Name, lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsSpawn, "test"))
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex", "model": "gpt-5-codex",
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, g.Name, "codex-default").Code)
	require.NoError(t, db.ReplaceSpawnHarnessRules(0, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: db.SpawnHarnessDeny, Reason: "profile may not cross the vendor boundary",
	}}))

	// The request deliberately omits harness. The group profile resolves it to
	// Codex before the policy check, so this indirect cross-harness path is
	// denied just like an explicit harness request.
	resp := f.AsAgent(lead).SpawnWith(g.Name, map[string]any{"alias": "worker"})
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "claude → codex")
	assert.Contains(t, string(resp.Raw), "profile may not cross the vendor boundary")
}

func TestSpawnHarnessPolicyCloneIncludesOwnerOnlyDestinationGroups(t *testing.T) {
	f := newFlow(t)
	const caller = "44444444-4444-4444-8444-444444444444"
	const target = "55555555-5555-4555-8555-555555555555"
	f.HaveAliveSession(caller, "spwn-clone-manager", "tmux-clone-manager", t.TempDir())
	targetCwd := t.TempDir()
	targetSim := f.HaveAliveCodexSession(target, "spwn-owner-target", "tmux-owner-target", targetCwd)
	require.NoError(t, targetSim.WriteThreadRow(testharness.CodexThreadSeed{
		Title: "owner target", FirstUserMessage: "owner target", Cwd: targetCwd,
	}))
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID: target, CustomTitle: "owner target", ProjectPath: targetCwd, Harness: "codex",
	}))
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermAgentClone, "test"))
	ownerGroup := f.HaveGroup("owner-only")
	require.NoError(t, db.AddAgentGroupOwner(ownerGroup.ID, target, "test"))
	require.NoError(t, db.ReplaceSpawnHarnessRules(ownerGroup.ID, []db.SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: db.SpawnHarnessDeny, Reason: "owner-only group stays on Codex",
	}}))

	resp := f.AsAgent(caller).CloneWith(target, map[string]any{"no_copy_conv": true})
	require.Equal(t, http.StatusForbidden, resp.Code, "body=%s", resp.Raw)
	assert.Empty(t, resp.NewConv, "policy denial must happen before clone spawn")
	assert.Contains(t, string(resp.Raw), `group \"owner-only\" policy`)
	assert.Contains(t, string(resp.Raw), "owner-only group stays on Codex")
}
