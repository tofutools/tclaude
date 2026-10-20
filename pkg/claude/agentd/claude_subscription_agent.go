package agentd

import (
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// An agent on a peer operator's Claude subscription loses its token when this
// daemon restarts. Its owner can send the token again
// (POST /api/agents/{id}/claude-subscription through the peer view, which the
// owner's own node fills in); anyone who may relaunch it can instead move it
// to this node's own login (DELETE, here or through the peer view).

// dashboardAgentClaudeSubscription is this node's operator's route: switching
// an agent to this node's own login. Only the owner's node holds the token,
// so there is nothing to send from here.
func dashboardAgentClaudeSubscription(w http.ResponseWriter, r *http.Request, selector string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE only: send the token again from its owner's node")
		return
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil || res.AgentID == "" {
		writeError(w, 404, "not_found", "no such agent")
		return
	}
	switchAgentToOwnLogin(w, res.AgentID, "")
}

func switchAgentToOwnLogin(w http.ResponseWriter, agentID, by string) {
	marker, err := db.GetAgentClaudeSubscription(agentID)
	if err != nil {
		writeError(w, 503, "unavailable", "could not read the agent's Claude subscription marker")
		return
	}
	if marker == nil {
		writeJSON(w, 200, map[string]any{"agent_id": agentID, "switched": false})
		return
	}
	if err := db.DeleteAgentClaudeSubscription(agentID); err != nil {
		writeError(w, 503, "unavailable", "could not switch the agent to this node's login")
		return
	}
	dropClaudeSubscription(agentID)
	recordFederationAudit("federation.claude_subscription.switch_local", marker.Peer, "", "", "agent="+agentID+" by="+firstNonEmpty(by, "operator"), 200)
	writeJSON(w, 200, map[string]any{"agent_id": agentID, "switched": true})
}

// servePeerAgentClaudeSubscription is a peer's route: its owner sends the
// token again (POST), or the owner or an unrestricted peer switches the agent
// to this node's own login (DELETE).
func servePeerAgentClaudeSubscription(w http.ResponseWriter, r *http.Request, v *peerView, rule peerViewRule) {
	target, err := db.GetAgent(r.PathValue("id"))
	if err != nil || target == nil || target.CurrentConvID == "" || !target.Active() {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	groups, err := db.ListGroupsForConv(target.CurrentConvID)
	visible := false
	for _, g := range groups {
		if !g.IsArchived() && fedPeerGroupVisible(v.peer.InstanceID, g.ID) {
			visible = true
			v.groupName = g.Name
			break
		}
	}
	if err != nil || !visible {
		writeError(w, 404, "not_found", "object not found")
		return
	}
	if !peerAgentActionAllowed(v.peer.InstanceID, target.CurrentConvID, rule.requires) {
		writeError(w, 403, "permission", "requires "+rule.requires+" for every affected group")
		return
	}
	v.targetConv = target.CurrentConvID
	marker, err := db.GetAgentClaudeSubscription(target.AgentID)
	if err != nil {
		writeError(w, 503, "unavailable", "could not read the agent's Claude subscription marker")
		return
	}
	owner := marker != nil && marker.Peer == v.peer.InstanceID
	if r.Method == http.MethodDelete {
		if marker != nil && !owner && !db.FederationPeerUnrestricted(v.peer.InstanceID) {
			writeError(w, 403, "permission", "only the subscription's owner, or an unrestricted peer, may switch this agent's login")
			return
		}
		switchAgentToOwnLogin(w, target.AgentID, v.peer.InstanceID)
		return
	}
	if marker == nil {
		writeError(w, 409, "not_subscription", "this agent runs on this node's own login")
		return
	}
	if !owner {
		writeError(w, 403, "permission", "only the operator whose subscription this agent runs on can send it")
		return
	}
	token, ok := liftClaudeSubscriptionToken(w, r)
	if !ok {
		return
	}
	if token == "" {
		writeError(w, 400, "invalid_arg", "no Claude subscription token was sent; update tclaude on the sending node")
		return
	}
	if err := holdClaudeSubscription(target.AgentID, claudeSubscriptionHandoff{peer: v.peer.InstanceID, token: token}); err != nil {
		writeError(w, 503, "unavailable", "could not hold the token")
		return
	}
	recordFederationAudit("federation.claude_subscription.resend", v.peer.InstanceID, target.CurrentConvID, v.groupName, "agent="+target.AgentID, 200)
	writeJSON(w, 200, map[string]any{"agent_id": target.AgentID, "held": true})
}

