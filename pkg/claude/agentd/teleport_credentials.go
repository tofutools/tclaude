package agentd

import (
	"context"
	"net/http"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// teleportCredentials is what a peer's teleport dialog may offer for an
// agent it sends to a group here: the model access modes this node accepts
// from that peer, and the gateways this node can itself use through other
// nodes' shares.
type teleportCredentials struct {
	// Any says every mode this node can resolve is accepted (unrestricted
	// trust); Allowed then lists nothing.
	Any     bool     `json:"any"`
	Allowed []string `json:"allowed"`
	Default string   `json:"default"`
	// RequesterPays is this node's requester-pays policy for a teleport
	// from that peer: off, allowed or required.
	RequesterPays string `json:"requester_pays"`
	// Shares are third nodes' gateways this node may use, as
	// proxy:<gateway>@<instance>. The asking peer's own gateways are never
	// listed: it offers those itself, paid through a lease.
	Shares []modelShare `json:"shares"`
}

// servePeerTeleportCredentials answers GET /api/teleport-credentials?group=G.
func servePeerTeleportCredentials(w http.ResponseWriter, r *http.Request, v *peerView, _ peerViewRule) {
	out := teleportCredentials{Allowed: []string{"local"}, Default: "local", Shares: []modelShare{}}
	var groupID int64
	if name := r.URL.Query().Get("group"); name != "" {
		g, err := db.GetAgentGroupByName(name)
		// Only a group the peer may send agents to is answered for.
		if err != nil || g == nil || !fedPeerAllows(v.peer.InstanceID, g.ID, PermAgentsTeleportReceive) && !fedPeerAllows(v.peer.InstanceID, g.ID, PermAgentsReceive) {
			writeError(w, 404, "not_found", "no such group")
			return
		}
		groupID = g.ID
	}
	if a, err := db.GetFederationNodeProfileAssignment(v.peer.InstanceID); err == nil && a != nil {
		if l := a.Profile.Definition.TeleportLanding; l != nil && (groupID == 0 || l.GroupID == groupID) {
			landing := *l
			if normalizeTeleportLanding(&landing) == nil {
				out.Allowed, out.Default = slices.Clone(landing.CredentialsAllowed), landing.CredentialsDefault
			}
		}
	}
	out.Any = db.FederationPeerUnrestricted(v.peer.InstanceID)
	if out.Any {
		out.Allowed = nil
	}
	if policy, err := requesterPaysPolicy(v.peer.InstanceID, groupID, true); err == nil {
		out.RequesterPays = policy
	}
	rt := currentFederation()
	peers, err := db.ListFederationPeers()
	if rt == nil || err != nil {
		writeJSON(w, 200, out)
		return
	}
	others := peers[:0]
	for _, p := range peers {
		if p.InstanceID != v.peer.InstanceID {
			others = append(others, p)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelSharesReceivedWait)
	defer cancel()
	shares, _ := receivedModelShares(ctx, rt, others)
	for _, s := range shares {
		if s.UsedUp || s.Peer == v.peer.InstanceID {
			continue
		}
		if out.Any || slices.ContainsFunc(out.Allowed, func(m string) bool {
			return sameTeleportCredentialMode(m, "proxy:"+s.Gateway+"@"+s.Peer)
		}) {
			out.Shares = append(out.Shares, s)
		}
	}
	writeJSON(w, 200, out)
}
