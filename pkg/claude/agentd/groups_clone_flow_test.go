package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// groupCloneRequest issues POST /v1/groups/{src}/clone as the human
// peer. body may be nil (default name) or carry {"new_name": ...} /
// {"no_copy_conv": true}. Tests run with no_copy_conv: true to skip
// the convops.CopyConversationToPath path the simulator doesn't
// model — same convention clone_flow_test.go uses for CloneFresh.
func groupCloneRequest(t *testing.T, f *testharness.Flow, src string, body map[string]any) *cloneGroupResp {
	t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	body["no_copy_conv"] = true
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/"+src+"/clone", body))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code,
		"clone group %q: body=%s", src, rec.Body.String())
	var out cloneGroupResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode body=%s", rec.Body.String())
	return &out
}

type cloneGroupResp struct {
	Group        string `json:"group"`
	SrcGroup     string `json:"src_group"`
	OwnersCopied int    `json:"owners_copied"`
	Members      []struct {
		SrcConv string `json:"src_conv"`
		NewConv string `json:"new_conv,omitempty"`
		Alias   string `json:"alias,omitempty"`
		Label   string `json:"label,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"members"`
}

// Scenario: clone a 2-member group with no explicit name. Default name
// should be "<src>-c-1"; the source group is left untouched; both
// members spawn fresh conv-ids in the new group with -c-N aliases.
func TestGroupsClone_DefaultsSuffix(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	const bConv = "bbb-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveAliveSession(bConv, "spwn-b", "tclaude-spwn-b", "/tmp/work")
	src := f.HaveGroup("team")
	f.HaveMember("team", aConv, "alice")
	f.HaveMember("team", bConv, "bob")

	resp := groupCloneRequest(t, f, "team", nil)
	assert.Equal(t, "team-c-1", resp.Group, "default name")
	assert.Equal(t, "team", resp.SrcGroup, "src_group")
	require.Len(t, resp.Members, 2, "members count")
	for _, m := range resp.Members {
		assert.Empty(t, m.Error, "member %s reported error: %s", m.SrcConv, m.Error)
		assert.NotEmpty(t, m.NewConv, "member %s missing new_conv", m.SrcConv)
	}

	// Source group untouched: same id, same 2 members.
	srcAfter, _ := db.GetAgentGroupByName("team")
	if assert.NotNil(t, srcAfter, "source group should still exist") {
		assert.Equal(t, src.ID, srcAfter.ID, "source group id should be stable")
	}
	srcMembers, _ := db.ListAgentGroupMembers(src.ID)
	assert.Len(t, srcMembers, 2, "source group member count")

	// New group exists with 2 members.
	newGroup, _ := db.GetAgentGroupByName("team-c-1")
	require.NotNil(t, newGroup, "team-c-1 should exist")
	newMembers, _ := db.ListAgentGroupMembers(newGroup.ID)
	assert.Len(t, newMembers, 2, "new group member count")
	for _, m := range newMembers {
		assert.NotEmpty(t, m.Alias, "new member should have a -c-<N> alias; got %+v", m)
	}
}

// Scenario: clone with an explicit name that doesn't collide.
func TestGroupsClone_ExplicitName(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", aConv, "alice")

	resp := groupCloneRequest(t, f, "team", map[string]any{"new_name": "team-experiment"})
	assert.Equal(t, "team-experiment", resp.Group, "explicit name")
	got, _ := db.GetAgentGroupByName("team-experiment")
	assert.NotNil(t, got, "team-experiment should exist")
}

// Scenario: clone with an explicit name that ALREADY exists → 409,
// no new group created. (Skips groupCloneRequest's status assertion
// since the helper would Fatal on non-200.)
func TestGroupsClone_NameCollisionIsConflict(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team")
	f.HaveGroup("team-experiment")
	f.HaveMember("team", aConv, "alice")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/team/clone",
		map[string]any{"new_name": "team-experiment", "no_copy_conv": true}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, rec.Code,
		"collision body=%s", rec.Body.String())
}

