package agentd

import (
	"errors"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Sharing a live page within its group (tcl-4hylua).
//
// A page belongs to the agent that showed it. When shown it is associated
// with one agent group: the shower's only group, the one it names with
// --group, or none (--private, or a shower in several groups that names
// none). Another member of that group then acts on the page by its owner and
// name (`tclaude agent page marks <owner>/<name>`), under group-scoped slugs:
//
//	groups.pages.read    marks, snapshot, DOM read, the patch journal
//	groups.pages.patch   add or reset live patches (CSS and DOM)
//	groups.pages.manage  eval and close
//
// read and patch are installed defaults, so sharing within the group is what
// membership implies; manage is not, so by default only the shower runs a
// script in the operator's view of its page or closes it.
//
// The invariants:
//   - Both the caller and the page's owner must be current members of the
//     page's group at the time of the request: leaving the group ends the
//     sharing at once. A page is never reachable from another group.
//   - Only a local agent's current conversation acts (pageCaller); a peer or
//     federated request never does.
//   - The page's read-back rules are its shower's: a static page is read
//     back only when the shower's view is exact (pageReadBackAllowed), which
//     every read path checks the same way for every caller.
//   - A caller that is not a member gets the same 404 as for a page that
//     does not exist, so a page's existence does not leak across groups.

// errPageNotShared is a page another agent showed that the caller may not
// reach: absent, closed, private, or in a group the caller is not in.
var errPageNotShared = errors.New("no such page")

// The link is the group's id, as a board's is (POST /api/boards/{id}/group),
// so it survives a rename, and a group's resources (pages, boards,
// processes) can be listed the same way, by the page view's group_id.

// pageShareGroup resolves the group a page is shown into: the named one,
// which the shower must be in, or the shower's only group (0: none). note
// says why a page is not shared, for show's output.
func pageShareGroup(agentID, named string, private bool) (groupID int64, note string, err error) {
	if private {
		return 0, "private: only you can act on it", nil
	}
	groups, err := pageActiveGroups(agentID)
	if err != nil {
		return 0, "", err
	}
	if named = strings.TrimSpace(named); named != "" {
		for _, g := range groups {
			if g.Name == named {
				return g.ID, "", nil
			}
		}
		return 0, "", errors.New("you are not a member of group " + named)
	}
	switch len(groups) {
	case 0:
		return 0, "not shared: you are in no group", nil
	case 1:
		return groups[0].ID, "", nil
	default:
		return 0, "not shared: you are in several groups; name one with --group", nil
	}
}

// pageActiveGroups are the groups agentID is a direct member of, leaving out
// archived ones: an archived group shares nothing.
func pageActiveGroups(agentID string) ([]*db.AgentGroup, error) {
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		return nil, err
	}
	out := groups[:0]
	for _, g := range groups {
		if g.ArchivedAt.IsZero() {
			out = append(out, g)
		}
	}
	return out, nil
}

// pageGroupOf is the group agentID is a member of now with id groupID, or
// nil.
func pageGroupOf(agentID string, groupID int64) *db.AgentGroup {
	if agentID == "" || groupID == 0 {
		return nil
	}
	groups, err := pageActiveGroups(agentID)
	if err != nil {
		return nil
	}
	for _, g := range groups {
		if g.ID == groupID {
			return g
		}
	}
	return nil
}

// pageGroupName is the current name of a page's group, "" for none.
func pageGroupName(groupID int64) string {
	if groupID == 0 {
		return ""
	}
	g, err := db.GetAgentGroupByID(groupID)
	if err != nil || g == nil {
		return ""
	}
	return g.Name
}

