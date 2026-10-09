package agentd_test

import (
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

func setFedAway(t *testing.T, fh *fedHarness, until time.Time) {
	t.Helper()
	r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/away", map[string]any{"cover": "bob", "until": until})
	require.Equal(t, 200, r.Code, r.Body.String())
}
func grantFedAnswers(t *testing.T, fh *fedHarness) {
	t.Helper()
	r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermApprovalsAnswer})
	require.Equal(t, 200, r.Code, r.Body.String())
}
func fedAwayTicket(t *testing.T, p *fedPeer, request string) (string, string) {
	t.Helper()
	var epoch string
	fedEventually(t, "approval notice", func() bool {
		for _, e := range p.envelopes(proto.KindAwayNotice) {
			var m proto.MailPayload
			require.NoError(t, e.DecodePayload(&m))
			if strings.Contains(m.Body, "Request: "+request+".") {
				epoch = e.InReplyTo
				return true
			}
		}
		return false
	})
	return request, epoch
}
func sendFedAwayDecision(t *testing.T, p *fedPeer, request, epoch, decision string) *proto.Envelope {
	t.Helper()
	e := p.envelope(proto.KindAwayAnswer, proto.Endpoint{}, map[string]any{"request": request, "epoch": epoch, "decision": decision})
	e.From.Agent = ""
	p.send(e)
	return e
}
func awaitFedAwayAck(t *testing.T, p *fedPeer, id, want string) {
	t.Helper()
	fedEventually(t, "answer receipt "+want, func() bool {
		for _, e := range p.envelopes(proto.KindAck) {
			if e.InReplyTo == id {
				var a proto.AckPayload
				require.NoError(t, e.DecodePayload(&a))
				if a.Status == want {
					return true
				}
			}
		}
		return false
	})
}
func TestFederation_AwayApprovalAuthority(t *testing.T) {
	for _, decision := range []string{"approve", "deny"} {
		t.Run(decision, func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveConvWithTitle("away-requester", "away requester")
			grantFedAnswers(t, fh)
			setFedAway(t, fh, time.Time{})
			id := strings.Repeat("a", 32)
			done, cleanup := agentd.StartFederationAwayApprovalForTest(id, "away-requester", 8*time.Second)
			t.Cleanup(cleanup)
			_, epoch := fedAwayTicket(t, fh.peer, id)
			// The decision is instance-only and one-shot; never an agent envelope or always grant.
			invalid := sendFedAwayDecision(t, fh.peer, id, epoch, "always")
			awaitFedAwayAck(t, fh.peer, invalid.ID, proto.AckRefused)
			agent := fh.peer.envelope(proto.KindAwayAnswer, proto.Endpoint{}, map[string]any{"request": id, "epoch": epoch, "decision": "approve"})
			fh.peer.send(agent)
			awaitFedAwayAck(t, fh.peer, agent.ID, proto.AckRefused)
			valid := sendFedAwayDecision(t, fh.peer, id, epoch, decision)
			select {
			case approved := <-done:
				require.Equal(t, decision == "approve", approved)
			case <-time.After(5 * time.Second):
				t.Fatal("waiter did not resolve")
			}
			awaitFedAwayAck(t, fh.peer, valid.ID, proto.AckAccepted)
			fh.peer.send(valid)
			awaitFedAwayAck(t, fh.peer, valid.ID, proto.AckAccepted)
			// Replay cannot produce a second decision.
			replay := sendFedAwayDecision(t, fh.peer, id, epoch, decision)
			awaitFedAwayAck(t, fh.peer, replay.ID, proto.AckRefused)
			logs, err := db.ListAuditLog(db.AuditLogFilter{Limit: 200})
			require.NoError(t, err)
			found := false
			for _, l := range logs {
				if l.Verb == "approval."+decision && l.ActorLabel == "operator@"+fh.peer.id.ID() {
					found = true
				}
			}
			require.True(t, found, "decision must name covering operator's instance")

		})
	}
}

