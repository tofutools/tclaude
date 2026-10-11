package agentd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func proposalDigest(t *testing.T, p proto.PeerProposalPayload) string {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func sendProposalControl(p *fedPeer, kind string, payload any) string {
	env := p.envelope(kind, proto.Endpoint{}, payload)
	env.From.Agent = ""
	env.From.Name = "operator"
	p.send(env)
	return env.ID
}

func TestFederation_ProposalVisibilityAndDuplicateReceipt(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("offered")
	f.HaveConvWithTitle("proposal-member-00001", "private-roster-name")
	f.HaveMember(g.Name, "proposal-member-00001")
	// A capability advertisement reveals no remote group or roster.
	sendProposalControl(p, proto.KindCatalog, proto.CatalogPayload{PeerProposals: true})
	fedEventually(t, "proposal capability", func() bool {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/proposals", map[string]any{"peer": "bob", "items": []map[string]any{{"kind": proto.ProposalShareVisibility, "host": "us", "group_id": g.Name, "include_members": true}}})
		return rec.Code == http.StatusAccepted
	})
	var offered proto.PeerProposalPayload
	fedEventually(t, "signed proposal", func() bool {
		agentd.FlushFederationOutboxForTest()
		envs := p.envelopes(proto.KindPeerProposal)
		if len(envs) == 0 {
			return false
		}
		require.NoError(t, envs[len(envs)-1].DecodePayload(&offered))
		return true
	})
	require.Equal(t, 1, offered.Items[0].MemberCount)
	encoded, err := json.Marshal(offered)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-roster-name")
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants, "proposal alone must not share")
	decision := proto.PeerProposalDecisionPayload{ID: offered.ID, Revision: offered.Revision, RevisionHash: proposalDigest(t, offered), Decision: "accept", ItemIDs: []string{offered.Items[0].ID}, WithoutMembers: []string{offered.Items[0].ID}}
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "narrowed visibility grant", func() bool { grants, _ = db.ListFederationPeerGrants(p.id.ID()); return len(grants) == 1 })
	require.Equal(t, agentd.PermGroupsVisibilityRead, grants[0].Slug)
	require.False(t, grants[0].SpawnPolicy.IncludeMembers)
	require.Equal(t, db.FederationGroupScope(g.ID), grants[0].Scope)
	// A lost acknowledgement and duplicate decision cannot undo later revocation.
	removed, err := db.DeleteFederationPeerGrant(p.id.ID(), grants[0].Slug, grants[0].Scope)
	require.NoError(t, err)
	require.True(t, removed)
	retry := sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "duplicate decision acknowledged", func() bool {
		for _, ack := range p.envelopes(proto.KindAck) {
			if ack.InReplyTo == retry {
				return true
			}
		}
		return false
	})
	grants, err = db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestFederation_ProposalRequiresHumanAndDurableDeadline(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("incoming")
	gid, err := db.FederationGroupIdentity(g.ID)
	require.NoError(t, err)
	item := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalShareVisibility, Host: p.agentdID, GroupID: gid, GroupName: g.Name, Status: "pending"}
	proposal := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: p.id.ID(), Author: p.id.ID(), Revision: 1, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Items: []proto.ProposalItem{item}}
	p.send(p.envelope(proto.KindPeerProposal, proto.Endpoint{}, proposal))
	fedEventually(t, "agent proposal refusal", func() bool {
		for _, e := range p.envelopes(proto.KindAck) {
			var a proto.AckPayload
			require.NoError(t, e.DecodePayload(&a))
			if a.Status == proto.AckRefused {
				return true
			}
		}
		return false
	})
	row, err := db.GetFederationProposal(proposal.ID)
	require.NoError(t, err)
	require.Nil(t, row)
	sendProposalControl(p, proto.KindPeerProposal, proposal)
	fedEventually(t, "durable incoming proposal", func() bool { row, _ = db.GetFederationProposal(proposal.ID); return row != nil })
	rec := fedHuman(t, f, http.MethodGet, "/v1/federation/proposals/"+proposal.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var view map[string]any
	testharness.DecodeJSON(t, rec, &view)
	require.Equal(t, proposal.Deadline, view["deadline"])
	require.NoError(t, db.ExpireFederationProposals(time.Now().Add(2*time.Hour)))
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+proposal.ID+"/decision", map[string]any{"revision": 1, "decision": "accept", "item_ids": []string{item.ID}})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestFederation_GroupLinkExactAuthorityAndRevocation(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	source := f.HaveGroup("source")
	f.HaveConvWithTitle("link-source-00001", "source-agent")
	f.HaveMember(source.Name, "link-source-00001")
	remoteID := "grp_0123456789abcdef0123456789abcdef"
	sendProposalControl(p, proto.KindCatalog, proto.CatalogPayload{PeerProposals: true, Groups: []proto.CatalogGroup{{ID: remoteID, Name: "builders", Caps: []string{proto.CapGroupVisibility, proto.CapGroupLinkAccept}}}})
	var response struct {
		Edges []struct {
			ID        string `json:"id"`
			Direction string `json:"direction"`
			State     string `json:"state"`
		} `json:"edges"`
	}
	fedEventually(t, "link capable catalog", func() bool { raw, _, _ := db.GetFederationCatalog(p.id.ID()); return strings.Contains(raw, remoteID) })
	created := fedHuman(t, f, http.MethodPost, "/v1/groups/"+source.Name+"/links", map[string]any{"to_peer": "bob", "to_group_id": remoteID, "mode": db.LinkModeMembersToMembers})
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	testharness.DecodeJSON(t, created, &response)
	require.Len(t, response.Edges, 1)
	require.Equal(t, "connecting", response.Edges[0].State)
	var proposal proto.PeerProposalPayload
	fedEventually(t, "link proposal at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		envs := p.envelopes(proto.KindPeerProposal)
		if len(envs) == 0 {
			return false
		}
		require.NoError(t, envs[len(envs)-1].DecodePayload(&proposal))
		return true
	})
	decision := proto.PeerProposalDecisionPayload{ID: proposal.ID, Revision: 1, RevisionHash: proposalDigest(t, proposal), Decision: "accept", ItemIDs: []string{proposal.Items[0].ID}, AppliedIDs: []string{proposal.Items[0].ID}}
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "both link halves active", func() bool {
		l, _ := db.GetFederationGroupLink(response.Edges[0].ID)
		return l != nil && l.LocalState == "active" && l.RemoteState == "active"
	})
	// No message.direct grant was minted. Membership in this source is the path.
	sendLinkedMail := func(body string) string {
		rec := postMessage(t, f, "link-source-00001", map[string]any{"to": "group:builders@bob", "body": body})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var sent struct {
			EnvelopeID string `json:"envelope_id"`
		}
		testharness.DecodeJSON(t, rec, &sent)
		fedEventually(t, "bound group mail", func() bool {
			agentd.FlushFederationOutboxForTest()
			for _, env := range p.envelopes(proto.KindGroupMail) {
				if env.ID == sent.EnvelopeID {
					var mail proto.GroupMailPayload
					require.NoError(t, env.DecodePayload(&mail))
					require.NotNil(t, mail.Link)
					require.Equal(t, remoteID, mail.Link.GroupID)
					return true
				}
			}
			return false
		})
		return sent.EnvelopeID
	}
	retryID := sendLinkedMail("linked group hello")
	deliveredID := sendLinkedMail("delivered original for reply")
	ack := p.envelope(proto.KindAck, proto.Endpoint{}, proto.AckPayload{Status: proto.AckAccepted})
	ack.InReplyTo = deliveredID
	p.send(ack)
	fedEventually(t, "delivered original settled", func() bool {
		row, err := db.GetFederationOutbox(deliveredID)
		return err == nil && row != nil && row.State == db.FedOutboxAccepted
	})
	rec := fedHuman(t, f, http.MethodDelete, "/v1/groups/"+source.Name+"/links/"+response.Edges[0].ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.NoError(t, db.UpdateFederationOutbox(retryID, db.FedOutboxQueued, time.Now().Add(-time.Second), "", 0))
	agentd.FlushFederationOutboxForTest()
	row, err := db.GetFederationOutbox(retryID)
	require.NoError(t, err)
	require.Equal(t, db.FedOutboxRefused, row.State)
	// Even a delivered original must not keep reply authority after revocation.
	aid, err := db.AgentIDForConv("link-source-00001")
	require.NoError(t, err)
	reply := p.envelope(proto.KindMail, proto.Endpoint{Agent: aid}, proto.MailPayload{Body: "reply after revocation"})
	reply.InReplyTo = deliveredID
	p.send(reply)
	require.Equal(t, proto.AckRefused, fedAckOf(t, p, reply.ID).Status)
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "revocation retained", func() bool {
		l, _ := db.GetFederationGroupLink(response.Edges[0].ID)
		return l != nil && l.LocalState == "revoked"
	})
	rec = postMessage(t, f, "link-source-00001", map[string]any{"to": "group:builders@bob", "body": "must be refused"})
	require.NotEqual(t, http.StatusOK, rec.Code)
}

