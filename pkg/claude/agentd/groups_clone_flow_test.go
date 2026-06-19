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
		Title   string `json:"title,omitempty"`
		Label   string `json:"label,omitempty"`
		Error   string `json:"error,omitempty"`
	} `json:"members"`
}

// Scenario: clone a 2-member group with no explicit name. Default name
// should be "<src>-c-1"; the source group is left untouched; both
// members spawn fresh conv-ids in the new group, each renamed to a
// `<src-member-title>-c-N` title.
func TestGroupsClone_DefaultsSuffix(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	const bConv = "bbb-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveConvWithTitle(bConv, "bob")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveAliveSession(bConv, "spwn-b", "tclaude-spwn-b", "/tmp/work")
	src := f.HaveGroup("team")
	f.HaveMember("team", aConv)
	f.HaveMember("team", bConv)

	resp := groupCloneRequest(t, f, "team", nil)
	assert.Equal(t, "team-c-1", resp.Group, "default name")
	assert.Equal(t, "team", resp.SrcGroup, "src_group")
	require.Len(t, resp.Members, 2, "members count")
	for _, m := range resp.Members {
		assert.Empty(t, m.Error, "member %s reported error: %s", m.SrcConv, m.Error)
		assert.NotEmpty(t, m.NewConv, "member %s missing new_conv", m.SrcConv)
		// Each clone is renamed to `<src-title>-c-<N>` — the title
		// carries the agent's single name now that membership rows
		// have none.
		assert.Contains(t, m.Title, "-c-", "clone %s should get a -c-<N> title; got %q", m.SrcConv, m.Title)
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
}

// Scenario: clone with an explicit name that doesn't collide.
func TestGroupsClone_ExplicitName(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team")
	f.HaveMember("team", aConv)

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
	f.HaveMember("team", aConv)

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
	f.HaveMember("team", memberConv)
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

// Scenario: the clone carries EVERY configurable group setting, not
// just the description — default cwd, startup context, default profile,
// the max-members cap and the notify switch. Each is set to a
// distinctive non-default value on the source; the clone must match all
// of them. Runs --no-agents so the assertion is purely about the group
// row (no live-session plumbing needed). notify defaults to true, so
// setting it false proves the value is copied rather than re-defaulted.
func TestGroupsClone_CopiesAllSettings(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("team")

	mustSet := func(_ int64, err error) { require.NoError(t, err) }
	mustSet(db.SetAgentGroupDescr("team", "the team descr"))
	mustSet(db.SetAgentGroupDefaultCwd("team", "/tmp/team-dir"))
	mustSet(db.SetAgentGroupDefaultContext("team", "shared startup context\nsecond line"))
	mustSet(db.SetAgentGroupDefaultProfile("team", "fast"))
	mustSet(db.SetAgentGroupMaxMembers("team", 7))
	mustSet(db.SetAgentGroupNotifyEnabled("team", false))

	resp := groupCloneRequest(t, f, "team", map[string]any{"no_clone_members": true})
	newGroup, _ := db.GetAgentGroupByName(resp.Group)
	require.NotNil(t, newGroup, "cloned group should exist")
	assert.Equal(t, "the team descr", newGroup.Descr, "descr copied")
	assert.Equal(t, "/tmp/team-dir", newGroup.DefaultCwd, "default cwd copied")
	assert.Equal(t, "fast", newGroup.DefaultProfile, "default profile copied")
	assert.Equal(t, 7, newGroup.MaxMembers, "max members copied")
	assert.False(t, newGroup.NotifyEnabled, "notify switch copied (false, not re-defaulted to true)")
	// default_context is normalized for the one-line header invariant
	// only on descr; context is multi-line and copied verbatim.
	assert.Equal(t, "shared startup context\nsecond line", newGroup.DefaultContext, "startup context copied verbatim")
}

// Scenario: --no-agents (no_clone_members) clones the group's settings +
// owners but skips the member-agent clone loop entirely. The new group
// comes up with zero members and the source's owner(s), and the source
// is left untouched.
func TestGroupsClone_NoAgents_SkipsMembersKeepsOwners(t *testing.T) {
	f := newFlow(t)

	const memberConv = "mem-aaaa-bbbb-cccc-1111"
	const ownerConv = "own-aaaa-bbbb-cccc-2222"
	f.HaveConvWithTitle(memberConv, "worker")
	f.HaveAliveSession(memberConv, "spwn-mem", "tclaude-spwn-mem", "/tmp/work")
	src := f.HaveGroup("team")
	f.HaveMember("team", memberConv)
	require.NoError(t, db.AddAgentGroupOwner(src.ID, ownerConv, "test"), "AddAgentGroupOwner")

	resp := groupCloneRequest(t, f, "team", map[string]any{"no_clone_members": true})
	assert.Empty(t, resp.Members, "no member clones in --no-agents mode")
	assert.Equal(t, 1, resp.OwnersCopied, "owners still copied")

	newGroup, _ := db.GetAgentGroupByName(resp.Group)
	require.NotNil(t, newGroup, "new group should exist")
	newMembers, _ := db.ListAgentGroupMembers(newGroup.ID)
	assert.Empty(t, newMembers, "new group should have no members")
	owners, _ := db.ListAgentGroupOwners(newGroup.ID)
	if assert.Len(t, owners, 1, "owner copied onto new group") {
		assert.Equal(t, ownerConv, owners[0].ConvID, "owner conv")
	}

	// Source untouched: still has its one member.
	srcMembers, _ := db.ListAgentGroupMembers(src.ID)
	assert.Len(t, srcMembers, 1, "source group keeps its member")
}

// Scenario: clone-of-clone strips the existing -c-<N> suffix when
// computing the next default name. team-c-1 cloned should produce
// team-c-2, NOT team-c-1-c-1. Mirrors uniqueCloneTitle for symmetry.
func TestGroupsClone_OfClone_StripsSuffix(t *testing.T) {
	f := newFlow(t)

	const aConv = "aaa-aaaa-bbbb-cccc-1111"
	f.HaveConvWithTitle(aConv, "alice")
	f.HaveAliveSession(aConv, "spwn-a", "tclaude-spwn-a", "/tmp/work")
	f.HaveGroup("team-c-1") // pretend this is already a clone
	f.HaveMember("team-c-1", aConv)

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
	f.HaveMember("team", aConv)
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
	f.HaveMember("team", liveConv)
	f.HaveMember("team", deadConv)

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
