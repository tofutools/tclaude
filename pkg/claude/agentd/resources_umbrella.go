package agentd

import (
	"net/http"
)

// Resource umbrellas: resources.read and resources.write cover every kind of
// resource at once (drawing boards, process templates, and the runs of a template),
// for a manager agent that looks after resources across groups. They are the
// privileged counterpart of groups.resources.read/write, which membership
// confers inside one group:
//
//   - Never member- or owner-implied, never default-granted: only an explicit
//     grant (or sudo) of an umbrella confers it. resources.write implies
//     resources.read, below a deny of resources.read.
//   - A grant may be scoped with the ordinary scope system: group= reaches the
//     resources linked to those groups, resource=kind:id the named resources,
//     and no scope means every resource.
//   - A dedicated deny of a per-kind slug still wins over the umbrella, as it
//     does over groups.admin.
//   - Neither umbrella passes an operator lock: only groups.resources.lock or
//     the human unlocks, and refuseLockedForAgent does not read these slugs.
//   - Run mutations stay on process.runs.manage: approving a process gate is a
//     different power from editing content.

const (
	PermResourcesRead  = "resources.read"
	PermResourcesWrite = "resources.write"
)

// Resource kinds as a resource= scope names them (resource=drawing_board:plan).
const (
	resourceScopeKindDrawingBoard    = "drawing_board"
	resourceScopeKindProcessTemplate = "process_template"
)

// resourceUmbrellaKind is one resource kind the umbrellas cover: the per-kind
// slugs they imply. A kind added here joins both umbrellas and the resource=
// scope on the same commit, so one grant keeps covering every kind.
type resourceUmbrellaKind struct {
	kind  string
	read  []string
	write []string
}

var resourceUmbrellaKinds = []resourceUmbrellaKind{
	{kind: resourceScopeKindDrawingBoard, read: []string{PermDrawingBoardsRead}, write: []string{PermDrawingBoardsWrite}},
	// A run is read as part of its template: resource=process_template:x
	// reaches x's runs, group=g the runs scoped to g.
	{kind: resourceScopeKindProcessTemplate,
		read:  []string{PermProcessTemplatesRead, PermProcessRunsRead},
		write: []string{PermProcessTemplatesManage}},
}

// resourceUmbrellaFor is the umbrella that implies slug, "" for none.
func resourceUmbrellaFor(slug string) string {
	for _, k := range resourceUmbrellaKinds {
		for _, s := range k.read {
			if s == slug {
				return PermResourcesRead
			}
		}
		for _, s := range k.write {
			if s == slug {
				return PermResourcesWrite
			}
		}
	}
	return ""
}

// permissionUmbrellaFor is the umbrella slug that implies slug, "" for none.
// The two umbrella families never overlap: groups.admin covers the groups.*
// namespace, the resource umbrellas the per-kind resource slugs. resources.write
// implies resources.read (a manager that may change a resource may read it),
// but only the umbrella does: drawing-boards.write still does not imply drawing-boards.read.
// A deny of resources.read wins over that step, as any dedicated deny does.
func permissionUmbrellaFor(slug string) string {
	if IsGroupsAdminImpliedSlug(slug) {
		return PermGroupsAdmin
	}
	if slug == PermResourcesRead {
		return PermResourcesWrite
	}
	// Managing a board covers its columns (tk_bf5d2b02).
	if slug == PermTaskboardColumns {
		return PermTaskboardManage
	}
	// Board options: changing them covers reading them, and managing the
	// board covers changing them; a member's own-board write covers its
	// own-board read (taskboard_options_access.go).
	switch slug {
	case PermTaskboardOptionsRead:
		return PermTaskboardOptionsWrite
	case PermTaskboardOptionsWrite:
		return PermTaskboardManage
	case PermTaskboardOptionsOwnRead:
		return PermTaskboardOptionsOwnWrite
	}
	return resourceUmbrellaFor(slug)
}

// isResourceScopeKind reports whether kind is one a resource= matcher may name.
func isResourceScopeKind(kind string) bool {
	for _, k := range resourceUmbrellaKinds {
		if k.kind == kind {
			return true
		}
	}
	return false
}

// resourceRef is the resource= value for one resource, "" when id is empty.
func resourceRef(kind, id string) string {
	if id == "" {
		return ""
	}
	return kind + ":" + id
}

// resourceUmbrellaContexts are the action contexts a scoped umbrella grant is
// evaluated against for one resource: one per group it is linked to, each
// naming the resource, or one naming only the resource when it has no group.
// structuralGroup stays empty: an umbrella grant is never a membership or
// ownership source.
func resourceUmbrellaContexts(groups []string, resource string) []ActionContext {
	if len(groups) == 0 {
		return []ActionContext{{Resource: resource}}
	}
	ctxs := make([]ActionContext, 0, len(groups))
	for _, g := range groups {
		ctxs = append(ctxs, ActionContext{Group: g, Resource: resource})
	}
	return ctxs
}

// scopedResourceUmbrella is the calling agent's verdict on global's umbrella
// when a SCOPED umbrella grant is what may speak for global, and ok false
// otherwise. An unscoped allow of global (the umbrella's included) is the
// ordinary gate's to answer, and a deny of global wins over the umbrella.
// The umbrella is resolved on its own, so a scoped grant of global itself
// (process.runs.read takes a peer= scope) does not hide it.
func scopedResourceUmbrella(r *http.Request, conv, global string) (permVerdict, bool) {
	umbrella := resourceUmbrellaFor(global)
	if conv == "" || umbrella == "" {
		return permVerdict{}, false
	}
	exact := resolvePermissionVerdictForRequest(r, conv, global)
	if exact.Resolution == permDeny || contextFreeResolution(exact) == permAllow {
		return permVerdict{}, false
	}
	v := resolvePermissionVerdictForRequest(r, conv, umbrella)
	if v.Resolution != permAllow || contextFreeResolution(v) == permAllow {
		return permVerdict{}, false
	}
	return v, true
}

// umbrellaVerdictReaches reports whether a scoped umbrella verdict covers a
// resource in groups: a read through any one of its groups, a write through
// every one (a change reaches all of them).
func umbrellaVerdictReaches(v permVerdict, conv, global string, groups []string, resource string, write bool) (bool, string) {
	matched := ""
	for _, ctx := range resourceUmbrellaContexts(groups, resource) {
		ok, m := permissionVerdictAllowsAction(v, conv, global, ctx)
		if ok && !write {
			return true, m
		}
		if !ok && write {
			return false, ""
		}
		if matched == "" {
			matched = m
		}
	}
	return write, matched
}

// agentReachesThroughScopedUmbrella reports, silently, whether the calling
// agent reaches a resource through a scoped resources.read/write grant, and
// on a pass records the umbrella as the slug that authorized the request. It
// records the umbrella, not global, so a later check that asks whether the
// request was authorized by the per-kind slug itself (a template save's pin
// and listing) does not mistake a scoped grant for the whole slug.
func agentReachesThroughScopedUmbrella(r *http.Request, conv, global string, groups []string, resource string, write bool) bool {
	v, ok := scopedResourceUmbrella(r, conv, global)
	if !ok {
		return false
	}
	reached, matched := umbrellaVerdictReaches(v, conv, global, groups, resource, write)
	if !reached {
		return false
	}
	umbrella := resourceUmbrellaFor(global)
	recordAuditPermissionScope(r, umbrella, matched)
	recordAuthorizedPermission(r, umbrella, v.SudoGrantID)
	return true
}