func TestFederation_ProposalCounterChangesLinkEndpoint(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	source := f.HaveGroup("counter-source")
	f.HaveConvWithTitle("counter-source-00001", "source-agent")
	f.HaveMember(source.Name, "counter-source-00001")
	first := "grp_11111111111111111111111111111111"
	second := "grp_22222222222222222222222222222222"
	sendProposalControl(p, proto.KindCatalog, proto.CatalogPayload{PeerProposals: true, Groups: []proto.CatalogGroup{
		{ID: first, Name: "first", Caps: []string{proto.CapGroupVisibility}},
		{ID: second, Name: "second", Caps: []string{proto.CapGroupVisibility}},
	}})
	fedEventually(t, "counter catalog", func() bool { raw, _, _ := db.GetFederationCatalog(p.id.ID()); return strings.Contains(raw, second) })
	created := fedHuman(t, f, http.MethodPost, "/v1/groups/"+source.Name+"/links", map[string]any{"to_peer": "bob", "to_group_id": first, "mode": db.LinkModeMembersToMembers})
	require.Equal(t, http.StatusAccepted, created.Code, created.Body.String())
	var original proto.PeerProposalPayload
	fedEventually(t, "original proposal", func() bool {
		agentd.FlushFederationOutboxForTest()
		envs := p.envelopes(proto.KindPeerProposal)
		if len(envs) == 0 {
			return false
		}
		require.NoError(t, envs[len(envs)-1].DecodePayload(&original))
		return true
	})
	counter := original
	counter.Items = append([]proto.ProposalItem(nil), original.Items...)
	counter.Author, counter.Revision, counter.PreviousHash = p.id.ID(), 2, proposalDigest(t, original)
	counter.Items[0].GroupID, counter.Items[0].GroupName = second, "second"
	sendProposalControl(p, proto.KindPeerProposal, counter)
	fedEventually(t, "counter received", func() bool { row, _ := db.GetFederationProposal(original.ID); return row != nil && row.Revision == 2 })
	accepted := fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+counter.ID+"/decision", map[string]any{"revision": 2, "decision": "accept", "item_ids": []string{counter.Items[0].ID}})
	require.Equal(t, http.StatusAccepted, accepted.Code, accepted.Body.String())
	decision := proto.PeerProposalDecisionPayload{ID: counter.ID, Revision: 2, RevisionHash: proposalDigest(t, counter), Decision: "accept", ItemIDs: []string{counter.Items[0].ID}, AppliedIDs: []string{counter.Items[0].ID}}
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "revised link active", func() bool {
		l, _ := db.GetFederationGroupLink("xlink_" + counter.ID + "_" + counter.Items[0].ID)
		return l != nil && l.LocalState == "active" && l.RemoteState == "active"
	})
	rec := postMessage(t, f, "counter-source-00001", map[string]any{"to": "group:first@bob", "body": "unapproved endpoint"})
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = postMessage(t, f, "counter-source-00001", map[string]any{"to": "group:second@bob", "body": "approved endpoint"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "mail reaches revised destination", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, env := range p.envelopes(proto.KindGroupMail) {
			var mail proto.GroupMailPayload
			require.NoError(t, env.DecodePayload(&mail))
			if mail.Body == "approved endpoint" {
				require.Equal(t, second, mail.Link.GroupID)
				return true
			}
		}
		return false
	})
}

