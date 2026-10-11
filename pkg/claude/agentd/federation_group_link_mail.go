package agentd

import (
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func groupLinkSenderAllowed(r *http.Request, conv string, l db.FederationGroupLink) bool {
	if l.LocalState != "active" || l.RemoteState != "active" || l.Direction != "out" {
		return false
	}
	g, err := db.GetAgentGroupByID(l.LocalGroupID)
	if err != nil || g == nil || g.IsArchived() {
		return false
	}
	if conv == "" {
		return true
	}
	// A direct deny suppresses structural link authority as for local links.
	if resolvePermissionVerdictForAction(r, conv, PermMessageDirect, ActionContext{RemotePeer: l.Peer, RemoteGroup: l.RemoteGroupName}).Resolution == permDeny {
		return false
	}
	if l.Mode == db.LinkModeOwnersToMembers {
		owner, err := db.IsAgentGroupOwner(g.ID, conv)
		return err == nil && owner
	}
	member, err := db.FindMemberInGroup(g.ID, conv)
	return err == nil && member != nil
}
func outboundGroupLink(r *http.Request, conv, peer, remoteID string) *db.FederationGroupLink {
	links, err := db.ListFederationGroupLinks(0)
	if err != nil {
		return nil
	}
	for _, l := range links {
		if l.Peer == peer && l.RemoteGroupID == remoteID && groupLinkSenderAllowed(r, conv, l) {
			return &l
		}
	}
	return nil
}
func linkMailAuthority(l *db.FederationGroupLink) (*proto.GroupLinkMailAuthority, error) {
	source, err := db.FederationGroupIdentity(l.LocalGroupID)
	if err != nil {
		return nil, err
	}
	return &proto.GroupLinkMailAuthority{ProposalID: l.ProposalID, ItemID: l.ItemID, SourceGroupID: source, GroupID: l.RemoteGroupID}, nil
}
func inboundGroupLink(peer string, a *proto.GroupLinkMailAuthority) *db.AgentGroup {
	if a == nil {
		return nil
	}
	l, err := db.GetFederationGroupLink("xlink_" + a.ProposalID + "_" + a.ItemID)
	if err != nil || l == nil || l.Peer != peer || l.Direction != "in" || l.LocalState != "active" || l.RemoteState != "active" || l.RemoteGroupID != a.SourceGroupID {
		return nil
	}
	g, err := db.FederationGroupByIdentity(a.GroupID)
	if err != nil || g == nil || g.IsArchived() || g.ID != l.LocalGroupID || !fedPeerGroupVisible(peer, g.ID) {
		return nil
	}
	return g
}
func (rt *fedRuntime) groupLinkMailCurrent(row db.FederationOutboxRow) bool {
	// Unbound mail keeps the existing permission path unchanged.
	d, err := db.Open()
	if err != nil {
		return false
	}
	var id string
	err = d.QueryRow(`SELECT link_id FROM federation_link_mail WHERE envelope_id=?`, row.EnvelopeID).Scan(&id)
	if err != nil { // Distinguish a legacy/unbound row from a broken read.
		l, e := db.FederationMailLink(row.EnvelopeID)
		return e == nil && l == nil
	}
	l, err := db.GetFederationGroupLink(id)
	return err == nil && l != nil && l.Peer == row.ToInstance && groupLinkSenderAllowed(nil, row.FromConv, *l)
}
func groupLinkByRemoteName(r *http.Request, conv string, peer *db.FederationPeer, name string) *db.FederationGroupLink {
	cat, _, err := fedCatalogFor(peer.InstanceID)
	if err != nil || cat == nil {
		return nil
	}
	for _, g := range cat.Groups {
		if strings.EqualFold(g.Name, name) {
			return outboundGroupLink(r, conv, peer.InstanceID, g.ID)
		}
	}
	return nil
}

func remoteLinkJSON(groupID int64, dir string, names map[int64]string) []linkJSON {
	out := []linkJSON{}
	links, err := db.ListFederationGroupLinks(groupID)
	if err != nil {
		return out
	}
	for _, l := range links {
		if dir != "" && dir != "both" && dir != l.Direction {
			continue
		}
		peer, _ := db.GetFederationPeer(l.Peer)
		label := l.Peer
		if peer != nil {
			label = peerDisplay(peer)
		}
		from, to := names[l.LocalGroupID], l.RemoteGroupName+"@"+label
		if l.Direction == "in" {
			from, to = to, from
		}
		out = append(out, linkJSON{ID: l.ID, From: from, To: to, Mode: l.Mode, CreatedAt: l.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"), Peer: l.Peer, Direction: l.Direction, LocalState: l.LocalState, RemoteState: l.RemoteState})
	}
	return out
}

func handleCanMessageFederated(w http.ResponseWriter, r *http.Request, conv, to string) bool {
	if group, peer, ok := splitFederatedGroup(to); ok {
		if l := groupLinkByRemoteName(r, conv, peer, group); l != nil {
			g, _ := db.GetAgentGroupByID(l.LocalGroupID)
			via := ""
			if g != nil {
				via = g.Name
			}
			writeJSON(w, 200, canMessageResp{Allowed: true, Reason: "via-peer-link", ViaGroup: via, LinkID: l.ID, Message: "Exact directed link; peer rechecks live ingress at delivery"})
			return true
		}
		writeJSON(w, 200, canMessageResp{Allowed: false, Message: "No active directed link from your current groups reaches this peer group"})
		return true
	}
	if !isFederatedAddress(to) {
		return false
	}
	target, err := resolveFederatedTarget(conv, to)
	if err != nil {
		writeJSON(w, 200, canMessageResp{Allowed: false, Message: err.Error()})
		return true
	}
	for _, group := range target.remoteGroup {
		if l := groupLinkByRemoteName(r, conv, target.peer, group); l != nil {
			g, _ := db.GetAgentGroupByID(l.LocalGroupID)
			via := ""
			if g != nil {
				via = g.Name
			}
			writeJSON(w, 200, canMessageResp{Allowed: true, Reason: "via-peer-link", ViaGroup: via, LinkID: l.ID})
			return true
		}
		if allowed, _, _ := permissionAllowsAction(r, conv, PermMessageDirect, ActionContext{RemotePeer: target.peer.InstanceID, RemoteGroup: group}); allowed {
			writeJSON(w, 200, canMessageResp{Allowed: true, Reason: "peer-grant", ViaGroup: group})
			return true
		}
	}
	writeJSON(w, 200, canMessageResp{Allowed: false, Message: "No peer-scoped mail grant or active directed link reaches this target"})
	return true
}
