package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func fedEnrollmentToken(t *testing.T, fh *fedHarness) string {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/profiles", map[string]any{"name": "rigs", "definition": map[string]any{"trust_level": "restricted", "peer_grants": []any{map[string]any{"slug": "config.offer"}}}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/enroll-tokens", map[string]any{"profile": "rigs", "uses": 1})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Token
}
func fedEnrollmentMessage(t *testing.T, p *fedPeer, kind, reply string, payload any) *proto.Envelope {
	t.Helper()
	env, e := proto.NewEnvelope(p.id, kind, proto.Endpoint{}, proto.Endpoint{Instance: p.agentdID}, 2*time.Minute, payload)
	require.NoError(t, e)
	env.InReplyTo = reply
	return env
}
func fedEnrollmentResult(t *testing.T, p *fedPeer, id string) proto.EnrollmentResult {
	t.Helper()
	var result proto.EnrollmentResult
	fedEventually(t, "sealed enrollment response", func() bool {
		for _, env := range p.envelopes(proto.KindEnrollResult) {
			if env.InReplyTo == id {
				return env.DecodePayload(&result) == nil
			}
		}
		return false
	})
	return result
}
func TestFederation_EnrollmentUntrustedBootstrapAndRetirement(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	bearer := fedEnrollmentToken(t, fh)
	tok, e := proto.ParseEnrollmentToken(bearer)
	require.NoError(t, e)
	_, e = db.UntrustFederationPeer(p.id.ID())
	require.NoError(t, e)
	req := fedEnrollmentMessage(t, p, proto.KindEnrollRequest, "", proto.EnrollmentRequest{Token: bearer})
	p.send(req)
	result := fedEnrollmentResult(t, p, req.ID)
	require.True(t, result.Accepted)
	peer, e := db.GetFederationPeer(p.id.ID())
	require.NoError(t, e)
	require.NotNil(t, peer)
	grants, e := db.ListFederationPeerGrants(p.id.ID())
	require.NoError(t, e)
	require.Len(t, grants, 1)
	// An authenticated retry recovers the same receipt after token revocation.
	require.NoError(t, db.RevokeFederationEnrollmentToken(tok.Claims.TokenID))
	req = fedEnrollmentMessage(t, p, proto.KindEnrollRequest, "", proto.EnrollmentRequest{Token: bearer})
	p.send(req)
	require.True(t, fedEnrollmentResult(t, p, req.ID).Accepted)
	_, e = db.UntrustFederationPeer(p.id.ID())
	require.NoError(t, e)
	req = fedEnrollmentMessage(t, p, proto.KindEnrollRequest, "", proto.EnrollmentRequest{Token: bearer})
	p.send(req)
	require.False(t, fedEnrollmentResult(t, p, req.ID).Accepted)
	peer, e = db.GetFederationPeer(p.id.ID())
	require.NoError(t, e)
	require.Nil(t, peer)
	rec := fedHuman(t, fh.f, http.MethodGet, "/v1/federation/enroll-tokens", nil)
	require.NotContains(t, rec.Body.String(), strings.Split(bearer, ".")[3])
	require.Contains(t, rec.Body.String(), `"used":1`)
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/enrollments", nil)
	require.Contains(t, rec.Body.String(), `"retired":true`)
	require.Contains(t, rec.Body.String(), p.id.ID())
	d, e := db.Open()
	require.NoError(t, e)
	var n int
	require.NoError(t, d.QueryRow(`SELECT count(*) FROM federation_outbox WHERE kind IN ('enroll_request','enroll_result')`).Scan(&n))
	require.Zero(t, n)
}
func TestFederation_EnrollmentNodeRequiresPinnedPendingResult(t *testing.T) {
	fh := newFedHarness(t)
	p := fh.peer
	_, e := db.UntrustFederationPeer(p.id.ID())
	require.NoError(t, e)
	bearer, tok, e := proto.NewEnrollmentToken(p.id, "profile-id", "rigs", 3, "unrestricted", time.Now().Add(time.Hour))
	require.NoError(t, e)
	in := map[string]any{"master": p.id.ID(), "token": bearer}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/enroll/preview", in)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), proto.Fingerprint(p.id.Pub))
	require.Contains(t, rec.Body.String(), "unrestricted")
	var preview struct {
		Token string `json:"preview_token"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &preview))
	in["preview_token"] = preview.Token
	// A response without a pending request cannot change trust.
	result := proto.EnrollmentResult{Accepted: true, TokenID: tok.Claims.TokenID, Node: p.agentdID, ProfileID: tok.Claims.ProfileID, ProfileRevision: 3, TrustLevel: "unrestricted"}
	p.send(fedEnrollmentMessage(t, p, proto.KindEnrollResult, "unsolicited", result))
	peer, e := db.GetFederationPeer(p.id.ID())
	require.NoError(t, e)
	require.Nil(t, peer)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- fedHuman(t, fh.f, http.MethodPost, "/v1/federation/enroll", in) }()
	var request *proto.Envelope
	fedEventually(t, "memory-only enrollment request", func() bool {
		requests := p.envelopes(proto.KindEnrollRequest)
		if len(requests) == 0 {
			return false
		}
		request = requests[0]
		return true
	})
	var payload proto.EnrollmentRequest
	require.NoError(t, request.DecodePayload(&payload))
	require.Equal(t, bearer, payload.Token)
	// The request ID alone is insufficient: signed consent terms must also match.
	wrong := result
	wrong.ProfileRevision++
	p.send(fedEnrollmentMessage(t, p, proto.KindEnrollResult, request.ID, wrong))
	select {
	case rec := <-done:
		t.Fatalf("mismatched result completed enrollment: %s", rec.Body.String())
	case <-time.After(80 * time.Millisecond):
	}
	p.send(fedEnrollmentMessage(t, p, proto.KindEnrollResult, request.ID, result))
	select {
	case rec = <-done:
		require.Equal(t, 200, rec.Code, rec.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("enrollment did not complete")
	}
	peer, e = db.GetFederationPeer(p.id.ID())
	require.NoError(t, e)
	require.Equal(t, "unrestricted", peer.TrustLevel)
	fedEventually(t, "post-trust master catalog refresh", func() bool { return len(p.envelopes(proto.KindCatalogReq)) > 0 })
	assignment, e := db.GetFederationNodeProfileAssignment(p.id.ID())
	require.NoError(t, e)
	require.Nil(t, assignment, "node never implicitly applies its default profile")
	rec = fedHuman(t, fh.f, http.MethodGet, "/v1/federation/enrollments", nil)
	require.NotContains(t, rec.Body.String(), strings.Split(bearer, ".")[3])
	require.Contains(t, rec.Body.String(), `"direction":"node"`)
}
func TestFederation_EnrollmentAPIsOperatorOnlyAndPinSelector(t *testing.T) {
	fh := newFedHarness(t)
	bearer := fedEnrollmentToken(t, fh)
	for _, path := range []string{"/v1/federation/enroll-tokens", "/v1/federation/enroll-tokens/id/revoke", "/v1/federation/enroll/preview", "/v1/federation/enroll"} {
		rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, path, map[string]any{"token": bearer}), "caller"))
		require.Equal(t, 403, rec.Code, path)
	}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/enroll/preview", map[string]any{"master": fh.peer.id.ID(), "token": bearer})
	require.Equal(t, 409, rec.Code, "own token cannot target another key")
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/enroll/preview", map[string]any{"master": fh.peer.id.ID(), "token": "tcle1.bad.secret"})
	require.Equal(t, 409, rec.Code)
	require.NotContains(t, rec.Body.String(), "bad.secret")
}