func TestFederation_ProposalCounterRetainsPrivateOfferAndTurn(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	local := f.HaveGroup("share-back")
	sendProposalControl(p, proto.KindCatalog, proto.CatalogPayload{PeerProposals: true})
	fedEventually(t, "private offer capable peer", func() bool {
		raw, _, _ := db.GetFederationCatalog(p.id.ID())
		return strings.Contains(raw, "peer_proposals")
	})
	item := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalShareVisibility, Host: p.id.ID(), GroupID: "grp_33333333333333333333333333333333", GroupName: "private-offer", IncludeMembers: true, MemberCount: 7, Status: "pending"}
	proposal := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: p.id.ID(), Author: p.id.ID(), Revision: 1, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Items: []proto.ProposalItem{item}}
	sendProposalControl(p, proto.KindPeerProposal, proposal)
	fedEventually(t, "private offer received", func() bool { row, _ := db.GetFederationProposal(proposal.ID); return row != nil })
	item.IncludeMembers = false
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+proposal.ID+"/decision", map[string]any{"revision": 1, "decision": "counter", "items": []proto.ProposalItem{item, {Kind: proto.ProposalShareVisibility, Host: "us", GroupID: local.Name}}})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/proposals/"+proposal.ID, nil)
	var view map[string]any
	testharness.DecodeJSON(t, rec, &view)
	require.Equal(t, "them", view["awaiting"])
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+proposal.ID+"/decision", map[string]any{"revision": 2, "decision": "accept", "item_ids": []string{item.ID}})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants, "countering must not share anything")
}

