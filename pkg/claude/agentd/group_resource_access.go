package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Group resource access: a group's members read and change the resources
// linked to that group (today its collab boards) without a global slug.
//
// groups.resources.read and groups.resources.write are group-scoped and
// MemberImplied: active membership in the resource's group confers them as a
// structural source, evaluated live at every request, so joining or leaving
// the group takes effect at once. A same-slug explicit deny wins over the
// membership source, as it does over ownership. write implies read.
//
// The global boards.read / boards.write keep covering every board, and a
// board linked to no group (or to an archived one) is reachable through them
// only. An operator lock on a resource (group_resource_locks.go) still refuses
// an agent's change, whatever the source of its access.

const (
	PermGroupsResourcesRead  = "groups.resources.read"
	PermGroupsResourcesWrite = "groups.resources.write"
)

// groupResourceSlugs are the group slugs that grant an access, widest last:
// write implies read.
func groupResourceSlugs(write bool) []string {
	if write {
		return []string{PermGroupsResourcesWrite}
	}
	return []string{PermGroupsResourcesRead, PermGroupsResourcesWrite}
}

// boardGlobalSlug is the global slug for an access to any board.
func boardGlobalSlug(write bool) string {
	if write {
		return PermBoardsWrite
	}
	return PermBoardsRead
}

// silentAgentAllows reports whether the confirmed, non-retired agent behind r
// holds slug for actx, without writing a response. On a pass it records the
// slug on the request the way the shared gate does.
func silentAgentAllows(r *http.Request, convID, slug string, actx ActionContext) bool {
	allowed, matched, err := permissionAllowsAction(r, convID, slug, actx)
	if err != nil || !allowed {
		return false
	}
	recordAuditPermissionScope(r, slug, matched)
	recordAuthorizedPermission(r, slug, loadBearingSudoGrantID(r, convID, slug, actx))
	return true
}

// confirmedAgentConv is the calling agent's conv id when the request comes
// straight from a confirmed, live agent; "" for everything else (the operator,
// a peer, an unidentified or retired caller), which the shared gate answers.
func confirmedAgentConv(r *http.Request) string {
	if peerActionFromRequest(r) != nil || peerGrantFromRequest(r) != nil {
		return ""
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		return ""
	}
	if state, err := db.AgentState(p.ConvID); err != nil || state == db.AgentStateRetired {
		return ""
	}
	return p.ConvID
}

// groupResourceContext is the action context for a resource in group: it
// selects the group as the membership subject.
func groupResourceContext(group string) ActionContext {
	return ActionContext{Group: group, structuralGroup: group}
}

// agentHoldsGroupResource reports, silently, whether the agent may read (or
// write) a resource of group through its group slugs.
func agentHoldsGroupResource(r *http.Request, convID, group string, write bool) bool {
	if group == "" {
		return false
	}
	// write implies read, but not past an explicit deny of read: the write
	// slug's membership source would otherwise bring read straight back.
	if !write && resolvePermissionVerdictForRequest(r, convID, PermGroupsResourcesRead).Resolution == permDeny {
		return false
	}
	for _, slug := range groupResourceSlugs(write) {
		if silentAgentAllows(r, convID, slug, groupResourceContext(group)) {
			return true
		}
	}
	return false
}

// requireGroupResourceAccess gates an access to a resource that may be linked
// to group ("" for none). It passes the operator, an agent holding the global
// slug, and an agent holding the group slug in that group. Anything else gets
// the shared gate's answer (a popup with --ask-human, else 403), asking for
// the group slug when the resource has a group and the caller has no explicit
// verdict on the global one, and for the global slug otherwise.
func requireGroupResourceAccess(w http.ResponseWriter, r *http.Request, global, group string, write bool) (string, bool) {
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if conv == "" || group == "" {
		return requirePermissionEx(w, r, global)
	}
	if silentAgentAllows(r, conv, global, ActionContext{}) || agentHoldsGroupResource(r, conv, group, write) {
		return conv, true
	}
	if resolvePermissionVerdictForRequest(r, conv, global).Resolution != permUndecided {
		return requirePermissionEx(w, r, global)
	}
	slug := PermGroupsResourcesRead
	if write {
		slug = PermGroupsResourcesWrite
	}
	return requirePermissionEx(w, r, slug, groupResourceContext(group))
}

// boardGroupName is the active group a stored board is linked to, "" when it
// has none (GroupName is "" for an archived group, too) or does not exist.
func boardGroupName(id string) string {
	b, err := db.GetCollabBoard(id)
	if err != nil || b == nil {
		return ""
	}
	return b.GroupName
}

// requireBoardAccess gates a read (or a write) of one board.
func requireBoardAccess(w http.ResponseWriter, r *http.Request, id string, write bool) (string, bool) {
	return requireGroupResourceAccess(w, r, boardGlobalSlug(write), boardGroupName(id), write)
}
