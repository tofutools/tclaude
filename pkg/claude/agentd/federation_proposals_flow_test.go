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
func sendProposalControl(p *fedPeer, kind string, payload any) {
	env := p.envelope(kind, proto.Endpoint{}, payload)
	env.From.Agent = ""
	env.From.Name = "operator"
	p.send(env)
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
		if rec.Code != http.StatusAccepted {
			return false
		}
		return true
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
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	time.Sleep(50 * time.Millisecond)
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
	rec := postMessage(t, f, "link-source-00001", map[string]any{"to": "group:builders@bob", "body": "linked group hello"})
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
	rec = fedHuman(t, f, http.MethodDelete, "/v1/groups/"+source.Name+"/links/"+response.Edges[0].ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.NoError(t, db.UpdateFederationOutbox(sent.EnvelopeID, db.FedOutboxQueued, time.Now().Add(-time.Second), "", 0))
	agentd.FlushFederationOutboxForTest()
	row, err := db.GetFederationOutbox(sent.EnvelopeID)
	require.NoError(t, err)
	require.Equal(t, db.FedOutboxRefused, row.State)
	sendProposalControl(p, proto.KindPeerProposalDecision, decision)
	fedEventually(t, "revocation retained", func() bool {
		l, _ := db.GetFederationGroupLink(response.Edges[0].ID)
		return l != nil && l.LocalState == "revoked"
	})
	rec = postMessage(t, f, "link-source-00001", map[string]any{"to": "group:builders@bob", "body": "must be refused"})
	require.NotEqual(t, http.StatusOK, rec.Code)
}
