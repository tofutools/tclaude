package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func cloneGroupsOf(t *testing.T, conv string) []string {
	t.Helper()
	groups, err := db.ListGroupsForConv(conv)
	require.NoError(t, err)
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return names
}

// Scenario: a clone inherits memberships and permission overrides, but group
// ownership only when asked for: ownership is administrative authority over a
// group, and a clone must not multiply it by default.
func TestClone_OwnershipIsOptIn(t *testing.T) {
	f := newFlow(t)
	const src = "own-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(src, "lead")
	f.HaveAliveSession(src, "spwn-own", "tclaude-spwn-own", f.TestCwd("own"))
	grp := f.HaveGroup("alpha")
	f.HaveMember("alpha", src)
	require.NoError(t, db.AddAgentGroupOwner(grp.ID, src, "test"))
	require.NoError(t, db.GrantAgentPermission(src, agentd.PermHumanClipboard, "test"))

	byDefault := f.AsHuman().CloneFresh(src)
	owned, err := db.ListGroupsOwnedBy(byDefault.NewConv)
	require.NoError(t, err)
	assert.Empty(t, owned, "a default clone must not own the source's groups")
	assert.Equal(t, []string{"alpha"}, cloneGroupsOf(t, byDefault.NewConv), "memberships are still copied")
	perms, err := db.ListAgentPermissionOverrideRowsForConv(byDefault.NewConv)
	require.NoError(t, err)
	require.Len(t, perms, 1)
	assert.Equal(t, agentd.PermHumanClipboard, perms[0].Slug, "permission overrides are still copied")

	withOwnership := f.AsHuman().CloneWith(src, map[string]any{"no_copy_conv": true, "copy_ownership": true})
	require.Equal(t, http.StatusOK, withOwnership.Code, "body=%s", withOwnership.Raw)
	owned, err = db.ListGroupsOwnedBy(withOwnership.NewConv)
	require.NoError(t, err)
	assert.Equal(t, []int64{grp.ID}, owned, "copy_ownership:true copies ownership")
}

// Scenario: `group` adds the clone to one more group in the same request. An
// agent caller needs the same groups.members.add right it would need to add a
// member to that group directly; without it nothing is spawned.
func TestClone_ExtraGroupIsGatedLikeAddingAMember(t *testing.T) {
	prev := agentd.CloneCooldown
	agentd.CloneCooldown = 0
	t.Cleanup(func() { agentd.CloneCooldown = prev })

	f := newFlow(t)
	const src = "grp-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(src, "worker")
	f.HaveAliveSession(src, "spwn-grp", "tclaude-spwn-grp", f.TestCwd("grp"))
	f.HaveGroup("alpha")
	f.HaveGroup("beta")
	f.HaveMember("alpha", src)
	// The clone itself is allowed; only the extra group is in question.
	require.NoError(t, db.GrantAgentPermission(src, agentd.PermAgentClone, "test"))

	denied := f.AsAgent(src).CloneWith(src, map[string]any{"no_copy_conv": true, "group": "beta"})
	assert.Equal(t, http.StatusForbidden, denied.Code, "body=%s", denied.Raw)
	assert.Contains(t, string(denied.Raw), agentd.PermGroupsMembersAdd)
	assert.Empty(t, denied.NewConv, "a refused clone must not spawn")

	missing := f.AsHuman().CloneWith(src, map[string]any{"no_copy_conv": true, "group": "nope"})
	assert.Equal(t, http.StatusNotFound, missing.Code, "body=%s", missing.Raw)

	require.NoError(t, db.GrantAgentPermission(src, agentd.PermGroupsMembersAdd, "test"))
	ok := f.AsAgent(src).CloneWith(src, map[string]any{"no_copy_conv": true, "group": "beta"})
	require.Equal(t, http.StatusOK, ok.Code, "body=%s", ok.Raw)
	assert.ElementsMatch(t, []string{"alpha", "beta"}, cloneGroupsOf(t, ok.NewConv))
	assert.Equal(t, []string{"alpha"}, cloneGroupsOf(t, src), "the source is untouched")
}
