package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tcl-u0qkk1: a template snapshotted from a group carries the group's owner
// scopes, which members own it and each member's permission grants. A
// restricted peer is shown each only under the grant that shows it on the
// group itself (peer_group_permissions.go), so a from-group preview or save
// is no way around those grants.

// peerFromGroupFixture gives crew an owner scope, makes its member an owner
// and grants the member a slug no other part of the fixture names. It also
// has an owner that is not a member, which the snapshot adds as an agent.
func peerFromGroupFixture(t *testing.T) *peerProcessHarness {
	t.Helper()
	h := newPeerProcessHarness(t)
	_, err := db.SetAgentGroupOwnerScopes("crew", `{"groups.members.spawn":{"spawn_profile":["narrow-profile"]}}`)
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(h.group.ID, "proc-crew-member", "human"))
	require.NoError(t, db.GrantAgentPermission("proc-crew-member", agentd.PermGroupsMembersStop, "human"))
	h.f.HaveConvWithTitle("crew-outside-owner", "outside-owner")
	require.NoError(t, db.AddAgentGroupOwner(h.group.ID, "crew-outside-owner", "human"))
	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	h.grant(t, agentd.PermTemplatesManage, "")
	return h
}

// grantLaunch lets the peer save what the member launches with (its role and
// approval), so a save reaches the checks pinned here.
func grantLaunch(t *testing.T, h *peerProcessHarness) {
	t.Helper()
	h.grant(t, agentd.PermGroupsMembersSpawn, "")
	h.grant(t, agentd.PermGroupsMembersSpawnAll, "")
}

func fromGroup(h *peerProcessHarness, name string, preview bool) (int, string) {
	return h.call("POST", "/api/templates/from-group", map[string]any{"group": "crew", "template_name": name, "preview": preview})
}

func assertNoGroupPermissionSettings(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, "narrow-profile", "owner scopes withheld: %s", body)
	assert.NotContains(t, body, `"is_owner":true`, "owner flags withheld: %s", body)
	assert.NotContains(t, body, `"effective_is_owner":true`, "owner flags withheld: %s", body)
	assert.NotContains(t, body, agentd.PermGroupsMembersStop, "member grants withheld: %s", body)
	assert.NotContains(t, body, "outside-owner", "an owner outside the roster is withheld whole: %s", body)
}

func TestPeerTemplateFromGroupPreviewWithholdsWhatTheGroupDoesNotShow(t *testing.T) {
	h := peerFromGroupFixture(t)
	code, body := fromGroup(h, "copy", true)
	require.Equal(t, 200, code, body)
	assertNoGroupPermissionSettings(t, body)

	// Each shows under the grant that shows it on the group.
	h.grant(t, agentd.PermGroupsSettingsOwnerScopes, "group=crew")
	h.grant(t, agentd.PermGroupsOwnersManage, "group=crew")
	h.grant(t, agentd.PermGroupsSettingsMemberPermissions, "group=crew")
	code, body = fromGroup(h, "copy", true)
	require.Equal(t, 200, code, body)
	assert.Contains(t, body, "narrow-profile")
	assert.Contains(t, body, `"is_owner":true`)
	assert.Contains(t, body, agentd.PermGroupsMembersStop)
	assert.Contains(t, body, "outside-owner")
}

// A save cannot drop the owner scopes: a template without them deploys
// owners holding more than the group's do. So a peer not shown them saves
// no template from that group.
func TestPeerTemplateFromGroupSaveRefusesUnreadableOwnerScopes(t *testing.T) {
	h := peerFromGroupFixture(t)
	grantLaunch(t, h)
	code, body := fromGroup(h, "copy", false)
	require.Equal(t, 403, code, body)
	assert.Contains(t, body, agentd.PermGroupsSettingsOwnerScopes)
	assertNoGroupPermissionSettings(t, body)
	tmpl, err := db.GetGroupTemplate("copy")
	require.NoError(t, err)
	assert.Nil(t, tmpl, "nothing saved")
}

// Without owner scopes, the save lands with owner flags and member grants
// left out, which only narrows what a deploy confers.
func TestPeerTemplateFromGroupSaveLeavesOutWhatTheGroupDoesNotShow(t *testing.T) {
	h := peerFromGroupFixture(t)
	grantLaunch(t, h)
	_, err := db.SetAgentGroupOwnerScopes("crew", "")
	require.NoError(t, err)
	code, body := fromGroup(h, "copy", false)
	require.Equal(t, 201, code, body)
	assertNoGroupPermissionSettings(t, body)
	tmpl, err := db.GetGroupTemplate("copy")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	for _, a := range tmpl.Agents {
		assert.False(t, a.IsOwner, "%s", a.Name)
		if a.ProfileInline != nil {
			assert.NotContains(t, a.ProfileInline.PermissionOverrides, agentd.PermGroupsMembersStop, "%s", a.Name)
		}
	}
}
