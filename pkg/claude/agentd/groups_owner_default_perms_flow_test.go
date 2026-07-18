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

// serveAs posts to the group mux as a specific agent peer and returns
// the status code — the bulk group lifecycle verbs (stop/resume/retire)
// reply 200 with a per-member result list even when every member is
// skipped, so the permission verdict shows up purely in the status code.
func serveGroupVerbAs(t *testing.T, f *testharness.Flow, verb, group, convID string) int {
	t.Helper()
	path := "/v1/groups/" + group + "/" + verb
	rec := testharness.Serve(f.Mux,
		agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, nil), convID))
	return rec.Code
}

// Scenario: a group OWNER runs its own team's lifecycle (stop / resume /
// retire / spawn) with NO explicit slug grant. Owner-state raises the
// default group-lifecycle slugs, so each verb succeeds — the operator's
// "owners can manage their own group's members" requirement.
func TestGroupOwnerDefaultPerms_OwnerPassesWithoutSlug(t *testing.T) {
	f := newFlow(t)

	const owner = "godo-aaaa-bbbb-cccc-dddd"
	const member = "godm-aaaa-bbbb-cccc-dddd"
	const memberLabel = "lbl-godm"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(member, "worker")
	f.HaveAliveSession(member, memberLabel, "tmux-godm", f.TestCwd("godm"))
	f.HaveMember("squad", member)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner")

	// stop / resume / retire: the owner passes the permission gate.
	assert.Equal(t, http.StatusOK, serveGroupVerbAs(t, f, "stop", "squad", owner),
		"owner should stop its own group without groups.stop")
	assert.Equal(t, http.StatusOK, serveGroupVerbAs(t, f, "resume", "squad", owner),
		"owner should resume its own group without groups.resume")
	assert.Equal(t, http.StatusOK, serveGroupVerbAs(t, f, "retire", "squad", owner),
		"owner should retire its own group without groups.retire")

	// spawn: the owner passes the permission gate AND the spawn
	// guardrails (which already treat an owner as allowed), so a fresh
	// agent lands in the group.
	resp := f.AsAgent(owner).SpawnWith("squad", map[string]any{"name": "newbie"})
	assert.Equal(t, http.StatusOK, resp.Code,
		"owner should spawn into its own group without groups.spawn; body=%s", string(resp.Raw))
	assert.NotEmpty(t, resp.ConvID, "spawn returned a conv-id")
}

// Scenario: a plain MEMBER (not an owner) with no slug tries the group
// lifecycle verbs. Membership alone confers nothing — only ownership or
// the explicit slug does. Every verb is refused.
func TestGroupOwnerDefaultPerms_NonOwnerDenied(t *testing.T) {
	f := newFlow(t)

	const member = "gndm-aaaa-bbbb-cccc-dddd"
	const other = "gndo-aaaa-bbbb-cccc-dddd"
	const otherLabel = "lbl-gndo"

	f.HaveGroup("squad")
	f.HaveConvWithTitle(other, "worker")
	f.HaveAliveSession(other, otherLabel, "tmux-gndo", f.TestCwd("gndo"))
	f.HaveMember("squad", other)
	f.HaveMember("squad", member) // a co-member, but NOT an owner

	assert.Equal(t, http.StatusForbidden, serveGroupVerbAs(t, f, "stop", "squad", member),
		"a non-owner member must not stop the group")
	assert.Equal(t, http.StatusForbidden, serveGroupVerbAs(t, f, "resume", "squad", member),
		"a non-owner member must not resume the group")
	assert.Equal(t, http.StatusForbidden, serveGroupVerbAs(t, f, "retire", "squad", member),
		"a non-owner member must not retire the group")

	resp := f.AsAgent(member).SpawnWith("squad", map[string]any{"name": "newbie"})
	assert.Equal(t, http.StatusForbidden, resp.Code,
		"a non-owner member must not spawn into the group; body=%s", string(resp.Raw))
}

// Scenario: an OWNER with an explicit DENY override on a group-lifecycle
// slug. Deny is always authoritative — it suppresses the owner-state
// default, exactly as it does for the context-info reads and every other
// gate. The owner is refused that verb despite ownership.
func TestGroupOwnerDefaultPerms_DenyOverrideBeatsOwner(t *testing.T) {
	f := newFlow(t)

	const owner = "gddo-aaaa-bbbb-cccc-dddd"
	const member = "gddm-aaaa-bbbb-cccc-dddd"
	const memberLabel = "lbl-gddm"

	g := f.HaveGroup("squad")
	f.HaveConvWithTitle(member, "worker")
	f.HaveAliveSession(member, memberLabel, "tmux-gddm", f.TestCwd("gddm"))
	f.HaveMember("squad", member)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner")
	// Deny the two lifecycle slugs specifically — owner status must not raise
	// either one back above an explicit deny.
	require.NoError(t,
		db.SetAgentPermissionOverride(owner, agentd.PermGroupsStop, db.PermEffectDeny, "test"),
		"seed deny override on groups.stop")
	require.NoError(t,
		db.SetAgentPermissionOverride(owner, agentd.PermGroupsResume, db.PermEffectDeny, "test"),
		"seed deny override on groups.resume")

	assert.Equal(t, http.StatusForbidden, serveGroupVerbAs(t, f, "stop", "squad", owner),
		"deny override on groups.stop must beat the owner default")
	assert.Equal(t, http.StatusForbidden, serveGroupVerbAs(t, f, "resume", "squad", owner),
		"deny override on groups.resume must beat the owner default")
	// An un-denied sibling still rides the owner default.
	assert.Equal(t, http.StatusOK, serveGroupVerbAs(t, f, "retire", "squad", owner),
		"owner still retires when only stop and resume are denied")
}
