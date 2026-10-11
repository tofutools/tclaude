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
