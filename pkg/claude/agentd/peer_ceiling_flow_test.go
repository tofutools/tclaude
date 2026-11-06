package agentd_test

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The peer ceiling: a local row a peer confers is covered only by the
// peer's own grants, group by group. Each case below is one condition of the
// ceiling: weakening that condition flips the case.
func TestPeerCeiling(t *testing.T) {
	h := newPeerProcessHarness(t)
	h.f.HaveGroup("other")
	h.f.HaveGroup("attic")
	peer := h.peer.id.ID()
	covers := func(slug string, scope map[string][]string) error {
		return agentd.PeerCeilingCoversForTest(peer, slug, scope)
	}
	crew := map[string][]string{"group": {"crew"}}

	// No grant, no ceiling.
	require.Error(t, covers(agentd.PermGroupsRosterRead, crew))

	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	require.NoError(t, covers(agentd.PermGroupsRosterRead, crew), "the grant held in the one group the row names")
	require.Error(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"other"}}), "a group the peer holds nothing in")
	require.Error(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"crew", "other"}}), "every named group, not the first")
	require.Error(t, covers(agentd.PermGroupsRosterRead, nil), "no group scope reaches every group")
	require.Error(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"nope"}}), "an unknown group")
	require.Error(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"crew"}, "peer": {"bob"}}), "a row reaching another node")
	h.grant(t, agentd.PermGroupsRosterRead, "")
	require.NoError(t, covers(agentd.PermGroupsRosterRead, nil), "held unscoped covers every group")
	require.NoError(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"crew", "other"}}))

	// An alias is held through its peer grant.
	require.Error(t, covers(agentd.PermGroupsMembersRename, crew))
	h.grant(t, agentd.PermGroupsMembersManage, "group=crew")
	require.NoError(t, covers(agentd.PermGroupsMembersRename, crew))
	require.Error(t, covers(agentd.PermGroupsMembersRename, map[string][]string{"group": {"other"}}))

	// An instance-wide grant is held unscoped and takes no group scope.
	require.Error(t, covers(agentd.PermPromptsManage, nil))
	h.grant(t, agentd.PermPromptsManage, "")
	require.NoError(t, covers(agentd.PermPromptsManage, nil))
	require.Error(t, covers(agentd.PermPromptsManage, crew), "prompts.manage takes no group scope")

	// An archived group answers no peer grant, so it covers nothing.
	h.grant(t, agentd.PermGroupsPresenceRead, "group=attic")
	require.NoError(t, covers(agentd.PermGroupsPresenceRead, map[string][]string{"group": {"attic"}}))
	require.NoError(t, db.ArchiveAgentGroup("attic"))
	err := covers(agentd.PermGroupsPresenceRead, map[string][]string{"group": {"attic"}})
	require.ErrorContains(t, err, "not an active group")

	// Held, but never conferrable: the peer route bounds what the local gate
	// does not, the peer grant carries a policy, or it is no local gate.
	for slug, scope := range map[string]string{
		agentd.PermGroupsMembersStop:        "group=crew",
		agentd.PermGroupsMembersRetire:      "group=crew",
		agentd.PermGroupsMembersDebugExport: "group=crew",
		agentd.PermGroupsMembersSpawn:       "group=crew",
		agentd.PermGroupsMembersCompact:     "group=crew",
	} {
		h.grant(t, slug, scope)
		require.Error(t, covers(slug, crew), slug)
		require.Error(t, covers(slug, nil), slug)
	}
	require.Error(t, covers(agentd.PermAgentStop, nil), "a global gate reaches agents in no group")
	require.Error(t, covers(agentd.PermSelfRename, nil), "a local-only slug")
	require.Error(t, covers("no.such.slug", nil))

	// Unrestricted trust holds every peer grant: its ceiling is the operator's.
	h.trustUnrestricted(t)
	require.NoError(t, covers(agentd.PermSelfRename, nil))
	require.NoError(t, covers(agentd.PermGroupsRosterRead, map[string][]string{"group": {"other"}}))
}

// The slugs a peer may confer are pinned: adding one is a review of what its
// peer route bounds that the local gate does not.
func TestPeerCeilingSlugsArePinned(t *testing.T) {
	got := agentd.PeerCeilingSlugsForTest()
	sort.Strings(got)
	want := []string{
		agentd.PermCostsRead, agentd.PermGroupsArchive, agentd.PermGroupsAttachment,
		agentd.PermGroupsMembersContextInfo, agentd.PermGroupsMembersInterrupt, agentd.PermGroupsMembersPR,
		agentd.PermGroupsMembersRemoteControl, agentd.PermGroupsMembersRename, agentd.PermGroupsMembersTags,
		agentd.PermGroupsMembersTask, agentd.PermGroupsMembersUpdate, agentd.PermGroupsPresenceRead,
		agentd.PermGroupsRename, agentd.PermGroupsResourcesRead,
		agentd.PermGroupsRosterRead, agentd.PermGroupsSettingsDescription,
		agentd.PermGroupsSettingsEnvironment, agentd.PermGroupsSettingsMaxMembers, agentd.PermGroupsSettingsNotifications,
		agentd.PermGroupsSettingsRemoteControlPolicy, agentd.PermMessageAttachments, agentd.PermProcessAdvance,
		agentd.PermPromptsManage, agentd.PermUsageRead,
	}
	sort.Strings(want)
	require.Equal(t, want, got)
}
