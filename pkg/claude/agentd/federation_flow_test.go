package agentd_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/client"
	"github.com/tofutools/tclaude/pkg/federation/hub"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// fedPeer is a scripted remote instance speaking the real wire protocol.
type fedPeer struct {
	t   *testing.T
	id  *proto.Identity
	cl  *client.Client
	mu  sync.Mutex
	got []*proto.Envelope
	// agentdPub is the local daemon's key as the hub directory reports it.
	agentdID string
}

func (p *fedPeer) envelopes(kind string) []*proto.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*proto.Envelope
	for _, e := range p.got {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func (p *fedPeer) send(env *proto.Envelope) {
	p.t.Helper()
	s, err := proto.Seal(p.id, env)
	require.NoError(p.t, err)
	res, err := p.cl.Send(context.Background(), p.agentdID, s)
	require.NoError(p.t, err)
	require.Equal(p.t, proto.SendDelivered, res.Status, "peer send: %+v", res)
}

func (p *fedPeer) envelope(kind string, to proto.Endpoint, payload any) *proto.Envelope {
	p.t.Helper()
	to.Instance = p.agentdID
	env, err := proto.NewEnvelope(p.id, kind, proto.Endpoint{Agent: "agt_bobremote0000000000000000", Name: "bob-agent"}, to, time.Hour, payload)
	require.NoError(p.t, err)
	return env
}

type fedHarness struct {
	f     *testharness.Flow
	hub   *hub.Hub
	store *hub.Store
	url   string
	peer  *fedPeer
}

func fedEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fedHuman(t *testing.T, f *testharness.Flow, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return testharness.Serve(f.Mux, agentd.AsHumanPeer(testharness.JSONRequest(t, method, path, body)))
}

type fedStatusView struct {
	InstanceID string `json:"instance_id"`
	Hub        *struct {
		State string `json:"state"`
	} `json:"hub"`
	Peers []struct {
		InstanceID string `json:"instance_id"`
		Trusted    bool   `json:"trusted"`
		Online     bool   `json:"online"`
	} `json:"peers"`
	Remote []struct {
		Label  string               `json:"label"`
		Groups []proto.CatalogGroup `json:"groups"`
	} `json:"remote"`
}

func fedStatus(t *testing.T, f *testharness.Flow) fedStatusView {
	t.Helper()
	rec := fedHuman(t, f, http.MethodGet, "/v1/federation/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var v fedStatusView
	testharness.DecodeJSON(t, rec, &v)
	return v
}

// newFedHarness connects the flow's daemon and one scripted peer to an
// in-process hub, then has the operator trust the peer as "bob".
func newFedHarness(t *testing.T) *fedHarness {
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)

	st, err := hub.OpenStore(filepath.Join(t.TempDir(), "hub.sqlite"))
	require.NoError(t, err)
	h, err := hub.New(st, hub.Config{})
	require.NoError(t, err)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(func() { h.Close(); srv.Close(); _ = st.Close() })
	url := "ws" + strings.TrimPrefix(srv.URL, "http")

	localID := agentd.FederationInstanceIDForTest()
	require.NoError(t, st.Admit(localID))
	peerID, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, st.Admit(peerID.ID()))

	p := &fedPeer{t: t, id: peerID, agentdID: localID}
	cl, err := client.New(client.Options{
		URL: url, Identity: peerID, Name: "bob-laptop", MaxBackoff: 200 * time.Millisecond,
		OnDeliver: func(from string, s *proto.Sealed) {
			key, ok := p.cl.LookupKey(from)
			if !ok {
				return
			}
			env, err := proto.Open(s, ed25519.PublicKey(key), peerID.ID(), time.Now())
			if err != nil {
				return
			}
			p.mu.Lock()
			p.got = append(p.got, env)
			p.mu.Unlock()
		},
	})
	require.NoError(t, err)
	p.cl = cl
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { cl.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": url, "name": "alice-box"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	fedEventually(t, "daemon sees peer online", func() bool {
		for _, pe := range fedStatus(t, f).Peers {
			if pe.InstanceID == peerID.ID() && pe.Online {
				return true
			}
		}
		return false
	})
	fedEventually(t, "peer sees daemon", func() bool { _, ok := cl.LookupKey(localID); return ok })

	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": peerID.ID(), "label": "bob"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return &fedHarness{f: f, hub: h, store: st, url: url, peer: p}
}

