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
//   - requireGroupPermission     → groups.{stop,resume,retire,spawn,attachment}
//   - groups_links owner bypass  → groups.link.{add,rm}
//   - requireCrossAgentPermission/requireGroupContextAccess →
//     agent.{reincarnate,compact,rename,clone,context-info,task,pr,tags,
//     schedule,stop,resume,delete,promote,retire}
//   - requireNotifyHumanPermission → human.notify
//   - requireProcessRunReadPermission → process.runs.read
//
// Known gap: templates.instantiate ALSO gets a group-owner bypass, but
// only at the rebrief endpoint (rebrief.go) — its other call site creates
// a new group, which ownership cannot scope. Marking the slug wholesale
// would overstate there, so it stays unmarked and an owner's rebrief
// right goes unlisted. Fixing that needs per-(slug, endpoint) scope.
//
// If you add/remove an owner-bypass call site, update both the registry
// scope AND this map — a drift here means the UI lies about what an owner
// can actually do. Slugs gated by plain requirePermission (no owner
// bypass — e.g. groups.create/rm/own, member.*, groups.rename/clone,
// permissions.*, message.direct) must NOT be marked.
func TestPermissionRegistry_OwnerImpliedSet(t *testing.T) {
	// Each slug mapped to the reach its bypass call site actually has.
	// The UI renders this scope verbatim ("via ownership of: dev, qa"),
	// so a wrong entry here tells an owner it can act on groups the gate
	// will refuse.
	wantScope := map[string]ownerScope{
		// requireCrossAgentPermission / requireGroupContextAccess —
		// members of owned groups.
		PermAgentReincarnate: ownerScopeMember,
		PermAgentCompact:     ownerScopeMember,
		PermAgentRename:      ownerScopeMember,
		PermAgentClone:       ownerScopeMember,
		PermAgentContextInfo: ownerScopeMember,
		PermAgentTask:        ownerScopeMember,
		PermAgentPR:          ownerScopeMember,
		PermAgentTags:        ownerScopeMember,
		PermAgentSchedule:    ownerScopeMember,
		PermAgentStop:        ownerScopeMember,
		PermAgentResume:      ownerScopeMember,
		PermAgentDelete:      ownerScopeMember,
		PermAgentPromote:     ownerScopeMember,
		PermAgentRetire:      ownerScopeMember,
		// requireGroupPermission and the other group-scoped endpoints —
		// the owned group itself.
		PermGroupsStop:       ownerScopeGroup,
		PermGroupsResume:     ownerScopeGroup,
		PermGroupsRetire:     ownerScopeGroup,
		PermGroupsSpawn:      ownerScopeGroup,
		PermGroupsAttachment: ownerScopeGroup,
		PermGroupsLinkAdd:    ownerScopeGroup,
		PermGroupsLinkRm:     ownerScopeGroup,
		PermGroupsNest:       ownerScopeGroup,
		PermProcessAdvance:   ownerScopeGroup,
		// ownsAnyGroup — unscoped, because there is no per-group surface
		// to scope them to.
		PermProcessRunsRead: ownerScopeAny,
		PermHumanNotify:     ownerScopeAny,
	}
	want := make([]string, 0, len(wantScope))
	for slug := range wantScope {
		want = append(want, slug)
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
	// The scope itself is the part the UI phrases, so pin it too — a slug
	// silently re-scoped from "member" to "any" would widen what the
	// listing claims without changing the set above.
	for slug, wantScope := range wantScope {
		if got := OwnerScopeForSlug(slug); got != string(wantScope) {
			t.Errorf("OwnerScopeForSlug(%q) = %q, want %q", slug, got, wantScope)
		}
	}
	// A slug with no bypass must report no scope at all — that is what
	// keeps it out of the owner-conferred rows.
	if got := OwnerScopeForSlug(PermGroupsCreate); got != "" {
		t.Errorf("OwnerScopeForSlug(%q) = %q, want \"\" (no owner bypass)", PermGroupsCreate, got)
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
	// Clearly-not-owner-implied slugs stay false. The process mutation slugs
	// are listed explicitly: ownership confers the process.runs.read READ,
	// and must never widen into template authoring or run execution.
	for _, s := range []string{
		PermGroupsCreate,
		PermProcessRunsManage,
		PermProcessTemplatesRead,
		PermProcessTemplatesManage,
	} {
		if IsOwnerImpliedSlug(s) {
			t.Errorf("IsOwnerImpliedSlug(%q) = true, want false (no owner bypass)", s)
		}
	}
}