func TestFederation_ProposalPartialAutoIngressAndImmediateRevoke(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("partial-ingress")
	f.HaveConvWithTitle("partial-recipient-0001", "recipient")
	f.HaveMember(g.Name, "partial-recipient-0001")
	gid, err := db.FederationGroupIdentity(g.ID)
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsVisibilityRead, Scope: db.FederationGroupScope(g.ID)}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsLinkAccept, Scope: db.FederationGroupScope(g.ID)}))
	ungranted := f.HaveGroup("manual-ingress")
	ungrantedID, err := db.FederationGroupIdentity(ungranted.ID)
	require.NoError(t, err)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: p.id.ID(), Slug: agentd.PermGroupsVisibilityRead, Scope: db.FederationGroupScope(ungranted.ID)}))
	remote := "grp_44444444444444444444444444444444"
	incoming := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalGroupLink, Host: p.agentdID, GroupID: gid, GroupName: g.Name, Source: p.id.ID(), SourceGroupID: remote, SourceGroupName: "remote", Mode: db.LinkModeMembersToMembers, Status: "pending"}
	outgoing := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalGroupLink, Host: p.id.ID(), GroupID: remote, GroupName: "remote", Source: p.agentdID, SourceGroupID: gid, SourceGroupName: g.Name, Mode: db.LinkModeMembersToMembers, Status: "pending"}
	manual := incoming
	manual.ID, manual.GroupID, manual.GroupName = proto.NewEnvelopeID(), ungrantedID, ungranted.Name
	proposal := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: p.id.ID(), Author: p.id.ID(), Revision: 1, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Items: []proto.ProposalItem{incoming, outgoing, manual}}
	sendProposalControl(p, proto.KindPeerProposal, proposal)
	fedEventually(t, "only granted ingress auto-applies", func() bool {
		l, _ := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + incoming.ID)
		return l != nil && l.LocalState == "active"
	})
	other, err := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + outgoing.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", other.LocalState)
	manualLink, err := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + manual.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", manualLink.LocalState, "a grant for another destination group must not auto-accept this ingress")
	decision := proto.PeerProposalDecisionPayload{ID: proposal.ID, Revision: 1, RevisionHash: proposalDigest(t, proposal), Decision: "accept_items", ItemIDs: []string{incoming.ID}, AppliedIDs: []string{incoming.ID}}
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "ingress acknowledged", func() bool {
		l, _ := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + incoming.ID)
		return l != nil && l.RemoteState == "active"
	})
	authority := &proto.GroupLinkMailAuthority{ProposalID: proposal.ID, ItemID: incoming.ID, SourceGroupID: remote, GroupID: gid}
	mail := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: g.Name, Link: authority, Body: "exact ingress works"})
	p.send(mail)
	require.Equal(t, proto.AckAccepted, fedAckOf(t, p, mail.ID).Status)
	require.Len(t, fedRemoteBodies(t, "partial-recipient-0001"), 1)
	rec := fedHuman(t, f, http.MethodDelete, "/v1/groups/"+g.Name+"/links/xlink_"+proposal.ID+"_"+incoming.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	late := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: g.Name, Link: authority, Body: "revoked ingress"})
	p.send(late)
	require.Equal(t, proto.AckRefused, fedAckOf(t, p, late.ID).Status)
	require.Len(t, fedRemoteBodies(t, "partial-recipient-0001"), 1)
}

