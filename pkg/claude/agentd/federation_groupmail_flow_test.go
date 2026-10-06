package agentd_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func fedAckOf(t *testing.T, p *fedPeer, id string) proto.AckPayload {
	t.Helper()
	var out proto.AckPayload
	fedEventually(t, "ack for "+id, func() bool {
		for _, a := range p.envelopes(proto.KindAck) {
			if a.InReplyTo == id {
				require.NoError(t, a.DecodePayload(&out))
				return true
			}
		}
		return false
	})
	return out
}

func fedRemoteBodies(t *testing.T, conv string) []string {
	t.Helper()
	msgs, err := db.ListAgentMessagesForConv(conv, 50)
	require.NoError(t, err)
	var out []string
	for _, m := range msgs {
		out = append(out, m.Body)
	}
	return out
}

// TestFederation_InboundGroupMail: a peer mails an exported group; this
// instance delivers to the group's current members, narrowed by role.
func TestFederation_InboundGroupMail(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const rev = "fedg1-rev-bbbb-cccc-000000000001"
	const dev = "fedg1-dev-bbbb-cccc-000000000002"
	const out = "fedg1-out-bbbb-cccc-000000000003"
	f.HaveGroup("builders")
	f.HaveGroup("private")
	f.HaveConvWithTitle(rev, "rev-agent")
	f.HaveConvWithTitle(dev, "dev-agent")
	f.HaveConvWithTitle(out, "out-agent")
	f.HaveMemberWithRole("builders", rev, "Reviewer")
	f.HaveMemberWithRole("builders", dev, "dev")
	f.HaveMember("private", out)
	f.HaveGroup("quiet")
	f.HaveMemberWithRole("quiet", out, "lead")

	rec := fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "builders", "peer": "bob", "caps": []string{"roster", "mail"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = fedGrantCaps(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"group": "quiet", "peer": "bob", "caps": []string{"mail"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	all := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: "builders", Subject: "standup", Body: "status please"})
	p.send(all)
	require.Equal(t, proto.AckPayload{Status: proto.AckAccepted, Delivered: 2}, fedAckOf(t, p, all.ID))
	for _, c := range []string{rev, dev} {
		bodies := fedRemoteBodies(t, c)
		require.Len(t, bodies, 1)
		require.Contains(t, bodies[0], "bob-agent@bob")
		require.Contains(t, bodies[0], "to group builders")
		require.True(t, strings.HasSuffix(bodies[0], "status please"))
	}

	// A resend is acked but delivers nothing new.
	p.send(all)
	fedEventually(t, "second ack", func() bool {
		n := 0
		for _, a := range p.envelopes(proto.KindAck) {
			if a.InReplyTo == all.ID {
				n++
			}
		}
		return n == 2
	})
	require.Len(t, fedRemoteBodies(t, rev), 1)

	// --role narrows on the receiving side, case-insensitively.
	reviewers := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: "builders", Role: "reviewer", Body: "please review #42"})
	p.send(reviewers)
	require.Equal(t, 1, fedAckOf(t, p, reviewers.ID).Delivered)
	require.Len(t, fedRemoteBodies(t, rev), 2)
	require.Len(t, fedRemoteBodies(t, dev), 1)

	// An unexported group is refused like a missing one.
	for _, g := range []string{"private", "nonexistent"} {
		e := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: g, Body: "hello?"})
		p.send(e)
		ack := fedAckOf(t, p, e.ID)
		require.Equal(t, proto.AckRefused, ack.Status)
		require.Equal(t, "not_exported", ack.Code)
	}
	require.Empty(t, fedRemoteBodies(t, out))

	// A role nobody holds is refused, not silently accepted.
	none := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: "builders", Role: "reviwer", Body: "typo"})
	p.send(none)
	require.Equal(t, "no_recipients", fedAckOf(t, p, none.ID).Code)

	// Without the roster, role filters are refused (they would reveal
	// roles) and acks carry no counts.
	probe := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: "quiet", Role: "lead", Body: "who leads?"})
	p.send(probe)
	ack := fedAckOf(t, p, probe.ID)
	require.Equal(t, proto.AckRefused, ack.Status)
	require.Equal(t, "not_exported", ack.Code)
	plain := p.envelope(proto.KindGroupMail, proto.Endpoint{}, proto.GroupMailPayload{Group: "quiet", Body: "hello quiet"})
	p.send(plain)
	require.Equal(t, proto.AckPayload{Status: proto.AckAccepted}, fedAckOf(t, p, plain.ID))
	require.Len(t, fedRemoteBodies(t, out), 1)

	// A member replies; the reply goes back to the sender as mail.
	in, err := db.FederationInboundByEnvelope(p.id.ID(), reviewers.ID)
	require.NoError(t, err)
	rep := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, fmt.Sprintf("/v1/messages/%d/reply", in.MessageID), map[string]any{"body": "on it"}), rev))
	require.Equal(t, http.StatusOK, rep.Code, rep.Body.String())
	fedEventually(t, "reply at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, m := range p.envelopes(proto.KindMail) {
			var mp proto.MailPayload
			_ = m.DecodePayload(&mp)
			if m.InReplyTo == reviewers.ID && mp.Body == "on it" {
				return true
			}
		}
		return false
	})
}