func TestFederation_ExportCatalogAndInboundMail(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fed1-alice-bbbb-cccc-000000000001"
	const hidden = "fed1-hide-bbbb-cccc-000000000002"
	f.HaveGroup("team")
	f.HaveGroup("private")
	f.HaveConvWithTitle(alice, "alice-agent")
	f.HaveMember("team", alice)
	f.HaveConvWithTitle(hidden, "hidden-agent")
	f.HaveMember("private", hidden)
	f.HaveAliveSession(alice, "spwn-fed1-a", "tclaude-spwn-fed1-a", f.TestCwd("work"))

	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/exports", map[string]any{"group": "team", "peer": "bob", "caps": []string{"roster", "mail"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The peer receives a signed catalog listing only the exported group.
	aliceAgent, err := db.AgentIDForConv(alice)
	require.NoError(t, err)
	var cat proto.CatalogPayload
	fedEventually(t, "catalog with alice", func() bool {
		cats := p.envelopes(proto.KindCatalog)
		if len(cats) == 0 {
			return false
		}
		require.NoError(t, cats[len(cats)-1].DecodePayload(&cat))
		return len(cat.Groups) == 1 && len(cat.Groups[0].Members) == 1
	})
	require.Equal(t, "team", cat.Groups[0].Name)
	require.Equal(t, aliceAgent, cat.Groups[0].Members[0].Agent)
	require.Equal(t, "alice-agent", cat.Groups[0].Members[0].Name)

	// Inbound mail to an exported member lands in its inbox and is acked.
	mail := p.envelope(proto.KindMail, proto.Endpoint{Agent: aliceAgent}, proto.MailPayload{Subject: "hello", Body: "ping from bob"})
	p.send(mail)
	fedEventually(t, "accepted ack", func() bool {
		for _, a := range p.envelopes(proto.KindAck) {
			var ack proto.AckPayload
			_ = a.DecodePayload(&ack)
			if a.InReplyTo == mail.ID && ack.Status == proto.AckAccepted {
				return true
			}
		}
		return false
	})
	in, err := db.FederationInboundByEnvelope(mail.ID)
	require.NoError(t, err)
	require.NotNil(t, in)
	read := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, fmt.Sprintf("/v1/messages/%d", in.MessageID), nil), alice))
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	var msg map[string]any
	testharness.DecodeJSON(t, read, &msg)
	require.Equal(t, "bob-agent@bob (remote)", msg["from_title"])
	require.Equal(t, true, msg["replyable"])
	require.Contains(t, msg["body"], "remote message from bob-agent@bob")
	require.Contains(t, msg["body"], "ping from bob")
	f.AssertSentContains("tclaude-spwn-fed1-a:0.0", "bob-agent@bob (remote)", 3*time.Second)

	// A resend of the same envelope is acked again but not duplicated.
	p.send(mail)
	fedEventually(t, "second ack", func() bool {
		n := 0
		for _, a := range p.envelopes(proto.KindAck) {
			if a.InReplyTo == mail.ID {
				n++
			}
		}
		return n >= 2
	})
	again, err := db.FederationInboundByEnvelope(mail.ID)
	require.NoError(t, err)
	require.Equal(t, in.MessageID, again.MessageID)

	// Mail to a member of a non-exported group is refused.
	hiddenAgent, err := db.AgentIDForConv(hidden)
	require.NoError(t, err)
	bad := p.envelope(proto.KindMail, proto.Endpoint{Agent: hiddenAgent}, proto.MailPayload{Body: "sneaky"})
	p.send(bad)
	fedEventually(t, "refusal", func() bool {
		for _, a := range p.envelopes(proto.KindAck) {
			var ack proto.AckPayload
			_ = a.DecodePayload(&ack)
			if a.InReplyTo == bad.ID && ack.Status == proto.AckRefused && ack.Code == "not_exported" {
				return true
			}
		}
		return false
	})
	gone, err := db.FederationInboundByEnvelope(bad.ID)
	require.NoError(t, err)
	require.Nil(t, gone)

	// Once untrusted, the peer's envelopes are dropped unanswered.
	defer func() {
		rec := fedHuman(t, f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		late := p.envelope(proto.KindMail, proto.Endpoint{Agent: aliceAgent}, proto.MailPayload{Body: "after untrust"})
		p.send(late)
		time.Sleep(300 * time.Millisecond)
		for _, a := range p.envelopes(proto.KindAck) {
			require.NotEqual(t, late.ID, a.InReplyTo, "untrusted peer got an answer")
		}
		dropped, err := db.FederationInboundByEnvelope(late.ID)
		require.NoError(t, err)
		require.Nil(t, dropped)
	}()

	// The agent replies through the normal reply verb; it goes back over
	// federation with in_reply_to set.
	rep := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, fmt.Sprintf("/v1/messages/%d/reply", in.MessageID), map[string]any{"body": "pong"}), alice))
	require.Equal(t, http.StatusOK, rep.Code, rep.Body.String())
	fedEventually(t, "reply at peer", func() bool {
		for _, m := range p.envelopes(proto.KindMail) {
			var mp proto.MailPayload
			_ = m.DecodePayload(&mp)
			if m.InReplyTo == mail.ID && mp.Body == "pong" && m.From.Agent == aliceAgent {
				return true
			}
		}
		return false
	})
}