// sharedPage finds a page owner showed that caller may reach through its
// group, and that group: shared, with both agents current members of it.
func sharedPage(caller, owner, name string) (*livePage, *db.AgentGroup, error) {
	livePages.mu.Lock()
	p := livePages.byOwner[owner][name]
	var groupID int64
	if p != nil {
		groupID = p.groupID
	}
	livePages.mu.Unlock()
	if p == nil || groupID == 0 || pageGroupOf(owner, groupID) == nil {
		return nil, nil, errPageNotShared
	}
	g := pageGroupOf(caller, groupID)
	if g == nil {
		return nil, nil, errPageNotShared
	}
	return p, g, nil
}

// groupOwnedPage finds a page the group named groupName owns, for a caller
// that is a current member of that group.
func groupOwnedPage(caller, groupName, name string) (*livePage, *db.AgentGroup, error) {
	g := groupForCaller(caller, groupName)
	if g == nil {
		return nil, nil, errPageNotShared
	}
	livePages.mu.Lock()
	p := livePages.byOwner[groupPageOwner(g.ID)][name]
	livePages.mu.Unlock()
	if p == nil {
		return nil, nil, errPageNotShared
	}
	return p, g, nil
}

// pagesSharedWith lists the pages other agents showed that caller may reach.
func pagesSharedWith(caller string) []*livePage {
	groups, err := pageActiveGroups(caller)
	if err != nil || len(groups) == 0 {
		return nil
	}
	in := map[int64]bool{}
	for _, g := range groups {
		in[g.ID] = true
	}
	var out []*livePage
	for _, p := range livePages.list("") {
		livePages.mu.Lock()
		owner, groupID := p.owner, p.groupID
		livePages.mu.Unlock()
		if owner != caller && in[groupID] && pageOwnerInGroup(owner, groupID) {
			out = append(out, p)
		}
	}
	return out
}

// pageFor finds the page a request names: one of the caller's own (no
// owner, or the caller as owner), or, with ?owner=<agent id>, one another
// member of its group showed, which needs perm scoped to that group. It is
// the one gate every page action passes, owner or not, so a check on the
// page itself (an operator's lock on a group resource) belongs here.
func pageFor(w http.ResponseWriter, r *http.Request, perm string) (*livePage, bool) {
	p, _, _, ok := pageForAs(w, r, perm)
	return p, ok
}

// pageForAs is pageFor, saying also whether the page is another agent's,
// shared with the caller through its group (g: that group), and the
// caller's agent id.
func pageForAs(w http.ResponseWriter, r *http.Request, perm string) (p *livePage, g *db.AgentGroup, caller string, ok bool) {
	_, agentID, ok := pageCaller(w, r)
	if !ok {
		return nil, nil, "", false
	}
	name := r.PathValue("name")
	if !pageNamePattern.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_arg", "invalid page name")
		return nil, nil, "", false
	}
	owner := strings.TrimSpace(r.URL.Query().Get("owner"))
	if owner == "" || owner == agentID {
		livePages.mu.Lock()
		p := livePages.byOwner[agentID][name]
		livePages.mu.Unlock()
		if p == nil {
			writeError(w, http.StatusNotFound, "not_found", "you have no page named "+name)
			return nil, nil, "", false
		}
		if !pageLockAllows(w, r, p, perm) {
			return nil, nil, "", false
		}
		return p, nil, agentID, true
	}
	if peerActionFromRequest(r) != nil {
		writeError(w, http.StatusForbidden, "local_only", "a shared live page is reachable only by local members of its group")
		return nil, nil, "", false
	}
	var err error
	p, g, err = sharedPage(agentID, owner, name)
	if err != nil && !isGroupPageOwner(owner) {
		// <group>/<name>: a page the group itself owns (pages_group.go).
		p, g, err = groupOwnedPage(agentID, owner, name)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no page "+owner+"/"+name+" is shared with you")
		return nil, nil, "", false
	}
	if _, ok := requirePermission(w, r, perm, ActionContext{Group: g.Name}); !ok {
		return nil, nil, "", false
	}
	if !pageLockAllows(w, r, p, perm) {
		return nil, nil, "", false
	}
	setAuditTargetLabel(r, "page "+owner+"/"+name)
	return p, g, agentID, true
}
