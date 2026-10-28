package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: the operator moves an agent between groups with
// `tclaude agent groups move` (POST /v1/groups/{from}/members/{conv}/move),
// keeping its sandbox on the group it leaves. The agent lands in the target
// group with its role, leaves the source, and is told what changed. An agent
// without the member-management grants is refused and nothing moves.
func TestGroupMove_MovesMemberAndNotifiesAgent(t *testing.T) {
	f := newFlow(t)
	plain := f.HaveGroup("plain")
	f.HaveGroup("elevated")
	const conv = "move-flow-conv"
	f.HaveMemberWithRole("plain", conv, "builder")
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	launched := sandboxpolicy.EmptySnapshot()
	launched.ResolutionGroupID = plain.ID
	launched.RefreshGroupEnvironment = true
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(agentID, &launched))

	const outsider = "move-flow-outsider"
	f.HaveEnrolledAgent(outsider)
	denied := agentReq(t, f, outsider, http.MethodPost, "/v1/groups/plain/members/"+conv+"/move",
		map[string]any{"to": "elevated"})
	assert.Equal(t, http.StatusForbidden, denied.Code, "body=%s", denied.Body.String())
	require.Len(t, f.ListGroupMembers("plain"), 1, "a refused move changes nothing")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/plain/members/"+conv+"/move", map[string]any{"to": "elevated", "keep_sandbox": true})))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	f.AssertNotGroupMember("plain", conv)
	members := f.ListGroupMembers("elevated")
	require.Len(t, members, 1)
	assert.Equal(t, conv, members[0].ConvID)
	assert.Equal(t, "builder", members[0].Role, "the role moves with the agent")

	inbox, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	assert.Equal(t, "Moved to group elevated", inbox[0].Subject)
	assert.Contains(t, inbox[0].Body, `Sandbox profile: stays on "plain"`)
	assert.Contains(t, inbox[0].Body, `Group environment variables: now comes from "elevated"`)

	again := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/plain/members/"+conv+"/move", map[string]any{"to": "elevated"})))
	assert.Equal(t, http.StatusNotFound, again.Code, "no longer a member of the source group")
}

// Scenario: the operator moves an agent out of a group whose permissions it
// still needs, keeping them (the move dialog's unticked "Switch to beta's
// permissions" box). The agent holds both groups' permissions at once, sees the
// kept group in whoami and in its notice, and loses them when the operator
// drops them. An agent manager cannot choose to keep permissions.
func TestGroupMove_KeepPermissionsIsOperatorOnlyAndDroppable(t *testing.T) {
	f := newFlow(t)
	alpha := f.HaveGroup("alpha")
	beta := f.HaveGroup("beta")
	require.NoError(t, db.ReplaceAgentGroupPermissions(alpha.ID, []string{agentd.PermGroupsMembersAdd}, "test"))
	const conv = "keep-perms-flow-conv"
	f.HaveMember("alpha", conv)

	const manager = "keep-perms-flow-manager"
	f.HaveEnrolledAgent(manager)
	require.NoError(t, db.GrantAgentPermission(manager, agentd.PermGroupsMembersRemove, "test"))
	require.NoError(t, db.GrantAgentPermission(manager, agentd.PermGroupsMembersAdd, "test"))
	denied := agentReq(t, f, manager, http.MethodPost, "/v1/groups/alpha/members/"+conv+"/move",
		map[string]any{"to": "beta", "keep_permissions": true})
	assert.Equal(t, http.StatusForbidden, denied.Code, "body=%s", denied.Body.String())
	require.Len(t, f.ListGroupMembers("alpha"), 1, "a refused move changes nothing")

	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/alpha/members/"+conv+"/move", map[string]any{"to": "beta", "keep_permissions": true})))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	f.AssertNotGroupMember("alpha", conv)

	held, err := db.HasAgentGroupPermissionForGroup(conv, alpha.ID, agentd.PermGroupsMembersAdd)
	require.NoError(t, err)
	assert.True(t, held, "alpha's grant still applies, bound to alpha")
	held, err = db.HasAgentGroupPermissionForGroup(conv, beta.ID, agentd.PermGroupsMembersAdd)
	require.NoError(t, err)
	assert.False(t, held, "and is not widened to beta")

	whoami := func() []string {
		t.Helper()
		who := agentReq(t, f, conv, http.MethodGet, "/v1/whoami", nil)
		require.Equal(t, http.StatusOK, who.Code)
		var body struct {
			Kept []string `json:"kept_permission_groups"`
		}
		testharness.DecodeJSON(t, who, &body)
		return body.Kept
	}
	assert.Equal(t, []string{"alpha"}, whoami())
	inbox, err := db.ListAgentMessagesForConv(conv, 10)
	require.NoError(t, err)
	require.NotEmpty(t, inbox)
	assert.Contains(t, inbox[0].Body, `the operator kept "alpha"'s for you too`)

	drop := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodDelete,
		"/v1/groups/alpha/kept-permissions/"+conv, nil)))
	require.Equal(t, http.StatusOK, drop.Code, "body=%s", drop.Body.String())
	assert.Empty(t, whoami())
	held, err = db.HasAgentGroupPermissionForGroup(conv, alpha.ID, agentd.PermGroupsMembersAdd)
	require.NoError(t, err)
	assert.False(t, held, "dropping ends alpha's permissions")
	again := testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodDelete,
		"/v1/groups/alpha/kept-permissions/"+conv, nil)))
	assert.Equal(t, http.StatusNotFound, again.Code)
}