func TestFederation_OutboundMailRequiresImportAndSlug(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer

	const alice = "fed2-alice-bbbb-cccc-000000000001"
	f.HaveGroup("team")
	f.HaveConvWithTitle(alice, "alice-agent")
	f.HaveMember("team", alice)

	// Bob's instance exports its group "builders" with bob-agent to us.
	p.send(p.envelope(proto.KindCatalog, proto.Endpoint{}, proto.CatalogPayload{Groups: []proto.CatalogGroup{{
		Name: "builders", Caps: []string{proto.CapRoster, proto.CapMail},
		Members: []proto.CatalogMember{{Agent: "agt_bobremote0000000000000000", Name: "bob-agent"}},
	}}}))
	fedEventually(t, "remote catalog visible", func() bool {
		for _, r := range fedStatus(t, f).Remote {
			if r.Label == "bob" && len(r.Groups) == 1 {
				return true
			}
		}
		return false
	})

	send := func() *httptest.ResponseRecorder {
		return postMessage(t, f, alice, map[string]any{"to": "bob-agent@bob", "body": "need a review"})
	}

	// Not imported yet.
	rec := send()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "not imported")

	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/imports", map[string]any{"local_group": "team", "peer": "bob", "remote_group": "builders"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Imported, but alice lacks federation.message.
	rec = send()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), agentd.PermFederationMessage)

	require.NoError(t, db.GrantAgentPermission(alice, agentd.PermFederationMessage, "test"))
	rec = send()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		EnvelopeID string `json:"envelope_id"`
		ViaGroup   string `json:"via_group"`
	}
	testharness.DecodeJSON(t, rec, &resp)
	require.Equal(t, "team", resp.ViaGroup)

	var got *proto.Envelope
	fedEventually(t, "mail at peer", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, m := range p.envelopes(proto.KindMail) {
			if m.ID == resp.EnvelopeID {
				got = m
				return true
			}
		}
		return false
	})
	var mp proto.MailPayload
	require.NoError(t, got.DecodePayload(&mp))
	require.Equal(t, "need a review", mp.Body)
	require.Equal(t, "alice-agent", got.From.Name)

	// The peer acks; the outbox row settles as accepted.
	ack := p.envelope(proto.KindAck, proto.Endpoint{}, proto.AckPayload{Status: proto.AckAccepted})
	ack.InReplyTo = resp.EnvelopeID
	p.send(ack)
	fedEventually(t, "outbox accepted", func() bool {
		row, _ := db.GetFederationOutbox(resp.EnvelopeID)
		return row != nil && row.State == db.FedOutboxAccepted
	})

	// Untrusting the peer drops its imports: sending is refused again.
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = send()
	require.NotEqual(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestFederation_ConfigGuards(t *testing.T) {
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)

	// Configuration is human-only.
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/config",
		map[string]any{"enabled": true, "hub_url": "ws://127.0.0.1:1"}), "fed3-agent-bbbb-cccc-000000000001"))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// Plain ws:// to a non-loopback hub is refused.
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": "ws://hub.example.com"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
