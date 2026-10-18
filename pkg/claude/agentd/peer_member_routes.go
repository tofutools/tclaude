package agentd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/notify"
)

// An agent's own controls on the Groups view over the peer view: a peer page
// changes an agent's ordinary settings through this node's own dashboard
// handlers, as it does a group's (peer_grant_routes.go). An agent's settings
// follow it into every group it is in, so the grant must be held in each of
// them, as a stop or a resume must.

const (
	// PermGroupsMembersManage is the federation peer grant (group-scoped) for
	// an agent's ordinary settings from the peer's dashboard: its name, tags,
	// task link and notifications, and its role and description in the group.
	// The agent's own settings need it in every active group the agent is in.
	// Unrestricted trust holds it.
	PermGroupsMembersManage = "groups.members.manage"
)

// peerAgentControlGrants are the grants behind an agent's peer-invoked
// controls, which peer_view.agents names per agent: its settings, and its
// lifecycle and move (peer_actions.go).
var peerAgentControlGrants = []string{PermGroupsMembersManage, PermGroupsMembersStop, PermGroupsMembersResume, PermGroupsMembersRetire, PermGroupsMembersClone, PermAgentMove}

// peerOwnerFootprintGrants are the ones whose footprint also covers the
// groups the agent owns (cloneAuthorizationGroups).
var peerOwnerFootprintGrants = []string{PermGroupsMembersRetire, PermGroupsMembersClone, PermAgentMove}

func addPeerMemberRules(rules map[string]peerViewRule) {
	for _, verb := range []string{"rename", "tags", "task", "notify"} {
		rules["POST /api/agents/{id}/"+verb] = peerViewRule{feature: "members.manage", requires: PermGroupsMembersManage, group: true, write: servePeerAgentInvoke}
	}
	// A member's role and description belong to the one group.
	pattern := "PATCH /api/groups/{name}/members/{conv}"
	rules[pattern] = withPeerInvoke(pattern, peerViewRule{feature: "members.manage", requires: PermGroupsMembersManage, group: true, invoke: &peerInvokeSpec{param: "name"}})
	// An agent's remote access and fast mode, its sandbox implementation, its
	// reincarnation, its removal from a group and its permanent delete reach
	// what no peer grant hands out, as does its debug export: only unrestricted trust holds these, through this node's own
	// handlers. One feature names one slug, so the metadata is stable.
	full := peerViewRule{feature: "agents.manage.full", requires: PermGroupsMembersDelete, unrestrictedOnly: true, write: servePeerFullAgent}
	for _, pattern := range []string{
		"POST /api/agents/{id}/remote-control",
		"POST /api/agents/{id}/fast-mode/{direction}",
		"POST /api/agents/{id}/reincarnate",
		"POST /api/agents/{id}/sandbox-impl",
		"DELETE /api/agents/{id}",
	} {
		rules[pattern] = full
	}
	// The delete dialog reads the agent's worktree first, the sandbox picker
	// the recorded posture.
	read := full
	read.write, read.serve = nil, servePeerFullAgent
	for _, pattern := range []string{
		"GET /api/agents/{id}/worktree",
		"GET /api/agents/{id}/sandbox-impl",
		"GET /api/agents/{id}/debug-export",
	} {
		rules[pattern] = read
	}
	full.write = servePeerFullGroup(groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardRemoveMember(w, g, r.PathValue("conv"))
	}))
	rules["DELETE /api/groups/{name}/members/{conv}"] = full
}

// servePeerFullAgent runs an agent route for unrestricted trust on exactly the
// agent its path names: the handler re-reads the path, so it gets that
// agent's conversation and the route's own verb (an escaped slash in the ID
// names no agent), and the audit names the agent.
func servePeerFullAgent(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	if !v.allows(rule, 0) {
		writeError(w, http.StatusForbidden, "permission", "requires unrestricted trust")
		return
	}
	id := r.PathValue("id")
	target, err := db.GetAgent(id)
	if err != nil || target == nil || target.CurrentConvID == "" {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	v.targetConv = target.CurrentConvID
	req := r.Clone(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true))
	req.URL.Path = "/api/agents/" + url.PathEscape(target.CurrentConvID) + strings.TrimPrefix(r.URL.Path, "/api/agents/"+id)
	req.URL.RawPath = ""
	handleDashboardAgentsAPI(w, req)
}