func TestFederation_AwayInvalidation(t *testing.T) {
	for _, boundary := range []string{"missing-grant", "return", "revoke-regrant", "change-cover", "expiry", "original-deadline", "restart", "untrust-retrust", "trust-downgrade"} {
		t.Run(boundary, func(t *testing.T) {
			fh := newFedHarness(t)
			fh.f.HaveConvWithTitle("away-requester", "away requester")
			grantFedAnswers(t, fh)
			until := time.Time{}
			if boundary == "expiry" {
				until = time.Now().Add(1500 * time.Millisecond)
			}
			setFedAway(t, fh, until)
			timeout := 8 * time.Second
			if boundary == "original-deadline" {
				timeout = 1500 * time.Millisecond
			}
			id := strings.Repeat("b", 32)
			done, cleanup := agentd.StartFederationAwayApprovalForTest(id, "away-requester", timeout)
			t.Cleanup(cleanup)
			_, epoch := fedAwayTicket(t, fh.peer, id)
			switch boundary {
			case "missing-grant", "revoke-regrant":
				r := fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermApprovalsAnswer})
				require.Equal(t, 200, r.Code, r.Body.String())
				if boundary == "revoke-regrant" {
					grantFedAnswers(t, fh)
				}
			case "return":
				r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/return", nil)
				require.Equal(t, 200, r.Code)
			case "change-cover":
				setFedAway(t, fh, time.Time{}) // even reselecting same peer starts a new epoch.
				fedEventually(t, "pending request reforwarded", func() bool {
					for _, notice := range fh.peer.envelopes(proto.KindAwayNotice) {
						if notice.InReplyTo != epoch {
							var m proto.MailPayload
							require.NoError(t, notice.DecodePayload(&m))
							if strings.Contains(m.Body, "Request: "+id+".") {
								return true
							}
						}
					}
					return false
				})
			case "original-deadline":
				t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
				h := agentd.BuildDashboardHandlerForTest()
				r := testharness.Serve(h, testharness.JSONRequest(t, http.MethodPost, "/api/access-requests/"+id+"/decision", map[string]any{"decision": "extend", "secs": 10}))
				require.Equal(t, 200, r.Code, r.Body.String())
				time.Sleep(1600 * time.Millisecond)
				select {
				case <-done:
					t.Fatal("local deadline should still be extended")
				default:
				}
			case "expiry":
				time.Sleep(1600 * time.Millisecond)
			case "restart":
				r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": false})
				require.Equal(t, 200, r.Code, r.Body.String())
				r = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true})
				require.Equal(t, 200, r.Code, r.Body.String())
				fedEventually(t, "reconnected after restart", func() bool { v := fedStatus(t, fh.f); return v.Hub != nil && v.Hub.State == "connected" })
			case "untrust-retrust":
				r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/untrust", map[string]any{"instance": "bob"})
				require.Equal(t, 200, r.Code, r.Body.String())
				r = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/peers/trust", map[string]any{"instance": fh.peer.id.ID(), "label": "bob"})
				require.Equal(t, 200, r.Code, r.Body.String())
				grantFedAnswers(t, fh)
			case "trust-downgrade":
				setFedTrustLevel(t, fh, "unrestricted")
				setFedTrustLevel(t, fh, "restricted")
			}
			e := sendFedAwayDecision(t, fh.peer, id, epoch, "approve")
			awaitFedAwayAck(t, fh.peer, e.ID, proto.AckRefused)
			select {
			case approved := <-done:
				require.False(t, approved)
			default:
			}
		})
	}
}

func TestFederation_AwayNoticesAndHumanOnly(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	f.HaveConvWithTitle("away-agent", "away agent")
	f.HaveGroup("team")
	f.HaveMember("team", "away-agent")
	f.HaveAliveSession("away-agent", "away-runtime", "tclaude-away-runtime", f.TestCwd("work"))
	f.SetSessionStatus("away-agent", "working")
	// Instance grant cannot accidentally become a group-scoped permission.
	r := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermApprovalsAnswer, "scope": "group=team"})
	require.Equal(t, 400, r.Code)
	for _, path := range []string{"away", "return", "answer"} {
		req := testharness.JSONRequest(t, http.MethodPost, "/v1/federation/"+path, map[string]any{"cover": "bob"})
		req = agentd.AsAgentPeer(req, "away-agent")
		r = testharness.Serve(f.Mux, req)
		require.Equal(t, 403, r.Code, r.Body.String())
	}
	setFedAway(t, fh, time.Time{})
	require.NoError(t, db.GrantAgentPermission("away-agent", "human.notify", "test"))
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", map[string]any{"subject": "question", "body": "First line\nSecond line"})
	req = agentd.AsAgentPeer(req, "away-agent")
	r = testharness.Serve(f.Mux, req)
	require.Equal(t, 200, r.Code, r.Body.String())
	fedEventually(t, "forwarded notify human", func() bool {
		for _, e := range p.envelopes(proto.KindAwayNotice) {
			var m proto.MailPayload
			require.NoError(t, e.DecodePayload(&m))
			if strings.Contains(m.Body, "First line\nSecond line") {
				return true
			}
		}
		return false
	})
	f.SetSessionStatus("away-agent", "awaiting_input")
	fedEventually(t, "question wait notice", func() bool {
		for _, e := range p.envelopes(proto.KindAwayNotice) {
			var m proto.MailPayload
			require.NoError(t, e.DecodePayload(&m))
			if strings.Contains(m.Subject, "waiting for question") {
				require.Contains(t, m.Body, "tclaude federation attach agt_")
				return true
			}
		}
		return false
	})
	// Received notices land locally once and never loop back as new cover mail.
	incoming := p.envelope(proto.KindAwayNotice, proto.Endpoint{}, proto.MailPayload{Subject: "remote needs help", Body: "attach agt_remote@instance"})
	incoming.From.Agent = ""
	p.send(incoming)
	p.send(incoming)
	fedEventually(t, "remote operator inbox", func() bool {
		messages, err := db.ListHumanMessages()
		require.NoError(t, err)
		n := 0
		for _, m := range messages {
			if m.Subject == "remote needs help" {
				n++
			}
		}
		return n == 1
	})
	r = fedHuman(t, f, http.MethodPost, "/v1/federation/return", nil)
	require.Equal(t, 200, r.Code)
	before := len(p.envelopes(proto.KindAwayNotice))
	req = testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", map[string]any{"subject": "returned", "body": "local only"})
	req = agentd.AsAgentPeer(req, "away-agent")
	r = testharness.Serve(f.Mux, req)
	require.Equal(t, 200, r.Code)
	agentd.FlushFederationOutboxForTest()
	require.Len(t, p.envelopes(proto.KindAwayNotice), before)
}

