package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationMailAndAway(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) []byte {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		return rec.Body.Bytes()
	}
	incoming := fh.peer.envelope(proto.KindOperatorMail, proto.Endpoint{}, proto.MailPayload{Subject: "hello", Body: "how is the work?"})
	fh.peer.send(incoming)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, incoming.ID).Status)
	var inbox []struct {
		Instance string `json:"instance"`
		Subject  string `json:"subject"`
	}
	require.NoError(t, json.Unmarshal(call("GET", "inbox", nil, 200), &inbox))
	require.Len(t, inbox, 1)
	require.Equal(t, fh.peer.id.ID(), inbox[0].Instance)
	var sent struct {
		ID string `json:"envelope_id"`
	}
	require.NoError(t, json.Unmarshal(call("POST", "notify", map[string]any{"peer": inbox[0].Instance, "subject": "Re: hello", "body": "ready"}, 200), &sent))
	fedEventually(t, "dashboard operator reply", func() bool {
		agentd.FlushFederationOutboxForTest()
		for _, env := range fh.peer.envelopes(proto.KindOperatorMail) {
			if env.ID == sent.ID {
				var mail proto.MailPayload
				require.NoError(t, env.DecodePayload(&mail))
				require.Equal(t, "ready", mail.Body)
				require.Equal(t, "Re: hello", mail.Subject)
				require.Empty(t, env.From.Agent)
				return true
			}
		}
		return false
	})
	// The sender resolves targets through the shared CLI path, never fabricating authority.
	call("POST", "send", map[string]string{"to": "nobody@unknown-peer", "body": "hello"}, 404)
	call("POST", "away", map[string]string{"cover": "bob"}, 200)
	// A HEAD request with a different cover cannot replace the current selection.
	for _, prefix := range []string{"/api/federation/", "/v1/federation/"} {
		req := testharness.JSONRequest(t, "HEAD", prefix+"away", map[string]string{"cover": "unknown-peer"})
		handler := h
		if prefix == "/v1/federation/" {
			handler = fh.f.Mux
			req = agentd.AsHumanPeer(req)
		}
		rec := testharness.Serve(handler, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), fh.peer.id.ID())
	}
	ticket := strings.Repeat("c", 32) + "." + strings.Repeat("d", 32) + "@bob"
	call("POST", "answer", map[string]string{"ticket": ticket, "decision": "always"}, 400)
	call("POST", "answer", map[string]string{"ticket": ticket, "decision": "deny"}, 200)
	require.JSONEq(t, `{"away":null}`, string(call("POST", "return", nil, 200)))
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"POST", "send"}, {"POST", "notify"}, {"GET", "inbox"}, {"GET", "away"}, {"POST", "away"}, {"POST", "return"}, {"POST", "answer"}} {
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	raw := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(raw)
	rec := testharness.Serve(raw, testharness.JSONRequest(t, "POST", "/api/federation/notify", map[string]string{"peer": "bob", "body": "hello"}))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