// servePeerFullGroup is servePeerGrantedWrite for a route on one group,
// recorded in the audit.
func servePeerFullGroup(next http.HandlerFunc) func(http.ResponseWriter, *http.Request, *peerView, peerViewRule) {
	write := servePeerGrantedWrite(next)
	return func(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
		v.groupName = r.PathValue("name")
		write(w, r, v, rule)
	}
}

// servePeerAgentInvoke runs an agent-scoped dashboard route for a peer that
// holds the rule's grant in every active group of an agent it can see. The
// request carries the peer's authority for that one agent, so a shared
// handler's permission gate answers from it (peerActionAuthority).
func servePeerAgentInvoke(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	target, ok := v.visibleAgent(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "object not found")
		return
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, rule.requires) {
		writeError(w, http.StatusForbidden, "permission", fmt.Sprintf("requires %s for every affected group", rule.requires))
		return
	}
	v.targetConv = target.CurrentConvID
	a := &peerActionAuthority{peer: v.peer.InstanceID, sourceConv: target.CurrentConvID, permission: rule.requires}
	ctx := context.WithValue(context.WithValue(r.Context(), remoteAuthedCtxKey{}, true), peerActionKey{}, a)
	// The handler resolves its target from the path again: name the
	// authorized agent's conversation exactly, never a selector that another
	// handle could answer to.
	req := r.Clone(ctx)
	req.URL.Path = "/api/agents/" + url.PathEscape(target.CurrentConvID) + "/" + path.Base(r.URL.Path)
	req.URL.RawPath = ""
	peerDashboardMux().ServeHTTP(w, req)
}

// visibleAgent resolves an agent ID to an agent in a group the peer can see,
// recording that group for the audit.
func (v *peerView) visibleAgent(id string) (*db.Agent, bool) {
	target, err := db.GetAgent(id)
	if err != nil || target == nil || target.CurrentConvID == "" {
		return nil, false
	}
	groups, err := db.ListGroupsForConv(target.CurrentConvID)
	if err != nil {
		return nil, false
	}
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			v.groupName = g.Name
			return target, true
		}
	}
	return nil, false
}

// agentGrants is peerAgentGrants for this request's peer, computed once per
// request (the snapshot rows and the metadata both read it).
func (v *peerView) agentGrants(groups []*db.AgentGroup) map[string][]string {
	if v == nil {
		return nil
	}
	if !v.agentGrantsDone {
		v.agentGrantsValue, v.agentGrantsDone = peerAgentGrants(v.peer.InstanceID, groups), true
	}
	return v.agentGrantsValue
}

// projectPeerMemberSettings fills a member row with the settings its editors
// change, which the peer projection otherwise withholds, where the peer may
// change them: an editor that started blank would replace values the peer
// never saw (tags are a replace-set). The description is the group's
// (groupHeld), the tags and notification override the agent's own
// (agentHeld). A failed read fails the projection: a blank start is what this
// prevents. The effective notification state the toggle shows also tells
// whether one of the agent's groups is muted.
func projectPeerMemberSettings(row *dashboardMember, descr string, groupHeld, agentHeld []string) error {
	if slices.Contains(groupHeld, PermGroupsMembersManage) {
		row.Descr = descr
	}
	if !slices.Contains(agentHeld, PermGroupsMembersManage) {
		return nil
	}
	tags, err := db.ListAgentTags(row.AgentID)
	if err != nil {
		return err
	}
	mode, err := db.GetAgentNotifyPref(row.AgentID)
	if err != nil {
		return err
	}
	row.Tags, row.Notify = tags, mode
	row.NotifyEffective = notify.AllowedForConv(row.ConvID)
	return nil
}