// TestFederation_OutboundGroupMail: an agent mails a remote group it imports.
func TestFederation_OutboundGroupMail(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fedg2-alice-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveConvWithTitle(alice, "alice-agent")
	f.HaveMember("team", alice)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{
		{Name: "builders", Caps: []string{proto.CapRoster, proto.CapMail}, Members: []proto.CatalogMember{{Agent: "agt_bobremote0000000000000000", Name: "bob-agent"}}},
		{Name: "lurkers", Caps: []string{proto.CapRoster}},
		{Name: "quiet", Caps: []string{proto.CapMail}},
	}}))
	fedEventually(t, "remote catalog visible", func() bool {
		for _, r := range fedStatus(t, f).Remote {
			if r.Label == "bob" && len(r.Groups) == 3 {
				return true
			}
		}
		return false
	})
	send := func(to string, extra map[string]any) (int, map[string]any) {
		body := map[string]any{"to": to, "body": "release at 5", "subject": "heads up"}
		for k, v := range extra {
			body[k] = v
		}
		rec := postMessage(t, f, alice, body)
		var out map[string]any
		testharness.DecodeJSON(t, rec, &out)
		return rec.Code, out
	}

	code, out := send("group:builders@bob", nil)
	require.Equal(t, http.StatusForbidden, code, out)
	require.Equal(t, "permission", out["code"])

	code, out = send("group:builders@bob", nil)
	require.Equal(t, http.StatusForbidden, code, out)
	require.Contains(t, fmt.Sprint(out), agentd.PermMessageDirect)

	// The operator needs only the import.
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/send", map[string]any{"to": "group:builders@bob", "body": "operator says hi"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var opResp struct {
		EnvelopeID string `json:"envelope_id"`
	}
	testharness.DecodeJSON(t, rec, &opResp)
	fedEventually(t, "operator group mail at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, m := range p.envelopes(proto.KindGroupMail) {
			if m.ID == opResp.EnvelopeID {
				return m.From.Agent == "" && m.From.Name == "human operator"
			}
		}
		return false
	})

	g := postPermissionScope(t, f, "grant", map[string]any{"target": alice, "slug": agentd.PermMessageDirect, "scope": map[string]any{"peer": []string{p.id.ID() + "/builders"}}})
	require.Equal(t, http.StatusOK, g.Code, g.Body)

	// A group without mail is not addressable; cc is refused.
	code, out = send("group:lurkers@bob", nil)
	require.Equal(t, http.StatusNotFound, code, out)
	code, _ = send("group:builders@bob", map[string]any{"cc": []string{"someone"}})
	require.Equal(t, http.StatusBadRequest, code)
	// --role needs the roster.
	code, out = send("group:quiet@bob", map[string]any{"role": "lead"})
	require.Equal(t, http.StatusBadRequest, code, out)

	code, out = send("group:builders@bob", map[string]any{"role": "reviewer"})
	require.Equal(t, http.StatusOK, code, out)
	require.Equal(t, "group:builders@bob", out["to"])
	require.Equal(t, "builders", out["via_group"])
	envID := out["envelope_id"].(string)
	var got *proto.Envelope
	fedEventually(t, "group mail at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, m := range p.envelopes(proto.KindGroupMail) {
			if m.ID == envID {
				got = m
				return true
			}
		}
		return false
	})
	var gp proto.GroupMailPayload
	require.NoError(t, got.DecodePayload(&gp))
	require.Equal(t, proto.GroupMailPayload{Group: "builders", Role: "reviewer", Subject: "heads up", Body: "release at 5"}, gp)

	// A remote member's reply to the group mail is accepted.
	aliceAgent, err := db.AgentIDForConv(alice)
	require.NoError(t, err)
	ack := p.envelope(proto.KindAck, proto.Endpoint{}, proto.AckPayload{Status: proto.AckAccepted, Delivered: 3})
	ack.InReplyTo = envID
	p.send(ack)
	fedEventually(t, "outbox notes delivery", func() bool {
		row, _ := db.GetFederationOutbox(envID)
		return row != nil && row.State == db.FedOutboxAccepted && row.LastError == "delivered to 3 members"
	})
	reply := p.envelope(proto.KindMail, proto.Endpoint{Agent: aliceAgent}, proto.MailPayload{Body: "ack from builders"})
	reply.InReplyTo = envID
	p.send(reply)
	require.Equal(t, proto.AckAccepted, fedAckOf(t, p, reply.ID).Status)
}

