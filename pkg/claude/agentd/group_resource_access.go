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
// to group ("" for none); resource is its resource= value (kind:id, "" for an
// access that names no one resource, such as a create or a group's listing).
// It passes the operator, an agent holding the global slug (a scoped
// resources.read/write grant included, when its scope reaches the resource),
// and an agent holding the group slug in that group. Anything else gets
// the shared gate's answer (a popup with --ask-human, else 403), asking for
// the group slug when the resource has a group and the caller has no explicit
// verdict on the global one, and for the global slug otherwise.
func requireGroupResourceAccess(w http.ResponseWriter, r *http.Request, global, group, resource string, write bool) (string, bool) {
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	var groups []string
	if group != "" {
		groups = []string{group}
	}
	if agentReachesThroughScopedUmbrella(r, conv, global, groups, resource, write) {
		return conv, true
	}
	if conv == "" || group == "" {
		return requireGlobalAndRecord(w, r, global)
	}
	if silentAgentAllows(r, conv, global, ActionContext{}) || agentHoldsGroupResource(r, conv, group, write) {
		return conv, true
	}
	if resolvePermissionVerdictForRequest(r, conv, global).Resolution != permUndecided {
		return requireGlobalAndRecord(w, r, global)
	}
	slug := PermGroupsResourcesRead
	if write {
		slug = PermGroupsResourcesWrite
	}
	return requirePermissionEx(w, r, slug, groupResourceContext(group))
}

// requireGlobalAndRecord runs the shared gate on the global slug and, when it
// passes an agent, records the slug as the one that authorized the request,
// so a later check (a save's metadata) does not ask the human again.
func requireGlobalAndRecord(w http.ResponseWriter, r *http.Request, global string) (string, bool) {
	conv, ok := requirePermissionEx(w, r, global)
	if ok && conv != "" {
		recordAuthorizedPermission(r, global, 0)
	}
	return conv, ok
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
	return requireGroupResourceAccess(w, r, boardGlobalSlug(write), boardGroupName(id), resourceRef(resourceScopeKindBoard, id), write)
}

// resourceLockContexts are the action contexts the lock slug must hold in for
// a resource: one per group it belongs to, or a single empty one (which only
// an unscoped grant satisfies) for a resource in no group. found is false when
// there is no such resource.
func resourceLockContexts(kind, id string) (ctxs []ActionContext, found bool) {
	groups, found := resourceLockGroups(kind, id)
	if kind == resourceKindProcess {
		// A template pinned nowhere is still one; applyResourceLock checks
		// that it exists.
		found = true
	}
	for _, g := range groups {
		ctxs = append(ctxs, groupResourceContext(g.Name))
	}
	if len(ctxs) == 0 {
		ctxs = []ActionContext{{}}
	}
	return ctxs, found
}

// agentHoldsResourceLock reports, silently, whether the calling agent holds
// groups.resources.lock for the resource: in every group it belongs to. Such
// an agent may change the resource while it is locked.
func agentHoldsResourceLock(r *http.Request, kind, id string) bool {
	conv := confirmedAgentConv(r)
	if conv == "" {
		return false
	}
	ctxs, _ := resourceLockContexts(kind, id)
	for _, ctx := range ctxs {
		if allowed, _, err := permissionAllowsAction(r, conv, PermGroupsResourcesLock, ctx); err != nil || !allowed {
			return false
		}
	}
	return true
}

// POST /v1/resource-locks {kind, id, locked} — an agent locks or unlocks a
// resource. It needs groups.resources.lock in every group the resource
// belongs to (an unscoped grant for one in no group); the operator always
// passes. kind is process, board or page, as on /api/resource-locks.
func handleAgentResourceLock(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeResourceLock(w, r)
	if !ok {
		return
	}
	switch req.Kind {
	case resourceKindProcess, resourceKindBoard, resourceKindPage:
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "kind must be process, board or page")
		return
	}
	ctxs, found := resourceLockContexts(req.Kind, req.ID)
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such "+req.Kind)
		return
	}
	for _, ctx := range ctxs {
		if _, ok := requirePermission(w, r, PermGroupsResourcesLock, ctx); !ok {
			return
		}
	}
	applyResourceLock(w, r, req)
}

