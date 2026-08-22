package agentd

import (
	"slices"
	"sort"
	"testing"
)

// TestPermissionRegistry_OwnerImpliedSet pins the exact ordinary permission
// slugs ownership confers. Group-local grants declare the group dimension;
// the two intentional global bonuses do not.
func TestPermissionRegistry_OwnerImpliedSet(t *testing.T) {
	entries := make(map[string]PermSlug, len(permissionRegistry))
	for _, entry := range permissionRegistry {
		entries[entry.Slug] = entry
	}
	wantGroupScoped := []string{
		PermGroupsMembersReincarnate, PermGroupsMembersCompact,
		PermGroupsMembersInterrupt, PermGroupsMembersRename,
		PermGroupsMembersClone, PermGroupsMembersContextInfo,
		PermGroupsMembersDebugExport,
		PermGroupsMembersTask, PermGroupsMembersPR, PermGroupsMembersTags,
		PermGroupsMembersSchedule, PermGroupsMembersStop,
		PermGroupsMembersResume, PermGroupsMembersDelete,
		PermGroupsMembersPromote, PermGroupsMembersRetire,
		PermGroupsMembersRemoteControl, PermGroupsMembersInboxWatch,
		PermGroupsMembersSpawn, PermGroupsOwnersManage, PermGroupsRename,
		PermGroupsSettingsDescription, PermGroupsSettingsDefaultDir,
		PermGroupsSettingsDefaultContext, PermGroupsSettingsDefaultProfile,
		PermGroupsSettingsMaxMembers, PermGroupsSettingsNotifications,
		PermGroupsSettingsRemoteControlPolicy,
		PermGroupsSettingsMemberPermissions, PermGroupsSettingsOwnerScopes,
		PermGroupsMembersAdd, PermGroupsMembersRemove, PermGroupsMembersUpdate,
		PermGroupsMessagesSchedule, PermGroupsArchive, PermGroupsAttachment,
		PermGroupsLinkAdd, PermGroupsLinkRemove, PermGroupsNest,
		PermGroupsTriggersRead, PermGroupsTriggersManage,
		PermProcessAdvance, PermTemplatesUse,
	}
	want := append([]string{}, wantGroupScoped...)
	want = append(want, PermProcessRunsRead, PermHumanNotify)
	for _, slug := range want {
		if !IsKnownPermSlug(slug) {
			t.Errorf("owner-implied slug %q is not registered", slug)
		}
	}
	for _, slug := range wantGroupScoped {
		entry := entries[slug]
		if !containsScopeDim(entry.ScopeDims, ScopeDimGroup) {
			t.Errorf("owner-implied slug %q does not declare group scope", slug)
		}
	}
	for _, slug := range []string{PermProcessRunsRead, PermHumanNotify} {
		entry := entries[slug]
		if containsScopeDim(entry.ScopeDims, ScopeDimGroup) {
			t.Errorf("global owner bonus %q unexpectedly declares group scope", slug)
		}
	}
	sort.Strings(want)

	got := OwnerImpliedSlugs()
	if len(got) != len(want) {
		t.Fatalf("owner-implied slug count = %d, want %d\n got:  %v\n want: %v",
			len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("owner-implied slug[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	for _, slug := range got {
		if !IsOwnerImpliedSlug(slug) {
			t.Errorf("IsOwnerImpliedSlug(%q) = false, want true", slug)
		}
	}

	wantSibling := map[string]string{
		PermAgentReincarnate:   PermGroupsMembersReincarnate,
		PermAgentCompact:       PermGroupsMembersCompact,
		PermAgentInterrupt:     PermGroupsMembersInterrupt,
		PermAgentRename:        PermGroupsMembersRename,
		PermAgentClone:         PermGroupsMembersClone,
		PermAgentContextInfo:   PermGroupsMembersContextInfo,
		PermAgentDebugExport:   PermGroupsMembersDebugExport,
		PermAgentTask:          PermGroupsMembersTask,
		PermAgentPR:            PermGroupsMembersPR,
		PermAgentTags:          PermGroupsMembersTags,
		PermAgentSchedule:      PermGroupsMembersSchedule,
		PermAgentStop:          PermGroupsMembersStop,
		PermAgentResume:        PermGroupsMembersResume,
		PermAgentDelete:        PermGroupsMembersDelete,
		PermAgentPromote:       PermGroupsMembersPromote,
		PermAgentRetire:        PermGroupsMembersRetire,
		PermAgentRemoteControl: PermGroupsMembersRemoteControl,
		PermAgentInboxWatch:    PermGroupsMembersInboxWatch,
		PermTriggersRead:       PermGroupsTriggersRead,
		PermTriggersManage:     PermGroupsTriggersManage,
	}
	for slug, sibling := range wantSibling {
		if got := GroupSiblingForSlug(slug); got != sibling {
			t.Errorf("GroupSiblingForSlug(%q) = %q, want %q", slug, got, sibling)
		}
	}

	// Slugs that ownership must not confer stay outside the set.
	for _, slug := range []string{
		PermAgentClone,
		PermGroupsCreate,
		PermProcessRunsManage,
		PermProcessTemplatesRead,
		PermProcessTemplatesManage,
	} {
		if IsOwnerImpliedSlug(slug) {
			t.Errorf("IsOwnerImpliedSlug(%q) = true, want false", slug)
		}
	}
}

func TestPermissionRegistry_MemberImpliedSet(t *testing.T) {
	var got []string
	for _, entry := range permissionRegistry {
		if entry.MemberImplied {
			got = append(got, entry.Slug)
		}
	}
	if want := []string{PermGroupsMessagesSchedule}; !slices.Equal(got, want) {
		t.Fatalf("member-implied slugs = %v, want %v", got, want)
	}
}
