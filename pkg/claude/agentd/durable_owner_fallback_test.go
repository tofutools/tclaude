package agentd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// groupStructuralSlugs is every registered groups.* slug that is scoped by
// the group dimension: the slugs a durable owner's group action can check.
func groupStructuralSlugs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range permissionRegistry {
		if strings.HasPrefix(p.Slug, "groups.") && containsScopeDim(p.ScopeDims, ScopeDimGroup) {
			out = append(out, p.Slug)
		}
	}
	require.NotEmpty(t, out)
	return out
}

// TestDurableOwnerAllows_EqualsTheRequestGate pins what durableOwnerAllows
// decides for every group-structural slug, across owners, explicit denies,
// members, strangers and archived or removed groups, against the request
// gate (permissionAllowsAction with a bare request) it is built on. The two
// must agree everywhere: ownership and membership already reach the request
// gate through structuralPermissionMatch, so the durable check adds nothing.
func TestDurableOwnerAllows_EqualsTheRequestGate(t *testing.T) {
	setupTestDB(t)
	for _, conv := range []string{"owner", "denied-owner", "member", "stranger", "archived-owner", "removed-owner"} {
		_, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
	}
	live, err := db.CreateAgentGroup("live", "")
	require.NoError(t, err)
	for _, c := range []string{"owner", "denied-owner"} {
		require.NoError(t, db.AddAgentGroupOwner(live, c, "<test>"))
	}
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: live, ConvID: "member"}))
	archived, err := db.CreateAgentGroup("archived", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(archived, "archived-owner", "<test>"))
	require.NoError(t, db.ArchiveAgentGroup("archived"))
	removed, err := db.CreateAgentGroup("removed", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(removed, "removed-owner", "<test>"))
	require.NoError(t, db.DeleteAgentGroup("removed"))

	slugs := groupStructuralSlugs(t)
	for _, slug := range slugs {
		require.NoError(t, db.SetAgentPermissionOverride("denied-owner", slug, db.PermEffectDeny, "<test>"))
	}

	cases := []struct{ conv, group string }{
		{"owner", "live"}, {"denied-owner", "live"}, {"member", "live"}, {"stranger", "live"},
		{"archived-owner", "archived"}, {"removed-owner", "removed"},
	}
	ownerAllowed := 0
	for _, slug := range slugs {
		for _, c := range cases {
			actx := ActionContext{Group: c.group, structuralGroup: c.group}
			durable, durableErr := durableOwnerAllows("cron", c.conv, slug, actx)
			req, _ := http.NewRequest(http.MethodPost, "http://cron.invalid", nil)
			gate, _, gateErr := permissionAllowsAction(req, c.conv, slug, actx)
			assert.Equal(t, gate, durable, "%s as %s in %s: durable check and request gate disagree", slug, c.conv, c.group)
			assert.Equal(t, gateErr, durableErr, "%s as %s in %s", slug, c.conv, c.group)
			switch c.conv {
			case "denied-owner", "stranger", "archived-owner", "removed-owner":
				assert.False(t, durable, "%s as %s in %s must be refused", slug, c.conv, c.group)
			case "owner":
				if durable {
					ownerAllowed++
				}
			}
		}
	}
	for _, slug := range []string{PermGroupsMessagesSchedule, PermGroupsTriggersManage, PermGroupsOwnersManage} {
		ok, err := durableOwnerAllows("trigger", "owner", slug, ActionContext{Group: "live", structuralGroup: "live"})
		require.NoError(t, err)
		assert.True(t, ok, "a group owner keeps %s, which the cron and trigger owner checks rely on", slug)
	}
	assert.Positive(t, ownerAllowed)
}

// The cron and trigger owner checks, end to end from a stored job or rule:
// the owner passes, an explicit deny and a stranger fail, and an archived
// group is refused before any permission is consulted.
func TestDurableOwnerChecks_CronAndTrigger(t *testing.T) {
	setupTestDB(t)
	agents := map[string]string{}
	for _, conv := range []string{"owner", "denied-owner", "stranger"} {
		id, _, err := db.EnsureAgentForConv(conv, "test")
		require.NoError(t, err)
		agents[conv] = id
	}
	live, err := db.CreateAgentGroup("live", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(live, "owner", "<test>"))
	require.NoError(t, db.AddAgentGroupOwner(live, "denied-owner", "<test>"))
	require.NoError(t, db.SetAgentPermissionOverride("denied-owner", PermGroupsMessagesSchedule, db.PermEffectDeny, "<test>"))
	require.NoError(t, db.SetAgentPermissionOverride("denied-owner", PermGroupsTriggersManage, db.PermEffectDeny, "<test>"))
	archived, err := db.CreateAgentGroup("archived", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(archived, "owner", "<test>"))
	require.NoError(t, db.ArchiveAgentGroup("archived"))

	cron := func(conv string, group int64) (string, error) {
		return cronSpawnOwnerConv(&db.AgentCronJob{OwnerAgent: agents[conv], GroupID: group})
	}
	trigger := func(conv string, group int64) (string, error) {
		return triggerOwnerConv(&db.TriggerRule{OwnerAgent: agents[conv], ScopeKind: db.TriggerScopeGroup, GroupID: group})
	}
	for name, check := range map[string]func(string, int64) (string, error){"cron": cron, "trigger": trigger} {
		got, err := check("owner", live)
		require.NoError(t, err, name)
		assert.Equal(t, "owner", got, "%s: the group owner passes", name)

		_, err = check("denied-owner", live)
		assert.Error(t, err, "%s: an explicit deny beats ownership", name)

		_, err = check("stranger", live)
		assert.Error(t, err, "%s: a non-owner fails", name)

		_, err = check("owner", archived)
		require.Error(t, err, "%s: an archived group is refused", name)
		assert.Contains(t, err.Error(), "unavailable", name)
	}
}
