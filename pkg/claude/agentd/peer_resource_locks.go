package agentd

import (
	"fmt"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Resource locks through the federation peer view (tcl-i9elme).
//
// A trusted peer's operator may lock and unlock this node's group resources
// from its own dashboard, but only as far as this node's grants to that peer
// reach: the peer is never this node's operator. Two routes serve it:
//
//   - GET /api/groups/{name}/resources lists a group's processes, boards and
//     pages with their locks, for a peer holding groups.resources.read in the
//     group (tcl-cpsepi). Locally the list is one rule, not one per kind, so
//     one peer grant guards it.
//   - POST /api/resource-locks sets or clears one lock, for a peer holding
//     groups.resources.lock in every group the resource belongs to. A process
//     template's lock holds wherever it is pinned, so every one of those
//     groups must grant it; a resource in no group the peer sees is not found.
//
// Only the peer's operator reaches the peer view (its own dashboard, behind
// its dashboard login); no agent on either node has a route to it, so a
// federated agent never toggles a lock this way. A local agent locks through
// its own route, POST /v1/resource-locks, under its own groups.resources.lock
// grant (group_resource_access.go).

// PermGroupsResourcesLock is the federation peer grant (group-scoped) to lock
// and unlock the group's resources against agent changes from the peer's
// dashboard. Unrestricted trust holds it.
const PermGroupsResourcesLock = "groups.resources.lock"

// PermGroupsResourcesRead (group_resource_access.go) is also the federation
// peer grant (group-scoped) to list the group's resources (its saved
// processes, boards and live pages, with their locks) on the peer's dashboard.
// Unrestricted trust holds it.

// resourceLockGroups returns the groups a resource belongs to: a board's
// group, a page's group, or every group a process template is pinned to. It
// reports false when there is no such resource.
func resourceLockGroups(kind, id string) ([]*db.AgentGroup, bool) {
	byID := func(groupID int64) []*db.AgentGroup {
		if groupID == 0 {
			return nil
		}
		g, err := db.GetAgentGroupByID(groupID)
		if err != nil || g == nil {
			return nil
		}
		return []*db.AgentGroup{g}
	}
	switch kind {
	case resourceKindBoard:
		b, err := db.GetCollabBoard(id)
		if err != nil || b == nil {
			return nil, false
		}
		return byID(b.GroupID), true
	case resourceKindPage:
		livePages.mu.Lock()
		p := livePages.byID[id]
		var owner string
		var groupID int64
		if p != nil {
			owner, groupID = p.owner, p.groupID
		}
		livePages.mu.Unlock()
		if p == nil {
			// A page saved with its group, closed now, keeps its lock.
			row, err := db.GetGroupPage(id)
			if err != nil || row == nil {
				return nil, false
			}
			return byID(row.GroupID), true
		}
		if !pageOwnerInGroup(owner, groupID) {
			return nil, true
		}
		return byID(groupID), true
	case resourceKindProcess:
		pinned, err := db.ProcessTemplatePinnedGroups()
		if err != nil {
			return nil, false
		}
		var out []*db.AgentGroup
		for _, name := range pinned[id] {
			if g, err := db.GetAgentGroupByName(name); err == nil && g != nil {
				out = append(out, g)
			}
		}
		return out, len(out) > 0
	}
	return nil, false
}

// servePeerResourceLock is POST /api/resource-locks through the peer view.
func servePeerResourceLock(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
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
	groups, _ := resourceLockGroups(req.Kind, req.ID)
	var visible []*db.AgentGroup
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			visible = append(visible, g)
		}
	}
	// A resource in no group the peer sees is none of its business, so the
	// answer does not tell it apart from one that does not exist.
	if len(visible) == 0 {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	v.groupName = visible[0].Name
	v.auditDetail = req.Kind // one of the closed kinds checked above
	// Every group it belongs to must grant the lock, including one the peer
	// does not see or one archived (it can be unarchived with the lock): a
	// template's lock reaches every group it is pinned to.
	for _, g := range groups {
		if fedPeerAllows(v.peer.InstanceID, g.ID, rule.requires) {
			continue
		}
		// A group the peer is not shown is not named, nor said to be hidden.
		where := "every group it belongs to"
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			where = "group:" + g.Name
		}
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("this node does not grant you %s for %s", rule.requires, where))
		return
	}
	applyResourceLock(w, r, req)
}

// peerGroupResources is one group's resources as a peer sees them.
type peerGroupResources struct {
	Group     string              `json:"group"`
	Processes []peerGroupProcess  `json:"processes"`
	Boards    []processGroupBoard `json:"boards"`
	Pages     []processGroupPage  `json:"pages"`
	CanLock   bool                `json:"canLock"`
}

type peerGroupProcess struct {
	TemplateID   string `json:"templateId"`
	Name         string `json:"name"`
	AgentsLocked bool   `json:"agentsLocked,omitempty"`
}

// servePeerGroupResources is GET /api/groups/{name}/resources through the
// peer view.
func servePeerGroupResources(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	g, err := db.GetAgentGroupByName(r.PathValue("name"))
	if err != nil || g == nil || g.IsArchived() || !fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	v.groupName = g.Name
	if !v.allows(rule, g.ID) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for group:%s", rule.requires, g.Name))
		return
	}
	boards, pages := groupResources()
	out := peerGroupResources{
		Group:     g.Name,
		Processes: []peerGroupProcess{},
		Boards:    boards[g.ID],
		Pages:     pages[g.ID],
		CanLock:   fedPeerAllows(v.peer.InstanceID, g.ID, PermGroupsResourcesLock),
	}
	if out.Boards == nil {
		out.Boards = []processGroupBoard{}
	}
	if out.Pages == nil {
		out.Pages = []processGroupPage{}
	}
	for _, p := range processSavedTemplates().groupProcesses(r.Context(), g.ID) {
		out.Processes = append(out.Processes, peerGroupProcess{TemplateID: p.TemplateID, Name: p.Name, AgentsLocked: p.AgentsLocked})
	}
	writeJSON(w, http.StatusOK, out)
}