func TestFederation_ProposalAcceptShareAndLinkTogether(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("share-and-link")
	gid, err := db.FederationGroupIdentity(g.ID)
	require.NoError(t, err)
	share := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalShareVisibility, Host: p.agentdID, GroupID: gid, GroupName: g.Name, Status: "pending"}
	link := proto.ProposalItem{ID: proto.NewEnvelopeID(), Kind: proto.ProposalGroupLink, Host: p.agentdID, GroupID: gid, GroupName: g.Name, Source: p.id.ID(), SourceGroupID: "grp_55555555555555555555555555555555", SourceGroupName: "remote", Mode: db.LinkModeMembersToMembers, Status: "pending"}
	proposal := proto.PeerProposalPayload{ID: proto.NewEnvelopeID(), Origin: p.id.ID(), Author: p.id.ID(), Revision: 1, Deadline: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), Items: []proto.ProposalItem{link, share}}
	sendProposalControl(p, proto.KindPeerProposal, proposal)
	fedEventually(t, "combined offer", func() bool { row, _ := db.GetFederationProposal(proposal.ID); return row != nil })
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+proposal.ID+"/decision", map[string]any{"revision": 1, "decision": "accept", "item_ids": []string{link.ID, share.ID}})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, agentd.PermGroupsVisibilityRead, grants[0].Slug)
	l, err := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + link.ID)
	require.NoError(t, err)
	require.Equal(t, "active", l.LocalState)
}