// GET /v1/groups/{name}/resources — a group's processes, boards and pages
// with their locks, for a member (groups.resources.read) or the operator.
// canLock says whether the caller may lock and unlock the group's boards and
// pages; each process carries its own, since a template pinned to several
// groups needs the grant in each.
func handleAgentGroupResources(w http.ResponseWriter, r *http.Request) {
	g, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if g == nil || g.IsArchived() {
		writeError(w, http.StatusNotFound, "not_found", "no active group "+guideText(r.PathValue("name"), 80))
		return
	}
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if conv == "" || !agentHoldsGroupResource(r, conv, g.Name, false) {
		if _, ok := requireGroupPermission(w, r, PermGroupsResourcesRead, g); !ok {
			return
		}
	}
	canLock := conv == "" && callerIsHuman(r)
	if conv != "" {
		canLock, _, _ = permissionAllowsAction(r, conv, PermGroupsResourcesLock, groupResourceContext(g.Name))
	}
	boards, pages := groupResources()
	out := peerGroupResources{
		Group: g.Name, Processes: []peerGroupProcess{}, Boards: boards[g.ID], Pages: pages[g.ID], CanLock: canLock,
	}
	if out.Boards == nil {
		out.Boards = []processGroupBoard{}
	}
	if out.Pages == nil {
		out.Pages = []processGroupPage{}
	}
	for _, p := range processSavedTemplates().groupProcesses(r.Context(), g.ID) {
		// A template's lock needs the grant in every group it is pinned to.
		mayLock := conv == "" && callerIsHuman(r) || conv != "" && agentHoldsResourceLock(r, resourceKindProcess, p.TemplateID)
		out.Processes = append(out.Processes, peerGroupProcess{TemplateID: p.TemplateID, Name: p.Name, AgentsLocked: p.AgentsLocked, CanLock: mayLock})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- process templates and runs ----
//
// A process template pinned to a group is one of that group's resources, and
// so is a run scoped to it. Membership confers:
//   - read: list and show the templates pinned to the group (any one of its
//     groups suffices), and list (?group=), show and read the events of the
//     runs scoped to it;
//   - write: save a new version of a template pinned to the group, when the
//     caller is a member of every group it is pinned to (a save changes it
//     for all of them). The lock still refuses it.
// Run mutations (pause, resume, decide, assign, reissue, resolve-blocked,
// record-outcome, rescope) stay on process.runs.manage: they steer other
// agents' work, beside program-profile authorization. Creating, deleting,
// listing in the library and pinning stay on process.templates.manage.

// processTemplateGroups names the active groups a template is pinned to.
func processTemplateGroups(id string) []string {
	pinned, err := db.ProcessTemplatePinnedGroups()
	if err != nil {
		return nil
	}
	var out []string
	for _, name := range pinned[id] {
		if g, err := db.GetAgentGroupByName(name); err == nil && g != nil && !g.IsArchived() {
			out = append(out, g.Name)
		}
	}
	return out
}

// requireGroupsResourceAccess is requireGroupResourceAccess for a resource
// in several groups: a read passes through any one of them, a write needs
// every one. A group= scoped umbrella follows the same rule.
func requireGroupsResourceAccess(w http.ResponseWriter, r *http.Request, global string, groups []string, resource string, write bool) (string, bool) {
	if len(groups) <= 1 {
		group := ""
		if len(groups) == 1 {
			group = groups[0]
		}
		return requireGroupResourceAccess(w, r, global, group, resource, write)
	}
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if agentReachesThroughScopedUmbrella(r, conv, global, groups, resource, write) {
		return conv, true
	}
	if conv == "" {
		return requirePermissionEx(w, r, global)
	}
	if silentAgentAllows(r, conv, global, ActionContext{}) {
		return conv, true
	}
	missing := ""
	for _, g := range groups {
		held := agentHoldsGroupResource(r, conv, g, write)
		if held && !write {
			return conv, true
		}
		if !held && missing == "" {
			missing = g
		}
	}
	if write && missing == "" {
		return conv, true
	}
	if missing == "" {
		missing = groups[0]
	}
	if resolvePermissionVerdictForRequest(r, conv, global).Resolution != permUndecided {
		return requireGlobalAndRecord(w, r, global)
	}
	slug := PermGroupsResourcesRead
	if write {
		slug = PermGroupsResourcesWrite
	}
	return requirePermissionEx(w, r, slug, groupResourceContext(missing))
}

// requireProcessTemplateAccess gates a read (or a save) of one template.
func requireProcessTemplateAccess(w http.ResponseWriter, r *http.Request, id string, write bool) (string, bool) {
	global := PermProcessTemplatesRead
	if write {
		global = PermProcessTemplatesManage
	}
	return requireGroupsResourceAccess(w, r, global, processTemplateGroups(id), resourceRef(resourceScopeKindProcessTemplate, id), write)
}

// readableProcessTemplates is what an agent without the global read lists:
// the templates it reads through its groups, and those a scoped
// resources.read grant reaches.
type readableProcessTemplates struct {
	conv     string
	member   map[string]bool
	groups   map[string][]string
	umbrella permVerdict
	scoped   bool
}

// any reports whether the agent may list anything at all.
func (t readableProcessTemplates) any() bool { return len(t.member) > 0 || t.scoped }

// reads reports whether the agent lists template id.
func (t readableProcessTemplates) reads(id string) bool {
	if t.member[id] {
		return true
	}
	if !t.scoped {
		return false
	}
	ok, _ := umbrellaVerdictReaches(t.umbrella, t.conv, PermProcessTemplatesRead, t.groups[id],
		resourceRef(resourceScopeKindProcessTemplate, id), false)
	return ok
}

// memberReadableProcessTemplates is, for an agent without the global read,
// the templates it lists; limited false when it has the global read or is not
// an agent the membership source applies to.
func memberReadableProcessTemplates(r *http.Request) (readableProcessTemplates, bool) {
	clearAuthorizedPermission(r)
	conv := confirmedAgentConv(r)
	if conv == "" || silentAgentAllows(r, conv, PermProcessTemplatesRead, ActionContext{}) {
		return readableProcessTemplates{}, false
	}
	pinned, err := db.ProcessTemplatePinnedGroups()
	if err != nil {
		return readableProcessTemplates{}, false
	}
	out := readableProcessTemplates{conv: conv, member: map[string]bool{}, groups: map[string][]string{}}
	out.umbrella, out.scoped = scopedResourceUmbrella(r, conv, PermProcessTemplatesRead)
	held := map[string]bool{}
	active := map[string]bool{}
	for id, groups := range pinned {
		for _, name := range groups {
			ok, seen := held[name]
			if !seen {
				g, err := db.GetAgentGroupByName(name)
				active[name] = err == nil && g != nil && !g.IsArchived()
				ok = active[name] && agentHoldsGroupResource(r, conv, name, false)
				held[name] = ok
			}
			if active[name] {
				out.groups[id] = append(out.groups[id], name)
			}
			if ok {
				out.member[id] = true
			}
		}
	}
	return out, true
}

// requireProcessRunRead gates the read of one run: process.runs.read, or the
// group read slug in the group the run is scoped to.
func requireProcessRunRead(w http.ResponseWriter, r *http.Request, runID string) (string, bool) {
	group := ""
	if scope, err := db.GetProcessRunScope(runID); err == nil && scope.Kind == db.ProcessRunScopeGroup {
		group = scope.GroupName
	}
	// A run is read as part of its template (resource=process_template:<id>).
	resource := ""
	if run, err := db.GetProcessRun(runID); err == nil && run != nil {
		resource = resourceRef(resourceScopeKindProcessTemplate, processRunTemplateID(run.TemplateRef))
	}
	return requireGroupResourceAccess(w, r, PermProcessRunsRead, group, resource, false)
}