func TestFederation_AwayAnswerSubmissionAndReceipt(t *testing.T) {
	fh := newFedHarness(t)
	ticket := strings.Repeat("c", 32) + "." + strings.Repeat("d", 32) + "@bob"
	for _, decision := range []string{"always", "always_scoped", "extend", ""} {
		r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/answer", map[string]any{"ticket": ticket, "decision": decision})
		require.Equal(t, 400, r.Code, r.Body.String())
	}
	r := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/answer", map[string]any{"ticket": ticket, "decision": "deny"})
	require.Equal(t, 200, r.Code, r.Body.String())
	var submitted *proto.Envelope
	fedEventually(t, "submitted answer", func() bool {
		rows := fh.peer.envelopes(proto.KindAwayAnswer)
		if len(rows) == 0 {
			return false
		}
		submitted = rows[0]
		return true
	})
	require.Empty(t, submitted.From.Agent)
	var answer struct{ Request, Epoch, Decision string }
	require.NoError(t, submitted.DecodePayload(&answer))
	require.Equal(t, "deny", answer.Decision)
	require.Equal(t, strings.Repeat("c", 32), answer.Request)
	receipt := fh.peer.envelope(proto.KindAck, proto.Endpoint{}, proto.AckPayload{Status: proto.AckRefused, Code: "denied", Reason: "coverage expired"})
	receipt.From.Agent = ""
	receipt.InReplyTo = submitted.ID
	fh.peer.send(receipt)
	fedEventually(t, "refused sender outbox", func() bool {
		row, err := db.GetFederationOutbox(submitted.ID)
		require.NoError(t, err)
		return row != nil && row.State == db.FedOutboxRefused
	})
	logs, err := db.ListAuditLog(db.AuditLogFilter{Limit: 100})
	require.NoError(t, err)
	origin := agentd.FederationInstanceIDForTest()
	queued, settled := false, false
	for _, l := range logs {
		if l.Verb == "federation.away.answer.out" && strings.Contains(l.Detail, "decider="+origin) {
			queued = true
		}
		if l.Verb == "federation.away.answer.result" && strings.Contains(l.Detail, "decider="+origin) && strings.Contains(l.Detail, "refused") {
			settled = true
		}
	}
	require.True(t, queued)
	require.True(t, settled)
}

func TestFederation_AwayUnrestrictedCover(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveConvWithTitle("away-requester", "away requester")
	setFedTrustLevel(t, fh, "unrestricted")
	setFedAway(t, fh, time.Time{})
	id := strings.Repeat("e", 32)
	done, cleanup := agentd.StartFederationAwayApprovalForTest(id, "away-requester", 8*time.Second)
	t.Cleanup(cleanup)
	_, epoch := fedAwayTicket(t, fh.peer, id)
	e := sendFedAwayDecision(t, fh.peer, id, epoch, "approve")
	select {
	case approved := <-done:
		require.True(t, approved)
	case <-time.After(5 * time.Second):
		t.Fatal("unrestricted selected cover could not answer")
	}
	awaitFedAwayAck(t, fh.peer, e.ID, proto.AckAccepted)
}
