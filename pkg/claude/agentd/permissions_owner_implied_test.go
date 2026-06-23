package agentd

import (
	"sort"
	"testing"
)

// TestPermissionRegistry_OwnerImpliedSet pins the EXACT set of slugs the
// registry marks OwnerImplied. The flag drives what the dashboard editor +
// the CLI `permissions ls` show as "held via ownership", so it must stay in
// lockstep with the daemon's actual owner-bypass call sites:
//
//   - requireGroupPermission     → groups.{stop,resume,retire,spawn}
//   - groups_links owner bypass  → groups.link.{add,rm}
//   - requireCrossAgentPermission/requireGroupContextAccess →
//     agent.{reincarnate,compact,rename,clone,context-info,schedule,
//     stop,resume,delete,promote,retire}
//   - requireNotifyHumanPermission → human.notify
//
// If you add/remove an owner-bypass call site, update both the registry
// flag AND this list — a drift here means the UI lies about what an owner
// can actually do. Slugs gated by plain requirePermission (no owner
// bypass — e.g. groups.create/rm/own, member.*, groups.rename/clone,
// permissions.*, message.direct) must NOT be marked.
func TestPermissionRegistry_OwnerImpliedSet(t *testing.T) {
	want := []string{
		PermAgentReincarnate,
		PermAgentCompact,
		PermAgentRename,
		PermAgentClone,
		PermAgentContextInfo,
		PermAgentSchedule,
		PermAgentStop,
		PermAgentResume,
		PermAgentDelete,
		PermAgentPromote,
		PermAgentRetire,
		PermGroupsStop,
		PermGroupsResume,
		PermGroupsRetire,
		PermGroupsSpawn,
		PermGroupsLinkAdd,
		PermGroupsLinkRm,
		PermHumanNotify,
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

	// Every OwnerImplied slug must be a registered slug, and the
	// IsOwnerImpliedSlug helper must agree with the OwnerImpliedSlugs set.
	for _, s := range got {
		if !IsKnownPermSlug(s) {
			t.Errorf("owner-implied slug %q is not in the registry", s)
		}
		if !IsOwnerImpliedSlug(s) {
			t.Errorf("IsOwnerImpliedSlug(%q) = false, want true", s)
		}
	}
	// A clearly-not-owner-implied slug stays false.
	if IsOwnerImpliedSlug(PermGroupsCreate) {
		t.Errorf("IsOwnerImpliedSlug(%q) = true, want false (no owner bypass)", PermGroupsCreate)
	}
}
