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