func TestFederation_ProposalWithdrawRetainsAppliedEdge(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	g := f.HaveGroup("withdraw-group")
	gid, err := db.FederationGroupIdentity(g.ID)
	require.NoError(t, err)
	remote := "grp_66666666666666666666666666666666"
	sendProposalControl(p, proto.KindCatalog, proto.CatalogPayload{PeerProposals: true, Groups: []proto.CatalogGroup{{ID: remote, Name: "remote", Caps: []string{proto.CapGroupVisibility}}}})
	fedEventually(t, "withdraw catalog", func() bool { raw, _, _ := db.GetFederationCatalog(p.id.ID()); return strings.Contains(raw, remote) })
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/proposals", map[string]any{"peer": "bob", "items": []proto.ProposalItem{
		{Kind: proto.ProposalGroupLink, Host: "them", GroupID: remote, Source: "us", SourceGroupID: gid, Mode: db.LinkModeMembersToMembers},
		{Kind: proto.ProposalShareVisibility, Host: "us", GroupID: gid},
	}})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var proposal proto.PeerProposalPayload
	fedEventually(t, "withdraw offer", func() bool {
		agentd.FlushFederationOutboxForTest()
		envs := p.envelopes(proto.KindPeerProposal)
		if len(envs) == 0 {
			return false
		}
		require.NoError(t, envs[len(envs)-1].DecodePayload(&proposal))
		return true
	})
	decision := proto.PeerProposalDecisionPayload{ID: proposal.ID, Revision: 1, RevisionHash: proposalDigest(t, proposal), Decision: "accept_items", ItemIDs: []string{proposal.Items[0].ID}, AppliedIDs: []string{proposal.Items[0].ID}}
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "partial edge active", func() bool {
		l, _ := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + proposal.Items[0].ID)
		return l != nil && l.RemoteState == "active"
	})
	counter := proposal
	counter.Author, counter.Revision, counter.PreviousHash = p.id.ID(), 2, proposalDigest(t, proposal)
	counter.Note = "retaining the approved edge, negotiating the share"
	sendProposalControl(p, proto.KindPeerProposal, counter)
	fedEventually(t, "counter retains applied receipts", func() bool {
		row, _ := db.GetFederationProposal(proposal.ID)
		if row == nil || row.Revision != 2 {
			return false
		}
		here, remote, err := db.FederationProposalApplied(proposal.ID, 2)
		require.NoError(t, err)
		return len(here) == 1 && len(remote) == 1
	})
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/proposals/"+proposal.ID+"/decision", map[string]any{"revision": 2, "decision": "withdraw"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	l, err := db.GetFederationGroupLink("xlink_" + proposal.ID + "_" + proposal.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, "active", l.LocalState)
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/proposals/"+proposal.ID, nil)
	var view map[string]any
	testharness.DecodeJSON(t, rec, &view)
	require.Equal(t, "withdrawn", view["state"])
	require.Empty(t, view["awaiting"])
	grants, err := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, err)
	require.Empty(t, grants)
}

func TestGroupLinksRemainLocalWithoutFederation(t *testing.T) {
	f := newFlow(t)
	a := f.HaveGroup("local-source")
	b := f.HaveGroup("local-destination")
	f.HaveConvWithTitle("local-source-001", "sender")
	f.HaveConvWithTitle("local-destination-001", "receiver")
	f.HaveMember(a.Name, "local-source-001")
	f.HaveMember(b.Name, "local-destination-001")
	rec := fedHuman(t, f, http.MethodPost, "/v1/groups/"+a.Name+"/links", map[string]any{"to": b.Name, "mode": db.LinkModeMembersToMembers})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var link map[string]any
	testharness.DecodeJSON(t, rec, &link)
	require.IsType(t, float64(0), link["id"], "local IDs retain their numeric wire type")
	rec = fedHuman(t, f, http.MethodGet, "/v1/can-message?from=local-source-001&to=local-destination-001", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var why map[string]any
	testharness.DecodeJSON(t, rec, &why)
	require.Equal(t, true, why["allowed"])
	require.Equal(t, "via-link", why["reason"])
	rec = postMessage(t, f, "local-source-001", map[string]any{"to": "local-destination-001", "body": "ordinary local message"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	proposals, err := db.ListFederationProposals()
	require.NoError(t, err)
	require.Empty(t, proposals)
}
