package agentd_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const groupsShowContext = "Use worktrees.\nRun focused tests."

// haveShowGroup is a group with a startup context, a directory, an
// environment with a secret value, a member and an owner who is not one.
func haveShowGroup(t *testing.T, f *testharness.Flow) (member, owner string) {
	t.Helper()
	g := f.HaveGroup("alpha")
	member, owner = "show-member-aaaa-1111", "show-owner-bbbb-2222"
	f.HaveConvWithTitle(member, "worker")
	f.HaveConvWithTitle(owner, "lead")
	f.HaveMemberWithRole("alpha", member, "dev")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))
	_, err := db.SetAgentGroupDefaultContext("alpha", groupsShowContext)
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultCwd("alpha", t.TempDir())
	require.NoError(t, err)
	_, err = db.SetAgentGroupEnvironment("alpha", []sandboxpolicy.EnvironmentEntry{{Name: "API_TOKEN", Value: "s3cret-value"}})
	require.NoError(t, err)
	return member, owner
}

// showGroup runs tclaude agent groups show alpha --json as the caller.
func showGroup(t *testing.T, f *testharness.Flow, asAgent string) (agent.GroupShow, string) {
	t.Helper()
	if asAgent == "" {
		bridgeAgentClientToMux(t, f.Mux)
	} else {
		bridgeAgentClientToMuxAsAgent(t, f.Mux, asAgent)
	}
	var stdout, stderr bytes.Buffer
	rc := agent.RunGroupsShow("alpha", true, &stdout, &stderr)
	require.Equal(t, 0, rc, stderr.String())
	var out agent.GroupShow
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &out), stdout.String())
	assert.NotContains(t, stdout.String(), "s3cret-value", "an environment value is never shown")
	return out, stdout.String()
}

func withheld(g agent.GroupShow) map[string]string {
	out := map[string]string{}
	for _, w := range g.Withheld {
		out[w.Section] = w.Slug
	}
	return out
}

func TestGroupsShow_HumanSeesEverySectionButNoEnvironmentValue(t *testing.T) {
	f := newFlow(t)
	member, owner := haveShowGroup(t, f)
	g, _ := showGroup(t, f, "")
	require.NotNil(t, g.Context)
	assert.Equal(t, groupsShowContext, g.Context.Text)
	assert.Equal(t, db.GroupContextHash(groupsShowContext), g.Context.Hash)
	assert.Equal(t, "contexts", g.Context.ReinjectAfterCompact)
	require.NotNil(t, g.Settings)
	assert.Equal(t, []string{"API_TOKEN"}, g.Settings.EnvironmentKeys)
	assert.NotEmpty(t, g.Settings.DefaultCwd)
	assert.Empty(t, g.Withheld)
	require.Len(t, g.Owners, 1)
	assert.Equal(t, owner, g.Owners[0].ConvID)
	var roles []string
	for _, m := range g.Members {
		if m.ConvID == member {
			roles = append(roles, m.Role)
		}
	}
	assert.Equal(t, []string{"dev"}, roles)
}

func TestGroupsShow_MemberReadsTheContextButNotTheSettings(t *testing.T) {
	f := newFlow(t)
	member, _ := haveShowGroup(t, f)
	g, _ := showGroup(t, f, member)
	require.NotNil(t, g.Context, "membership confers groups.context.read")
	assert.Equal(t, groupsShowContext, g.Context.Text)
	assert.Nil(t, g.Settings)
	assert.Equal(t, map[string]string{"settings": agentd.PermGroupsSettingsRead}, withheld(g))
}

func TestGroupsShow_OwnerReadsContextAndSettings(t *testing.T) {
	f := newFlow(t)
	_, owner := haveShowGroup(t, f)
	g, _ := showGroup(t, f, owner)
	require.NotNil(t, g.Context)
	require.NotNil(t, g.Settings, "ownership confers groups.settings.read")
	assert.Equal(t, []string{"API_TOKEN"}, g.Settings.EnvironmentKeys)
	assert.Empty(t, g.Withheld)
}

func TestGroupsShow_OutsiderGetsTheSummaryAndIsToldWhatWasWithheld(t *testing.T) {
	f := newFlow(t)
	haveShowGroup(t, f)
	const outsider = "show-outsider-cccc-3333"
	f.HaveConvWithTitle(outsider, "outsider")
	g, text := showGroup(t, f, outsider)
	assert.Nil(t, g.Context)
	assert.Nil(t, g.Settings)
	assert.NotContains(t, text, "Use worktrees")
	assert.Equal(t, map[string]string{"context": agentd.PermGroupsContextRead, "settings": agentd.PermGroupsSettingsRead}, withheld(g))
	assert.Len(t, g.Owners, 1, "the summary is as open as groups owners")

	var stdout, stderr bytes.Buffer
	require.Equal(t, 0, agent.RunGroupsShow("alpha", false, &stdout, &stderr), stderr.String())
	assert.Contains(t, stdout.String(), "withheld: context (needs groups.context.read)")
	assert.Contains(t, stdout.String(), "withheld: settings (needs groups.settings.read)")
}

func TestGroupsShow_GrantsUmbrellaAndDenyDecideASection(t *testing.T) {
	f := newFlow(t)
	member, _ := haveShowGroup(t, f)
	require.NoError(t, db.SetAgentPermissionOverride(member, agentd.PermGroupsContextRead, db.PermEffectDeny, "test"))
	g, _ := showGroup(t, f, member)
	assert.Nil(t, g.Context, "an explicit deny beats what membership confers")

	const admin = "show-admin-dddd-4444"
	f.HaveConvWithTitle(admin, "admin")
	require.NoError(t, db.GrantAgentPermission(admin, agentd.PermGroupsAdmin, "test"))
	g, _ = showGroup(t, f, admin)
	assert.NotNil(t, g.Context, "groups.admin covers the read slugs")
	assert.NotNil(t, g.Settings)

	const reader = "show-reader-eeee-5555"
	f.HaveConvWithTitle(reader, "reader")
	require.NoError(t, db.GrantAgentPermission(reader, agentd.PermGroupsSettingsRead, "test"))
	g, _ = showGroup(t, f, reader)
	assert.NotNil(t, g.Settings)
	assert.Nil(t, g.Context)
}