// peerAgentGrants names, per agent in a group the peer can see, the grants of
// peerAgentControlGrants the peer holds in every active group of the agent
// (peerAgentActionAllowed), so its dashboard enables exactly the agent
// controls a click would be allowed to run. A peer holding none of them
// anywhere costs one grant listing. Unrestricted trust holds them all, which
// the metadata's level already says: nothing is listed.
func peerAgentGrants(peer string, groups []*db.AgentGroup) map[string][]string {
	if db.FederationPeerUnrestricted(peer) {
		return nil
	}
	grants, err := db.ListEffectiveFederationPeerGrants(peer)
	if err != nil || !slices.ContainsFunc(grants, func(g db.FederationPeerGrant) bool { return slices.Contains(peerAgentControlGrants, g.Slug) }) {
		return nil
	}
	holds := func(groupID int64, slug string) bool {
		for _, g := range grants {
			if g.Slug == slug && (g.Scope == "" || g.Scope == db.FederationGroupScope(groupID)) {
				return true
			}
		}
		return false
	}
	var ids []int64
	for _, g := range groups {
		if !g.IsArchived() {
			ids = append(ids, g.ID)
		}
	}
	members, err := db.ListAgentGroupMembersBatch(ids)
	if err != nil {
		return nil
	}
	// The batch names each member by its agent's current conversation:
	// count, per conversation, its groups and those holding each grant, and
	// resolve agent IDs only for the ones that qualify.
	held := map[string]map[string]int{}
	groupsOf := map[string]int{}
	seen := map[string]bool{}
	memberOf := map[string]map[int64]bool{}
	count := func(counts map[string]map[string]int, conv string, groupID int64, slugs []string) {
		for _, slug := range slugs {
			if holds(groupID, slug) {
				if counts[conv] == nil {
					counts[conv] = map[string]int{}
				}
				counts[conv][slug]++
			}
		}
	}
	for _, g := range groups {
		if g.IsArchived() {
			continue
		}
		visible := fedPeerGroupVisible(peer, g.ID)
		for _, m := range members[g.ID] {
			groupsOf[m.ConvID]++
			seen[m.ConvID] = seen[m.ConvID] || visible
			if memberOf[m.ConvID] == nil {
				memberOf[m.ConvID] = map[int64]bool{}
			}
			memberOf[m.ConvID][g.ID] = true
			count(held, m.ConvID, g.ID, peerAgentControlGrants)
		}
	}
	// The groups an agent owns without being in them widen its retire and
	// clone footprint; read only when the peer holds one of those anywhere.
	owned, ownedHeld := map[string]int{}, map[string]map[string]int{}
	if slices.ContainsFunc(grants, func(g db.FederationPeerGrant) bool { return slices.Contains(peerOwnerFootprintGrants, g.Slug) }) {
		owners, err := db.ListAgentGroupOwnersBatch(ids)
		if err != nil {
			return nil
		}
		for _, id := range ids {
			for _, o := range owners[id] {
				if o.ConvID == "" || memberOf[o.ConvID][id] {
					continue
				}
				owned[o.ConvID]++
				count(ownedHeld, o.ConvID, id, peerOwnerFootprintGrants)
			}
		}
	}
	out := map[string][]string{}
	for conv, counts := range held {
		if !seen[conv] {
			continue
		}
		var all []string
		for _, slug := range peerAgentControlGrants {
			n, total := counts[slug], groupsOf[conv]
			if slices.Contains(peerOwnerFootprintGrants, slug) {
				n, total = n+ownedHeld[conv][slug], total+owned[conv]
			}
			if n == total {
				all = append(all, slug)
			}
		}
		if len(all) == 0 {
			continue
		}
		if aid, err := db.AgentIDForConv(conv); err == nil && aid != "" {
			out[aid] = all
		}
	}
	return out
}