// TestFederation_RemoteCC: remote member@peer entries in --cc each get their
// own sealed copy; they are authorized before anything is written.
func TestFederation_RemoteCC(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fedc1-alice-bbbb-cccc-000000000001"
	const carol = "fedc1-carol-bbbb-cccc-000000000002"
	f.HaveGroup("team")
	f.HaveConvWithTitle(alice, "alice-agent")
	f.HaveConvWithTitle(carol, "carol-agent")
	f.HaveMember("team", alice)
	f.HaveMember("team", carol)
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapMail},
		Members: []proto.CatalogMember{
			{Agent: "agt_bobremote0000000000000000", Name: "bob-agent"},
			{Agent: "agt_danremote0000000000000000", Name: "dan-agent"},
		},
	}}}))
	fedEventually(t, "remote catalog visible", func() bool {
		for _, r := range fedStatus(t, f).Remote {
			if r.Label == "bob" && len(r.Groups) == 1 {
				return true
			}
		}
		return false
	})
	send := func(body map[string]any) (int, map[string]any) {
		body["body"] = "ship it"
		rec := postMessage(t, f, alice, body)
		var out map[string]any
		testharness.DecodeJSON(t, rec, &out)
		return rec.Code, out
	}

	// Without federation.message the whole send is refused: carol gets
	// nothing either.
	code, out := send(map[string]any{"to": "carol-agent", "cc": []string{"bob-agent@bob"}})
	require.Equal(t, http.StatusForbidden, code, out)
	require.Empty(t, fedRemoteBodies(t, carol))
	// Several uncovered recipients are refused outright: a one-shot
	// approval would only ever show the human one of them.
	code, out = send(map[string]any{"to": "carol-agent", "cc": []string{"bob-agent@bob", "dan-agent@bob"}})
	require.Equal(t, http.StatusForbidden, code, out)
	require.Contains(t, out["error"], "dan-agent@bob")
	require.Empty(t, fedRemoteBodies(t, carol))

	g := postPermissionScope(t, f, "grant", map[string]any{"target": alice, "slug": agentd.PermMessageDirect, "scope": map[string]any{"peer": []string{p.id.ID() + "/builders"}}})
	require.Equal(t, http.StatusOK, g.Code, g.Body)

	code, out = send(map[string]any{"to": "carol-agent", "cc": []string{"bob-agent@bob"}})
	require.Equal(t, http.StatusOK, code, out)
	rcpts := out["recipients"].([]any)
	require.Len(t, rcpts, 2)
	require.Equal(t, "carol-agent", rcpts[0].(map[string]any)["title"])
	remote := rcpts[1].(map[string]any)
	require.Equal(t, "bob-agent@bob", remote["title"])
	require.Equal(t, true, remote["queued"])
	envID := remote["envelope_id"].(string)
	require.Len(t, fedRemoteBodies(t, carol), 1)
	fedEventually(t, "cc copy at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, m := range p.envelopes(proto.KindMail) {
			if m.ID == envID && m.To.Agent == "agt_bobremote0000000000000000" {
				return true
			}
		}
		return false
	})

	// What a remote copy would refuse is checked before the local send.
	code, out = send(map[string]any{"to": "carol-agent", "cc": []string{"bob-agent@bob"}, "subject": strings.Repeat("s", 600)})
	require.Equal(t, http.StatusBadRequest, code, out)
	require.Len(t, fedRemoteBodies(t, carol), 1)

	// The primary is not sent twice when it is also cc'd.
	code, out = send(map[string]any{"to": "bob-agent@bob", "cc": []string{"bob-agent@bob"}})
	require.Equal(t, http.StatusOK, code, out)
	require.Nil(t, out["cc"])

	// A remote primary may cc other remote members, not local agents.
	code, out = send(map[string]any{"to": "bob-agent@bob", "cc": []string{"dan-agent@bob"}})
	require.Equal(t, http.StatusOK, code, out)
	cc := out["cc"].([]any)
	require.Len(t, cc, 1)
	require.Equal(t, "dan-agent@bob", cc[0].(map[string]any)["to"])
	code, out = send(map[string]any{"to": "bob-agent@bob", "cc": []string{"carol-agent"}})
	require.Equal(t, http.StatusBadRequest, code, out)
}
