package agentd

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
// controls, which peer_view.agents names per agent.
var peerAgentControlGrants = []string{PermGroupsMembersManage}

func addPeerMemberRules(rules map[string]peerViewRule) {
	for _, verb := range []string{"rename", "tags", "task", "notify"} {
		rules["POST /api/agents/{id}/"+verb] = peerViewRule{feature: "members.manage", requires: PermGroupsMembersManage, group: true, write: servePeerAgentInvoke}
	}
	// A member's role and description belong to the one group.
	pattern := "PATCH /api/groups/{name}/members/{conv}"
	rules[pattern] = withPeerInvoke(pattern, peerViewRule{feature: "members.manage", requires: PermGroupsMembersManage, group: true, invoke: &peerInvokeSpec{param: "name"}})
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
	peerDashboardMux().ServeHTTP(w, r.Clone(ctx))
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
	// held[agent][slug] counts the agent's groups holding slug; groupsOf
	// counts its groups, and seen marks an agent the peer can see.
	held := map[string]map[string]int{}
	groupsOf := map[string]int{}
	seen := map[string]bool{}
	agentOf := map[string]string{}
	for _, g := range groups {
		if g.IsArchived() {
			continue
		}
		visible := fedPeerGroupVisible(peer, g.ID)
		for _, m := range members[g.ID] {
			aid, ok := agentOf[m.ConvID]
			if !ok {
				aid, _ = db.AgentIDForConv(m.ConvID)
				agentOf[m.ConvID] = aid
			}
			if aid == "" {
				continue
			}
			groupsOf[aid]++
			seen[aid] = seen[aid] || visible
			for _, slug := range peerAgentControlGrants {
				if holds(g.ID, slug) {
					if held[aid] == nil {
						held[aid] = map[string]int{}
					}
					held[aid][slug]++
				}
			}
		}
	}
	out := map[string][]string{}
	for aid, counts := range held {
		if !seen[aid] {
			continue
		}
		var all []string
		for _, slug := range peerAgentControlGrants {
			if counts[slug] == groupsOf[aid] {
				all = append(all, slug)
			}
		}
		if len(all) > 0 {
			out[aid] = all
		}
	}
	return out
}