// Scenario: source group has owners. They should be copied (same
// conv-ids, no clone) onto the new group. Mirrors the TODO's
// "owners stay as the same conv-id" rule.
func TestGroupsClone_OwnersCopied(t *testing.T) {
	f := newFlow(t)

	const memberConv = "mem-aaaa-bbbb-cccc-1111"
	const ownerConv = "own-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(memberConv, "worker")
	f.HaveAliveSession(memberConv, "spwn-mem", "tclaude-spwn-mem", "/tmp/work")
	src := f.HaveGroup("team")
	f.HaveMember("team", memberConv, "worker")
	require.NoError(t, db.AddAgentGroupOwner(src.ID, ownerConv, "test"), "AddAgentGroupOwner")

	resp := groupCloneRequest(t, f, "team", nil)
	assert.Equal(t, 1, resp.OwnersCopied, "owners_copied")
	newGroup, _ := db.GetAgentGroupByName(resp.Group)
	require.NotNil(t, newGroup, "new group missing")
	owners, _ := db.ListAgentGroupOwners(newGroup.ID)
	if assert.Len(t, owners, 1, "new group owners") {
		assert.Equal(t, ownerConv, owners[0].ConvID, "owner conv")
	}
}

// Scenario: clone-of-clone strips the existing -c-<N> suffix when
// computing the next default name. team-c-1 cloned should produce
// team-c-2, NOT team-c-1-c-1. Mirrors uniqueCloneAlias for symmetry.
func TestGroupsClone_OfClone_StripsSuffix(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team-c-1") // pretend this is already a clone
	f.HaveMember("team-c-1", aConv, "alice")

	resp := groupCloneRequest(t, f, "team-c-1", nil)
	// The base is "team" (after stripping -c-1); the next free N is
	// 2 since -c-1 is already used.
	assert.Equal(t, "team-c-2", resp.Group, "clone-of-clone name")
}

// Scenario: source group is archived → 409 (mutating ops on archived
// groups are refused per the existing requireGroupActive guard).
// Pins the auth + state-machine: archive must seal the group from
// clones too, not just member-mutations.
func TestGroupsClone_ArchivedSourceRejected(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", aConv, "alice")
	require.NoError(t, db.ArchiveAgentGroup("team"), "archive")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost,
		"/v1/groups/team/clone", map[string]any{"no_copy_conv": true}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusConflict, rec.Code, "archived source clone")
}

// Scenario: a member with no live tmux session is skipped (status
// "skipped: source has no live tmux session"), and the clone of the
// other live members proceeds. Pins the partial-failure semantics
// from the TODO doc — the new group exists, contains the live
// members' clones, and the offline member is reported.
func TestGroupsClone_OfflineMemberSkipped(t *testing.T) {
	f := newFlow(t)

	const liveConv = "liv-aaaa-bbbb-cccc-1111"
	const deadConv = "ded-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(liveConv, "alice")
	f.HaveConvWithTitle(deadConv, "ghost")
	f.HaveAliveSession(liveConv, "spwn-liv", "tclaude-spwn-liv", "/tmp/work")
	// deadConv intentionally has NO live session.
	f.HaveGroup("team")
	f.HaveMember("team", liveConv, "alice")
	f.HaveMember("team", deadConv, "ghost")

	resp := groupCloneRequest(t, f, "team", nil)
	require.Len(t, resp.Members, 2, "members reported")
	skipped := 0
	cloned := 0
	for _, m := range resp.Members {
		if m.Error != "" {
			skipped++
		} else {
			cloned++
		}
	}
	assert.Equal(t, 1, skipped, "skipped count")
	assert.Equal(t, 1, cloned, "cloned count")

	// New group exists with the one live clone.
	newGroup, _ := db.GetAgentGroupByName(resp.Group)
	require.NotNil(t, newGroup, "new group should exist even with partial failure")
	newMembers, _ := db.ListAgentGroupMembers(newGroup.ID)
	assert.Len(t, newMembers, 1, "new group should have 1 member (the live clone)")
}
